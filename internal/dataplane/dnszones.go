package dataplane

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"odac/internal/api"
	"odac/internal/lang"
)

// DNS zone/record management — the port of DNS.js record()/delete()/list()
// and #updateSOASerial, per contracts/dns-control.md "Zone/record management
// semantics". The Go binary only serves the last pushed snapshot; all CRUD
// stays here in the orchestrator. Every config.dns mutation runs inside
// cfg.Mutate (see config.Store) and is followed by ForceSave + an async
// SyncConfig, matching Node's force() + un-awaited syncConfig().

var __ = lang.T

// dnsRecordTypes are the accepted record types; multi-value types allow
// several records per (type, name) — the rest replace unless overridden by
// an explicit "unique" flag.
var (
	dnsRecordTypes     = map[string]bool{"A": true, "AAAA": true, "CAA": true, "CNAME": true, "MX": true, "NS": true, "TXT": true}
	dnsMultiValueTypes = map[string]bool{"CAA": true, "MX": true, "NS": true, "TXT": true}
)

// Record ports DNS.record(...args): add records ({name, type, value,
// priority?, ttl?, unique?}), auto-initializing zones with SOA + default
// CAA pair, bumping SOA serials, persisting and syncing on change.
func (d *DNS) Record(args ...map[string]any) {
	changed := map[string]bool{}

	d.cfg.Mutate(func() {
		dnsCfg, _ := d.cfg.Get("dns").(map[string]any)
		if dnsCfg == nil {
			dnsCfg = map[string]any{}
			d.cfg.Set("dns", dnsCfg)
		}

		for _, obj := range args {
			if obj == nil {
				continue
			}
			domain := str(obj["name"])

			// Zone matching: walk labels upward to find an existing zone;
			// none found → the full name becomes a new zone.
			zoneDomain := domain
			found := false
			for temp := domain; strings.Contains(temp, "."); temp = temp[strings.Index(temp, ".")+1:] {
				if isSafeKey(temp) && hasKey(dnsCfg, temp) {
					zoneDomain = temp
					found = true
					break
				}
			}
			if !found {
				zoneDomain = domain
			}
			if !isSafeKey(zoneDomain) {
				continue
			}

			if !hasKey(dnsCfg, zoneDomain) {
				dnsCfg[zoneDomain] = d.newZone(zoneDomain)
			}
			zone, _ := dnsCfg[zoneDomain].(map[string]any)
			if zone == nil {
				continue
			}
			if !truthy(obj["type"]) {
				continue
			}

			typ := strings.ToUpper(str(obj["type"]))
			if !dnsRecordTypes[typ] {
				continue
			}

			isUnique := !dnsMultiValueTypes[typ]
			if u, ok := obj["unique"]; ok {
				isUnique = truthy(u)
			}

			records, _ := zone["records"].([]any)
			if isUnique {
				kept := make([]any, 0, len(records))
				for _, r := range records {
					rm, _ := r.(map[string]any)
					if rm != nil && jsEqual(rm["type"], typ) && jsEqual(rm["name"], obj["name"]) {
						continue
					}
					kept = append(kept, r)
				}
				records = kept
			}

			rec := map[string]any{
				"id":   newUUID(),
				"name": obj["name"],
				"ttl":  float64(3600),
				"type": typ,
			}
			if truthy(obj["ttl"]) { // Node: obj.ttl || 3600, original value kept
				rec["ttl"] = obj["ttl"]
			}
			// priority/value stay absent when not supplied — Node sets them
			// to undefined, which its JSON persistence omits the same way.
			if v, ok := obj["priority"]; ok {
				rec["priority"] = v
			}
			if v, ok := obj["value"]; ok {
				rec["value"] = v
			}
			zone["records"] = append(records, rec)

			changed[zoneDomain] = true
		}

		for domain := range changed {
			d.updateSOASerial(dnsCfg, domain)
		}
		if len(changed) > 0 {
			d.cfg.Touch("dns")
		}
	})

	if len(changed) > 0 {
		d.persistAndSync()
	}
}

