package dataplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fixedDay = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// newZoneDNS is newTestDNS plus a pinned clock for SOA-serial assertions.
func newZoneDNS(t *testing.T, cs *controlServer) *DNS {
	t.Helper()
	d, _ := newTestDNS(t, cs)
	d.clock = func() time.Time { return fixedDay }
	return d
}

func zoneOf(t *testing.T, d *DNS, name string) map[string]any {
	t.Helper()
	zone, _ := d.cfg.Map("dns")[name].(map[string]any)
	if zone == nil {
		t.Fatalf("zone %q missing (dns = %v)", name, d.cfg.Map("dns"))
	}
	return zone
}

func recordsOf(t *testing.T, d *DNS, name string) []map[string]any {
	t.Helper()
	raw, _ := zoneOf(t, d, name)["records"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRecordAutoInitializesZone(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)

	d.Record(map[string]any{"name": "example.com", "type": "a", "value": "1.2.3.4"})

	zone := zoneOf(t, d, "example.com")
	soa, _ := zone["soa"].(map[string]any)
	wantSOA := map[string]any{
		"email": "hostmaster.example.com", "expire": float64(604800), "minimum": float64(3600),
		"primary": "ns1.example.com", "refresh": float64(3600), "retry": float64(600),
		// Serial starts at 2026070801 and the change bumps it to ...802.
		"serial": float64(2026070802), "ttl": float64(3600),
	}
	if !reflect.DeepEqual(soa, wantSOA) {
		t.Errorf("soa = %v, want %v", soa, wantSOA)
	}

	records := recordsOf(t, d, "example.com")
	if len(records) != 3 {
		t.Fatalf("records = %v, want 2 CAA + 1 A", records)
	}
	for i, want := range []string{"0 issue letsencrypt.org", "0 issuewild letsencrypt.org"} {
		if records[i]["type"] != "CAA" || records[i]["value"] != want || records[i]["ttl"] != float64(3600) {
			t.Errorf("CAA[%d] = %v", i, records[i])
		}
		if !uuidRe.MatchString(str(records[i]["id"])) {
			t.Errorf("CAA[%d] id = %v, not a v4 UUID", i, records[i]["id"])
		}
	}
	a := records[2]
	if a["type"] != "A" || a["value"] != "1.2.3.4" || a["ttl"] != float64(3600) || a["name"] != "example.com" {
		t.Errorf("A record = %v", a)
	}
	if _, ok := a["priority"]; ok {
		t.Errorf("priority key present without input: %v", a)
	}

	// Change epilogue: persisted to disk + zone push to the binary.
	payload := cs.nextConfig(t)
	zones, _ := payload["zones"].(map[string]any)
	if zones["example.com"] == nil {
		t.Errorf("sync payload zones = %v", payload["zones"])
	}
	raw, err := os.ReadFile(filepath.Join(d.cfg.BaseDir(), "config", "dns.json"))
	if err != nil || !strings.Contains(string(raw), "example.com") {
		t.Errorf("dns.json not persisted: %v %s", err, raw)
	}
}

func TestRecordZoneWalk(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.Record(map[string]any{"name": "example.com", "type": "A", "value": "1.1.1.1"})
	cs.nextConfig(t)

	d.Record(map[string]any{"name": "api.sub.example.com", "type": "CNAME", "value": "example.com"})
	cs.nextConfig(t)

	if dns := d.cfg.Map("dns"); len(dns) != 1 {
		t.Fatalf("zone walk created a new zone: %v", keys(dns))
	}
	records := recordsOf(t, d, "example.com")
	last := records[len(records)-1]
	if last["type"] != "CNAME" || last["name"] != "api.sub.example.com" {
		t.Errorf("walked record = %v", last)
	}
}

func TestRecordUniqueness(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)

	// A replaces per (type, name); MX accumulates; explicit unique overrides.
	d.Record(
		map[string]any{"name": "example.com", "type": "A", "value": "1.1.1.1"},
		map[string]any{"name": "example.com", "type": "A", "value": "2.2.2.2"},
		map[string]any{"name": "example.com", "type": "MX", "value": "mx1.example.com", "priority": float64(10)},
		map[string]any{"name": "example.com", "type": "MX", "value": "mx2.example.com", "priority": float64(20)},
	)
	byType := map[string][]map[string]any{}
	for _, r := range recordsOf(t, d, "example.com") {
		byType[str(r["type"])] = append(byType[str(r["type"])], r)
	}
	if len(byType["A"]) != 1 || byType["A"][0]["value"] != "2.2.2.2" {
		t.Errorf("A records = %v, want single replaced entry", byType["A"])
	}
	if len(byType["MX"]) != 2 || byType["MX"][0]["priority"] != float64(10) {
		t.Errorf("MX records = %v, want two with priorities", byType["MX"])
	}

	// unique:true forces replacement of a multi-value type.
	d.Record(map[string]any{"name": "example.com", "type": "MX", "value": "mx3.example.com", "unique": true})
	byType = map[string][]map[string]any{}
	for _, r := range recordsOf(t, d, "example.com") {
		byType[str(r["type"])] = append(byType[str(r["type"])], r)
	}
	if len(byType["MX"]) != 1 || byType["MX"][0]["value"] != "mx3.example.com" {
		t.Errorf("MX after unique override = %v", byType["MX"])
	}

	// Invalid type and missing type are skipped silently.
	before := len(recordsOf(t, d, "example.com"))
	d.Record(
		map[string]any{"name": "example.com", "type": "SRV", "value": "x"},
		map[string]any{"name": "example.com", "value": "no type"},
	)
	if got := len(recordsOf(t, d, "example.com")); got != before {
		t.Errorf("invalid records were added (%d → %d)", before, got)
	}
}

