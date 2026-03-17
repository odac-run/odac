package resolver

// DNS record type handlers for A, AAAA, CNAME, MX, TXT, NS, SOA, and CAA.
// Each handler follows the same pattern: lookup indexed records, filter by
// query name, and append miekg/dns RR structs to msg.Answer or msg.Ns.
//
// Design rationale: Handlers are methods on Resolver (not standalone functions)
// to access the shared IP configuration for PTR-aware address resolution.

import (
	"net"
	"strings"

	"github.com/miekg/dns"

	"odac-dns/config"
)

// processA handles A record queries. Replaces loopback (127.0.0.1) with
// the server's detected public IPv4 via PTR-aware resolution.
func (r *Resolver) processA(msg *dns.Msg, zone *zoneData, qName, fqdn string, ips config.IPConfig) {
	key := recordKey{name: qName, rtype: "A"}
	records, ok := zone.records[key]
	if !ok {
		return
	}

	for _, rec := range records {
		address := rec.Value

		// Replace loopback with the server's public IP (PTR-aware)
		if address == "127.0.0.1" || address == "" {
			resolved := resolveIPByPTR(qName, "ipv4", ips)
			if resolved != "" {
				address = resolved
			}
		}

		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil {
			continue // Skip invalid IPv4 addresses
		}

		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}

		msg.Answer = append(msg.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: fqdn, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
			A:   ip.To4(),
		})
	}
}

// processAAAA handles AAAA record queries with PTR-aware IPv6 resolution.
func (r *Resolver) processAAAA(msg *dns.Msg, zone *zoneData, qName, fqdn string, ips config.IPConfig) {
	key := recordKey{name: qName, rtype: "AAAA"}
	records, ok := zone.records[key]
	if !ok {
		return
	}

	for _, rec := range records {
		address := rec.Value

		// Resolve to server's public IPv6 if value is a placeholder
		if address == "::1" || address == "" {
			resolved := resolveIPByPTR(qName, "ipv6", ips)
			if resolved != "" {
				address = resolved
			}
		}

		ip := net.ParseIP(address)
		if ip == nil || ip.To4() != nil {
			continue // Skip if not a valid IPv6 address
		}

		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}

		msg.Answer = append(msg.Answer, &dns.AAAA{
			Hdr:  dns.RR_Header{Name: fqdn, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl},
			AAAA: ip,
		})
	}
}

// processCNAME handles CNAME record queries.
// Per RFC 1034, if a CNAME exists for a name, it is the only record returned.
func (r *Resolver) processCNAME(msg *dns.Msg, records []config.Record, qName string) {
	fqdn := dns.Fqdn(qName)
	for _, rec := range records {
		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}
		msg.Answer = append(msg.Answer, &dns.CNAME{
			Hdr:    dns.RR_Header{Name: fqdn, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl},
			Target: dns.Fqdn(rec.Value),
		})
	}
}

// processCAA handles CAA record queries. If no explicit CAA records exist,
// injects a default Let's Encrypt issuance policy to enable automatic ACME.
func (r *Resolver) processCAA(msg *dns.Msg, zone *zoneData, qName, fqdn string) {
	key := recordKey{name: qName, rtype: "CAA"}
	records := zone.records[key]

	if len(records) > 0 {
		for _, rec := range records {
			caa := parseCAA(rec.Value, fqdn, rec.TTL)
			if caa != nil {
				msg.Answer = append(msg.Answer, caa)
			}
		}
		return
	}

	// Default: Allow Let's Encrypt issuance (same behavior as Node.js DNS.js)
	r.addDefaultCAARecords(msg, fqdn)
}

// processMX handles MX record queries with priority support.
func (r *Resolver) processMX(msg *dns.Msg, zone *zoneData, qName, fqdn string) {
	key := recordKey{name: qName, rtype: "MX"}
	records, ok := zone.records[key]
	if !ok {
		return
	}

	for _, rec := range records {
		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}

		priority := uint16(rec.Priority)
		if priority == 0 {
			priority = 10
		}

		msg.Answer = append(msg.Answer, &dns.MX{
			Hdr:        dns.RR_Header{Name: fqdn, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: ttl},
			Mx:         dns.Fqdn(rec.Value),
			Preference: priority,
		})
	}
}

// processNS handles NS record queries. NS records are placed in the
// Authority section with the Authoritative flag set per RFC 1035.
func (r *Resolver) processNS(msg *dns.Msg, zone *zoneData, qName, fqdn, domain string) {
	key := recordKey{name: qName, rtype: "NS"}
	records, ok := zone.records[key]
	if !ok {
		return
	}

	msg.Authoritative = true

	for _, rec := range records {
		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}

		msg.Ns = append(msg.Ns, &dns.NS{
			Hdr: dns.RR_Header{Name: fqdn, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: ttl},
			Ns:  dns.Fqdn(rec.Value),
		})
	}
}

