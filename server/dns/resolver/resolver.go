// Package resolver implements the authoritative DNS query processing engine.
// It handles zone resolution, record lookup, and RFC-compliant response
// generation for all supported record types. Designed for O(1) zone lookups
// and minimal allocations per query using sync.Pool and pre-built zone maps.
package resolver

import (
	"log"
	"strings"
	"sync"

	"github.com/miekg/dns"

	"odac-dns/config"
)

// Resolver is the core DNS query handler. It maintains an in-memory zone
// database that is atomically swapped on config updates from Node.js.
type Resolver struct {
	ips   config.IPConfig
	mu    sync.RWMutex
	zones map[string]*zoneData // domain -> zone (read-heavy, write-rare)
}

// zoneData is the pre-processed zone optimized for query-time performance.
// Records are indexed by (lowercase name, type) for O(1) lookup per query.
type zoneData struct {
	domain  string
	records map[recordKey][]config.Record // (name, type) -> records
	soa     config.SOARecord
}

// recordKey is the composite key for record indexing.
type recordKey struct {
	name     string
	rtype    string
}

// NewResolver creates a new DNS resolver with empty zone data.
func NewResolver() *Resolver {
	return &Resolver{
		zones: make(map[string]*zoneData),
	}
}

// UpdateConfig atomically replaces the entire zone database.
// Called when Node.js sends a POST /config with fresh zone data.
// Builds optimized lookup indices for O(1) per-query performance.
func (r *Resolver) UpdateConfig(cfg config.Config) {
	newZones := make(map[string]*zoneData, len(cfg.Zones))

	for domain, zone := range cfg.Zones {
		zd := &zoneData{
			domain:  strings.ToLower(domain),
			records: make(map[recordKey][]config.Record),
			soa:     zone.SOA,
		}

		// Build record index: group by (lowercase name, uppercase type)
		for _, rec := range zone.Records {
			key := recordKey{
				name:  strings.ToLower(rec.Name),
				rtype: strings.ToUpper(rec.Type),
			}
			zd.records[key] = append(zd.records[key], rec)
		}

		newZones[zd.domain] = zd
	}

	r.mu.Lock()
	r.zones = newZones
	r.ips = cfg.IPs
	r.mu.Unlock()

	log.Printf("[DNS] Config updated: %d zones loaded", len(newZones))
}

// ServeDNS implements the miekg/dns.Handler interface.
// This is the hot path — every DNS query hits this method.
func (r *Resolver) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true
	msg.Compress = true

	// Validate request structure
	if len(req.Question) == 0 {
		msg.Rcode = dns.RcodeFormatError
		w.WriteMsg(msg)
		return
	}

	q := req.Question[0]
	qName := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	qType := q.Qtype

	// Resolve zone: exact match first, then walk up the domain tree
	r.mu.RLock()
	zone, domain := r.resolveZone(qName)
	ips := r.ips
	r.mu.RUnlock()

	if zone == nil {
		// Not authoritative for this domain → NXDOMAIN
		msg.Rcode = dns.RcodeNameError
		w.WriteMsg(msg)
		return
	}

	// Check if the queried name exists in the zone at all (any type)
	if !r.nameExists(zone, qName, domain) {
		// Name does not exist → NXDOMAIN with SOA in authority (RFC 2308)
		msg.Rcode = dns.RcodeNameError
		r.addSOAAuthority(msg, zone, domain)
		w.WriteMsg(msg)
		return
	}

	// RFC 8482: Refuse ANY queries to prevent amplification attacks.
	// Return only SOA to minimize response size.
	if qType == dns.TypeANY {
		r.processSOA(msg, zone, domain)
		w.WriteMsg(msg)
		return
	}

	// RFC 1034: CNAME takes precedence. If a CNAME exists for this name,
	// return only the CNAME regardless of the query type.
	cnameKey := recordKey{name: qName, rtype: "CNAME"}
	if cnameRecords, ok := zone.records[cnameKey]; ok && len(cnameRecords) > 0 {
		r.processCNAME(msg, cnameRecords, qName)
		w.WriteMsg(msg)
		return
	}

	// Dispatch to the appropriate record type handler
	fqdn := dns.Fqdn(qName)

	switch qType {
	case dns.TypeA:
		r.processA(msg, zone, qName, fqdn, ips)
	case dns.TypeAAAA:
		r.processAAAA(msg, zone, qName, fqdn, ips)
	case dns.TypeCNAME:
		r.processCNAME(msg, zone.records[cnameKey], qName)
	case dns.TypeMX:
		r.processMX(msg, zone, qName, fqdn)
	case dns.TypeTXT:
		r.processTXT(msg, zone, qName, fqdn)
	case dns.TypeNS:
		r.processNS(msg, zone, qName, fqdn, domain)
	case dns.TypeSOA:
		r.processSOA(msg, zone, domain)
	case dns.TypeCAA:
		r.processCAA(msg, zone, qName, fqdn)
	default:
		// Unknown type → NODATA (empty answer, no error)
	}

	w.WriteMsg(msg)
}

// resolveZone finds the authoritative zone for a query name.
// Walks up the domain tree: sub.example.com → example.com → com
// Returns nil if no zone is found.
func (r *Resolver) resolveZone(qName string) (*zoneData, string) {
	domain := qName
	for {
		if z, ok := r.zones[domain]; ok {
			return z, domain
		}
		idx := strings.IndexByte(domain, '.')
		if idx < 0 {
			break
		}
		domain = domain[idx+1:]
	}
	return nil, ""
}

// nameExists checks if a given name has any records in the zone.
// The zone apex (domain itself) always "exists" because it has SOA.
func (r *Resolver) nameExists(zone *zoneData, qName, domain string) bool {
	if qName == domain {
		return true
	}
	// Scan all record keys for a name match — O(k) where k = unique (name,type) pairs
	for key := range zone.records {
		if key.name == qName {
			return true
		}
	}
	return false
}

// addSOAAuthority appends the zone's SOA record to the Authority section.
// Used for NXDOMAIN responses per RFC 2308 (negative caching).
func (r *Resolver) addSOAAuthority(msg *dns.Msg, zone *zoneData, domain string) {
	soa := zone.soa
	ttl := uint32(soa.TTL)
	if ttl == 0 {
		ttl = 3600
	}
	minimum := uint32(soa.Minimum)
	if minimum == 0 {
		minimum = 3600
	}

	msg.Ns = append(msg.Ns, &dns.SOA{
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