func TestRecordKeepsCustomTTLType(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.Record(map[string]any{"name": "example.com", "type": "TXT", "value": "v", "ttl": "600"})
	records := recordsOf(t, d, "example.com")
	if got := records[len(records)-1]["ttl"]; got != "600" {
		t.Errorf("ttl = %v (%T), want original string kept (Node: obj.ttl || 3600)", got, got)
	}
}

func TestRecordRejectsUnsafeKeys(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.Record(map[string]any{"name": "__proto__", "type": "A", "value": "1.1.1.1"})
	d.Record(map[string]any{"name": "constructor", "type": "A", "value": "1.1.1.1"})
	if dns := d.cfg.Map("dns"); len(dns) != 0 {
		t.Errorf("unsafe zone keys accepted: %v", keys(dns))
	}
	cs.expectNoConfig(t, 100*time.Millisecond)
}

func TestSOASerialSameDayIncrementsAcrossDays(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.Record(map[string]any{"name": "example.com", "type": "A", "value": "1.1.1.1"})
	soa, _ := zoneOf(t, d, "example.com")["soa"].(map[string]any)
	if soa["serial"] != float64(2026070802) {
		t.Errorf("serial = %v, want 2026070802 (init 01 + change bump)", soa["serial"])
	}

	d.Record(map[string]any{"name": "example.com", "type": "TXT", "value": "x"})
	soa, _ = zoneOf(t, d, "example.com")["soa"].(map[string]any)
	if soa["serial"] != float64(2026070803) {
		t.Errorf("same-day serial = %v, want 2026070803", soa["serial"])
	}

	d.clock = func() time.Time { return fixedDay.AddDate(0, 0, 1) }
	d.Record(map[string]any{"name": "example.com", "type": "TXT", "value": "y"})
	soa, _ = zoneOf(t, d, "example.com")["soa"].(map[string]any)
	if soa["serial"] != float64(2026070901) {
		t.Errorf("next-day serial = %v, want reset to 2026070901", soa["serial"])
	}
}

func TestDeleteRecords(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.cfg.Set("dns", map[string]any{
		"example.com": map[string]any{
			"soa": map[string]any{"serial": float64(2020010101)},
			"records": []any{
				map[string]any{"id": "1", "name": "example.com", "type": "A", "value": "1.1.1.1"},
				map[string]any{"id": "2", "name": "example.com", "type": "A", "value": "2.2.2.2"},
				map[string]any{"id": "3", "name": "www.example.com", "type": "A", "value": "1.1.1.1"},
				map[string]any{"id": "4", "name": "example.com", "type": "TXT", "value": "keep"},
			},
		},
	})

	// Value filter: only the matching A record goes; zone-walk resolves the
	// www name into the example.com zone.
	d.Delete(map[string]any{"name": "example.com", "type": "a", "value": "1.1.1.1"})
	if got := len(recordsOf(t, d, "example.com")); got != 3 {
		t.Fatalf("records after value delete = %d, want 3", got)
	}
	soa, _ := zoneOf(t, d, "example.com")["soa"].(map[string]any)
	if soa["serial"] != float64(2026070801) {
		t.Errorf("serial after delete = %v, want reset to today01", soa["serial"])
	}

	// No value: every (type, name) match goes.
	d.Delete(map[string]any{"name": "www.example.com", "type": "A"})
	records := recordsOf(t, d, "example.com")
	if len(records) != 2 {
		t.Fatalf("records = %v", records)
	}

	// Non-matching value leaves everything (and skips sync/persist).
	d.Delete(map[string]any{"name": "example.com", "type": "TXT", "value": "other"})
	if got := len(recordsOf(t, d, "example.com")); got != 2 {
		t.Errorf("non-match delete changed records: %d", got)
	}
}