// Delete ports DNS.delete(...args): remove records matching (type, name)
// and optionally value; empty-value dynamic A/AAAA records compare against
// their resolved value, mirroring list().
func (d *DNS) Delete(args ...map[string]any) {
	changed := map[string]bool{}

	d.cfg.Mutate(func() {
		dnsCfg, _ := d.cfg.Get("dns").(map[string]any)
		if dnsCfg == nil {
			return
		}
		_, v6, primary := d.IPInfo()

		for _, obj := range args {
			if obj == nil {
				continue
			}
			domain := str(obj["name"])
			if !isSafeKey(domain) {
				continue
			}
			for strings.Contains(domain, ".") && (!hasKey(dnsCfg, domain) || !isSafeKey(domain)) {
				domain = domain[strings.Index(domain, ".")+1:]
			}
			if !isSafeKey(domain) || !hasKey(dnsCfg, domain) {
				continue
			}
			if !truthy(obj["type"]) {
				continue
			}

			typ := strings.ToUpper(str(obj["type"]))
			zone, _ := dnsCfg[domain].(map[string]any)
			if zone == nil {
				continue
			}
			records, _ := zone["records"].([]any)
			kept := make([]any, 0, len(records))
			for _, r := range records {
				rm, _ := r.(map[string]any)
				if rm == nil || !jsEqual(rm["type"], typ) || !jsEqual(rm["name"], obj["name"]) {
					kept = append(kept, r)
					continue
				}
				recordValue := rm["value"]
				if !truthy(recordValue) && rm["type"] == "A" {
					recordValue = orDefault(primary, "127.0.0.1")
				} else if !truthy(recordValue) && rm["type"] == "AAAA" {
					if v := firstIPv6(v6); v != "" {
						recordValue = v
					}
				}
				if truthy(obj["value"]) && !jsEqual(recordValue, obj["value"]) {
					kept = append(kept, r)
					continue
				}
				// matched → dropped
			}
			if len(kept) != len(records) {
				zone["records"] = kept
				changed[domain] = true
			}
		}

		for domain := range changed {
			d.updateSOASerial(dnsCfg, domain)
		}
		if len(changed) > 0 {
			d.cfg.Touch("dns")
		}
	})

	if len(changed) > 0 {
		d.persistAndSync()
	}
}

// List ports DNS.list(domain): a deep copy of all zones with dynamic
// (empty-value) A/AAAA records resolved to the detected addresses,
// optionally filtered to one domain's records.
func (d *DNS) List(domainArg any) api.Result {
	domain := ""
	if s, ok := domainArg.(string); ok {
		norm := strings.ToLower(strings.TrimSpace(s))
		if norm != "undefined" && norm != "null" {
			domain = norm
		}
	}

	zones := map[string]any{}
	d.cfg.View(func() {
		if src := d.cfg.Get("dns"); src != nil {
			if raw, err := json.Marshal(src); err == nil {
				json.Unmarshal(raw, &zones) // JSON.parse(JSON.stringify(...))
			}
		}
	})

	_, v6, primary := d.IPInfo()
	ipv6 := firstIPv6(v6)
	for _, z := range zones {
		zm, _ := z.(map[string]any)
		if zm == nil {
			continue
		}
		records, ok := zm["records"].([]any)
		if !ok {
			continue
		}
		for _, r := range records {
			rm, _ := r.(map[string]any)
			if rm == nil {
				continue
			}
			if rm["type"] == "A" && !truthy(rm["value"]) {
				rm["value"] = orDefault(primary, "127.0.0.1")
			} else if rm["type"] == "AAAA" && !truthy(rm["value"]) && ipv6 != "" {
				rm["value"] = ipv6
			}
		}
	}

	if domain != "" {
		if z := zones[domain]; truthy(z) {
			zm, _ := z.(map[string]any)
			var records any
			if zm != nil {
				records = zm["records"]
			}
			return api.Res(true, orList(records))
		}
		return api.Res(false, __("No DNS records found for domain %s.", domain))
	}
	return api.Res(true, zonesJSON(zones))
}