// processSOA handles SOA record queries. Returns the zone's SOA from
// the pre-built zone data.
func (r *Resolver) processSOA(msg *dns.Msg, zone *zoneData, domain string) {
	soa := zone.soa
	ttl := uint32(soa.TTL)
	if ttl == 0 {
		ttl = 3600
	}
	minimum := uint32(soa.Minimum)
	if minimum == 0 {
		minimum = 3600
	}

	msg.Answer = append(msg.Answer, &dns.SOA{
		Hdr:     dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Expire:  uint32(soa.Expire),
		Minttl:  minimum,
		Mbox:    dns.Fqdn(soa.Email),
		Ns:      dns.Fqdn(soa.Primary),
		Refresh: uint32(soa.Refresh),
		Retry:   uint32(soa.Retry),
		Serial:  uint32(soa.Serial),
	})
}

// processTXT handles TXT record queries. Values exceeding the 255-byte DNS
// string limit are automatically split into multiple character-strings per
// RFC 7208 / RFC 4408. This is essential for 2048-bit DKIM public keys.
func (r *Resolver) processTXT(msg *dns.Msg, zone *zoneData, qName, fqdn string) {
	key := recordKey{name: qName, rtype: "TXT"}
	records, ok := zone.records[key]
	if !ok {
		return
	}

	for _, rec := range records {
		ttl := uint32(rec.TTL)
		if ttl == 0 {
			ttl = 3600
		}

		// Split value into 255-byte chunks per RFC 7208
		txt := splitTXT(rec.Value)

		msg.Answer = append(msg.Answer, &dns.TXT{
			Hdr: dns.RR_Header{Name: fqdn, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl},
			Txt: txt,
		})
	}
}

// splitTXT splits a TXT value into 255-byte character-strings per RFC 4408.
// A single DNS TXT rdata can contain multiple strings, each max 255 bytes.
// This is critical for DKIM keys which routinely exceed 255 characters.
func splitTXT(value string) []string {
	if len(value) <= 255 {
		return []string{value}
	}

	var chunks []string
	for len(value) > 0 {
		end := 255
		if end > len(value) {
			end = len(value)
		}
		chunks = append(chunks, value[:end])
		value = value[end:]
	}
	return chunks
}

// parseCAA parses a CAA record value string in the format "flags tag value"
// (e.g., "0 issue letsencrypt.org") into a dns.CAA resource record.
func parseCAA(value, fqdn string, ttl int) *dns.CAA {
	parts := strings.SplitN(value, " ", 3)
	if len(parts) < 3 {
		return nil
	}

	flags := uint8(0)
	if parts[0] == "128" {
		flags = 128 // Critical flag
	}

	t := uint32(ttl)
	if t == 0 {
		t = 3600
	}

	return &dns.CAA{
		Flag:  flags,
		Hdr:   dns.RR_Header{Name: fqdn, Rrtype: dns.TypeCAA, Class: dns.ClassINET, Ttl: t},
		Tag:   parts[1],
		Value: parts[2],
	}
}

// addDefaultCAARecords injects Let's Encrypt default CAA issuance policy.
// Mirrors the behavior of Node.js DNS.js #addDefaultCAARecords().
func (r *Resolver) addDefaultCAARecords(msg *dns.Msg, fqdn string) {
	msg.Answer = append(msg.Answer,
		&dns.CAA{
			Flag:  0,
			Hdr:   dns.RR_Header{Name: fqdn, Rrtype: dns.TypeCAA, Class: dns.ClassINET, Ttl: 3600},
			Tag:   "issue",
			Value: "letsencrypt.org",
		},
		&dns.CAA{
			Flag:  0,
			Hdr:   dns.RR_Header{Name: fqdn, Rrtype: dns.TypeCAA, Class: dns.ClassINET, Ttl: 3600},
			Tag:   "issuewild",
			Value: "letsencrypt.org",
		},
	)
}

// resolveIPByPTR resolves an IP address by matching PTR records to the
// given domain. Priority: exact PTR match → subdomain match → first public IP.
// Mirrors Node.js DNS.js #resolveIPByPTR() behavior.
func resolveIPByPTR(domain, ipType string, ips config.IPConfig) string {
	var ipList []config.IPEntry
	if ipType == "ipv6" {
		ipList = ips.IPv6
	} else {
		ipList = ips.IPv4
	}

	// Find first public IP as default
	var defaultIP string
	for _, entry := range ipList {
		if entry.Public {
			defaultIP = entry.Address
			break
		}
	}

	// Fallback to primary IP for IPv4
	if defaultIP == "" && ipType == "ipv4" {
		defaultIP = ips.Primary
	}

	// Try PTR-based resolution
	domainLower := strings.ToLower(domain)
	for _, entry := range ipList {
		if entry.PTR == "" {
			continue
		}
		ptrLower := strings.ToLower(entry.PTR)

		// Exact match
		if ptrLower == domainLower {
			return entry.Address
		}

		// Subdomain match (PTR is subdomain of query, or query is subdomain of PTR's root)
		if strings.HasSuffix(domainLower, "."+ptrLower) || strings.HasSuffix(ptrLower, "."+domainLower) {
			return entry.Address
		}
	}

	return defaultIP
}