func TestDeleteResolvesDynamicValue(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.primary = "9.9.9.9"
	d.cfg.Set("dns", map[string]any{
		"example.com": map[string]any{
			"soa": map[string]any{"serial": float64(2020010101)},
			"records": []any{
				map[string]any{"id": "1", "name": "example.com", "type": "A"}, // dynamic
			},
		},
	})

	d.Delete(map[string]any{"name": "example.com", "type": "A", "value": "8.8.8.8"})
	if got := len(recordsOf(t, d, "example.com")); got != 1 {
		t.Fatal("dynamic record deleted despite value mismatch")
	}
	d.Delete(map[string]any{"name": "example.com", "type": "A", "value": "9.9.9.9"})
	if got := len(recordsOf(t, d, "example.com")); got != 0 {
		t.Error("dynamic record kept despite resolved-value match")
	}
}

func TestListResolvesAndFilters(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	d.primary = "5.5.5.5"
	d.ipv6 = []IPEntry{{Address: "fd00::1", Public: false}, {Address: "2001:db8::1", Public: true}}
	d.cfg.Set("dns", map[string]any{
		"example.com": map[string]any{
			"soa": map[string]any{"serial": float64(1)},
			"records": []any{
				map[string]any{"id": "1", "name": "example.com", "type": "A"},
				map[string]any{"id": "2", "name": "example.com", "type": "AAAA"},
				map[string]any{"id": "3", "name": "example.com", "type": "A", "value": "7.7.7.7"},
			},
		},
	})

	res := d.List("example.com")
	if !res.Status || !res.HasData {
		t.Fatalf("List = %+v", res)
	}
	records, _ := res.Data.([]any)
	if len(records) != 3 {
		t.Fatalf("records = %v", res.Data)
	}
	r0, _ := records[0].(map[string]any)
	r1, _ := records[1].(map[string]any)
	r2, _ := records[2].(map[string]any)
	if r0["value"] != "5.5.5.5" {
		t.Errorf("dynamic A = %v, want primary", r0["value"])
	}
	if r1["value"] != "2001:db8::1" {
		t.Errorf("dynamic AAAA = %v, want first public IPv6", r1["value"])
	}
	if r2["value"] != "7.7.7.7" {
		t.Errorf("static A = %v, must stay", r2["value"])
	}

	// The copy must not leak resolved values into config.
	orig := recordsOf(t, d, "example.com")
	if _, ok := orig[0]["value"]; ok {
		t.Error("List mutated the live config records")
	}

	// Filter normalization: trim + lowercase; unknown domain → message.
	if res := d.List("  EXAMPLE.COM "); !res.Status {
		t.Errorf("normalized filter failed: %+v", res)
	}
	res = d.List("missing.test")
	if res.Status || res.Message != "No DNS records found for domain missing.test." {
		t.Errorf("missing domain = %+v", res)
	}

	// Non-string / "undefined" / "null" args list everything (the full-map
	// payload is pre-serialized with Node's zone key order — see zonesJSON).
	for _, arg := range []any{nil, float64(4), "undefined", "NULL"} {
		res := d.List(arg)
		raw, _ := res.Data.(json.RawMessage)
		var zones map[string]any
		json.Unmarshal(raw, &zones)
		if !res.Status || zones["example.com"] == nil {
			t.Errorf("List(%v) = %+v, want full zone map", arg, res)
		}
	}
	// soa must precede records inside each zone (Node's creation order).
	if full, _ := d.List(nil).Data.(json.RawMessage); !regexp.MustCompile(`"soa":.*"records":`).Match(full) {
		t.Errorf("zone key order not soa-first: %s", full)
	}
}

func TestListEmptyConfig(t *testing.T) {
	cs := newControlServer(t)
	d := newZoneDNS(t, cs)
	res := d.List(nil)
	raw, _ := json.Marshal(res.Data)
	if !res.Status || string(raw) != "{}" {
		t.Errorf("List on empty config = %+v (%s)", res, raw)
	}
}