// zonesJSON serializes the full zone map with Node's key order inside each
// zone: soa before records (DNS.js creates zones as {soa, records}), other
// keys after, alphabetically. Node's own inner objects (soa fields, record
// fields) are alphabetical already, so this makes the dns.list payload
// byte-match Node on Node-written data. Zone NAMES are emitted sorted — a
// recorded deviation (Node preserves file insertion order; the config store
// loads into Go maps, which have none).
func zonesJSON(zones map[string]any) json.RawMessage {
	out := []byte{'{'}
	for _, name := range sortedKeys(zones) {
		if len(out) > 1 {
			out = append(out, ',')
		}
		key, _ := json.Marshal(name)
		out = append(append(out, key...), ':')

		zm, ok := zones[name].(map[string]any)
		if !ok {
			raw, err := json.Marshal(zones[name])
			if err != nil {
				raw = []byte("null")
			}
			out = append(out, raw...)
			continue
		}
		out = append(out, '{')
		fields := 0
		appendZoneField := func(field string) {
			v, ok := zm[field]
			if !ok {
				return
			}
			if fields > 0 {
				out = append(out, ',')
			}
			fields++
			fkey, _ := json.Marshal(field)
			raw, err := json.Marshal(v)
			if err != nil {
				raw = []byte("null")
			}
			out = append(append(append(out, fkey...), ':'), raw...)
		}
		appendZoneField("soa")
		appendZoneField("records")
		for _, field := range sortedKeys(zm) {
			if field != "soa" && field != "records" {
				appendZoneField(field)
			}
		}
		out = append(out, '}')
	}
	return append(out, '}')
}

// newZone auto-initializes a zone: SOA with serial YYYYMMDD01 (UTC, like
// Node's toISOString-derived date) and the default Let's Encrypt CAA pair.
func (d *DNS) newZone(zoneDomain string) map[string]any {
	dateStr := d.now().UTC().Format("20060102")
	serial, _ := strconv.ParseFloat(dateStr+"01", 64)
	return map[string]any{
		"soa": map[string]any{
			"email":   "hostmaster." + zoneDomain,
			"expire":  float64(604800),
			"minimum": float64(3600),
			"primary": "ns1." + zoneDomain,
			"refresh": float64(3600),
			"retry":   float64(600),
			"serial":  serial,
			"ttl":     float64(3600),
		},
		"records": []any{
			map[string]any{"id": newUUID(), "name": zoneDomain, "ttl": float64(3600), "type": "CAA", "value": "0 issue letsencrypt.org"},
			map[string]any{"id": newUUID(), "name": zoneDomain, "ttl": float64(3600), "type": "CAA", "value": "0 issuewild letsencrypt.org"},
		},
	}
}

// updateSOASerial ports #updateSOASerial: same-day changes increment the
// serial, otherwise it resets to YYYYMMDD01. Caller holds cfg.Mutate.
func (d *DNS) updateSOASerial(dnsCfg map[string]any, domain string) {
	zone, _ := dnsCfg[domain].(map[string]any)
	if zone == nil {
		return
	}
	soa, _ := zone["soa"].(map[string]any)
	if soa == nil {
		return
	}
	dateStr := d.now().UTC().Format("20060102")
	serialStr := jsNumberString(soa["serial"])
	if len(serialStr) >= 8 && serialStr[:8] == dateStr {
		if f, ok := soa["serial"].(float64); ok {
			soa["serial"] = f + 1
		}
	} else {
		serial, _ := strconv.ParseFloat(dateStr+"01", 64)
		soa["serial"] = serial
	}
}

// persistAndSync is the change epilogue: Config.force() then the un-awaited
// syncConfig() — persistence is synchronous, the push runs in the background.
func (d *DNS) persistAndSync() {
	if err := d.cfg.ForceSave(); err != nil {
		d.log.Error(fmt.Sprintf("Failed to persist config: %s", err))
	}
	go d.SyncConfig()
}

// isSafeKey ports #isSafe: the JS prototype-pollution guard, kept for
// wire-compatible rejections.
func isSafeKey(key string) bool {
	switch strings.ToLower(key) {
	case "__proto__", "constructor", "prototype":
		return false
	}
	return true
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// firstIPv6 ports ips.ipv6.find(i => i.public)?.address || ips.ipv6[0]?.address.
func firstIPv6(v6 []IPEntry) string {
	for _, e := range v6 {
		if e.Public {
			return e.Address
		}
	}
	if len(v6) > 0 {
		return v6[0].Address
	}
	return ""
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// jsNumberString renders a decoded-JSON number like Number.prototype
// .toString (integral floats without a decimal point); non-numbers render
// via str.
func jsNumberString(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return str(v)
}

// newUUID generates a random (version 4) UUID like Node's crypto.randomUUID.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable, like randomUUID
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// now is injected for SOA-serial tests.
func (d *DNS) now() time.Time {
	if d.clock != nil {
		return d.clock()
	}
	return time.Now()
}
