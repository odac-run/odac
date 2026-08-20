package domains

// Port of test/server/Domain.test.js plus extra spec cases for behavior the
// jest suite left uncovered (www-strip, wildcard/IP handling, SPF assembly,
// protocol prefixes, list payload shape).

import (
	"encoding/json"
	"strings"
	"testing"
)

func msgOf(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	return s
}

// ─── add() ──────────────────────────────────────────────────────────────

func TestAddValidDomain(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("example.com", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}

	rec := fx.domain("example.com")
	if rec == nil || rec["appId"] != "myapp" {
		t.Fatalf("record = %v", rec)
	}
	if created, _ := rec["created"].(float64); created <= 0 {
		t.Fatalf("created = %v", rec["created"])
	}
	subs, _ := rec["subdomain"].([]any)
	if len(subs) != 2 || subs[0] != "www" || subs[1] != "mail" {
		t.Fatalf("subdomain = %v", rec["subdomain"])
	}
	if _, ok := rec["cert"].(map[string]any); !ok {
		t.Fatalf("cert tracking not initialized: %v", rec["cert"])
	}

	// DNS record set: A, AAAA, CNAME(www), A(mail), MX, DMARC TXT, SPF TXT.
	types := map[string]int{}
	var dmarc, spf map[string]any
	for _, rec := range fx.dns.recorded() {
		types[rec["type"].(string)]++
		if rec["type"] == "TXT" && strings.HasPrefix(rec["name"].(string), "_dmarc") {
			dmarc = rec
		} else if rec["type"] == "TXT" {
			spf = rec
		}
	}
	if types["A"] != 2 || types["AAAA"] != 1 || types["CNAME"] != 1 || types["MX"] != 1 || types["TXT"] != 2 {
		t.Fatalf("record types = %v", types)
	}
	if dmarc == nil || dmarc["value"] != "v=DMARC1; p=reject; rua=mailto:postmaster@example.com" {
		t.Fatalf("dmarc = %v", dmarc)
	}
	// SPF includes the detected public IPs.
	if spf == nil || spf["value"] != "v=spf1 a mx ip4:1.2.3.4 ip6:2001:db8::1 ~all" {
		t.Fatalf("spf = %v", spf)
	}

	// SSL provisioning fired for the domain.
	if got := fx.renew.renewed(); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("renewed = %v", got)
	}
	if fx.proxy.syncCount() == 0 {
		t.Fatal("proxy not synced")
	}
}

func TestAddRejectsInvalidFormats(t *testing.T) {
	fx := newFixture(t)

	for _, bad := range []string{"invalid", "../hack.com", "a", "exa/mple.com", `exa\mple.com`} {
		r := fx.d.Add(bad, "myapp")
		if r.Status {
			t.Fatalf("%q accepted", bad)
		}
		if !strings.Contains(msgOf(t, r.Message), "Invalid domain format") {
			t.Fatalf("%q message = %v", bad, r.Message)
		}
	}

	r := fx.d.Add(nil, "myapp")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "Domain is required") {
		t.Fatalf("nil domain = %+v", r)
	}
	r = fx.d.Add("example.com", nil)
	if r.Status || !strings.Contains(msgOf(t, r.Message), "App ID is required") {
		t.Fatalf("nil app = %+v", r)
	}
}

func TestAddRejectsUnknownApp(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("example.com", "nonexistent")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "App nonexistent not found") {
		t.Fatalf("r = %+v", r)
	}
	if fx.domain("example.com") != nil {
		t.Fatal("domain registered despite missing app")
	}
}

func TestAddRejectsDuplicate(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{"example.com": map[string]any{"appId": "myapp"}})

	r := fx.d.Add("example.com", "otherapp")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "Domain example.com is already registered") {
		t.Fatalf("r = %+v", r)
	}
}

// The other half of the host-networking gate (appmgr.SetNetworkMode guards
// the reverse direction): a host-mode app cannot run Blue-Green, so it must
// not acquire a routed domain through the back door either.
func TestAddRejectsHostNetworkedApp(t *testing.T) {
	hostFixture := func(t *testing.T) *fixture {
		fx := newFixture(t)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("apps", []any{
				map[string]any{"id": "app-1", "name": "myapp", "networkMode": "host"},
				map[string]any{"id": "app-2", "name": "otherapp"},
			})
		})
		return fx
	}

	t.Run("root domain", func(t *testing.T) {
		fx := hostFixture(t)

		r := fx.d.Add("example.com", "myapp")
		if r.Status || !strings.Contains(msgOf(t, r.Message), "host networking") {
			t.Fatalf("r = %+v", r)
		}
		if fx.domain("example.com") != nil {
			t.Fatal("domain registered for a host-networked app")
		}
		if len(fx.dns.recorded()) != 0 || len(fx.renew.renewed()) != 0 {
			t.Fatal("DNS/SSL fired despite the refusal")
		}
	})

	// The subdomain branch sits after the guard, so it must be covered too.
	t.Run("subdomain of an existing parent", func(t *testing.T) {
		fx := hostFixture(t)
		fx.setDomains(map[string]any{
			"example.com": map[string]any{"appId": "myapp", "created": nowMs()},
		})

		r := fx.d.Add("sub.example.com", "myapp")
		if r.Status || !strings.Contains(msgOf(t, r.Message), "host networking") {
			t.Fatalf("r = %+v", r)
		}
		rec := fx.domain("example.com")
		if subs, _ := rec["subdomain"].([]any); len(subs) != 0 {
			t.Fatalf("subdomain added anyway: %v", rec["subdomain"])
		}
	})

	// Lookup by id must be gated identically to lookup by name.
	t.Run("app referenced by id", func(t *testing.T) {
		fx := hostFixture(t)
		if r := fx.d.Add("example.com", "app-1"); r.Status {
			t.Fatal("host-networked app accepted a domain via its id")
		}
	})

	// A bridge app in the same config must stay unaffected.
	t.Run("bridge app is unaffected", func(t *testing.T) {
		fx := hostFixture(t)
		if r := fx.d.Add("example.com", "otherapp"); !r.Status {
			t.Fatalf("bridge app refused: %v", r.Message)
		}
	})
}

func TestAddSkipsDNSAndSSLForLocalhost(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("localhost", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}
	rec := fx.domain("localhost")
	if rec == nil {
		t.Fatal("localhost not registered")
	}
	if subs, _ := rec["subdomain"].([]any); len(subs) != 0 {
		t.Fatalf("subdomain = %v", rec["subdomain"])
	}
	if _, hasCert := rec["cert"]; hasCert {
		t.Fatal("cert tracking initialized for localhost")
	}
	if len(fx.dns.recorded()) != 0 {
		t.Fatalf("DNS records created: %v", fx.dns.recorded())
	}
	if len(fx.renew.renewed()) != 0 {
		t.Fatalf("SSL renewed: %v", fx.renew.renewed())
	}
}

func TestAddSkipsDNSForIPAddress(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("192.168.1.10", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}
	if len(fx.dns.recorded()) != 0 || len(fx.renew.renewed()) != 0 {
		t.Fatal("DNS/SSL fired for an IP domain")
	}
}

func TestAddSubdomainToExistingParent(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{"appId": "myapp", "created": nowMs()},
	})

	r := fx.d.Add("sub.example.com", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}
	if !strings.Contains(msgOf(t, r.Message), "Added sub.example.com as a subdomain of example.com") {
		t.Fatalf("message = %v", r.Message)
	}

	parent := fx.domain("example.com")
	if !listContains(parent["subdomain"], "sub") {
		t.Fatalf("parent subdomain = %v", parent["subdomain"])
	}
	if fx.domain("sub.example.com") != nil {
		t.Fatal("subdomain added as a separate domain")
	}

	recs := fx.dns.recorded()
	if len(recs) != 1 || recs[0]["type"] != "CNAME" ||
		recs[0]["name"] != "sub.example.com" || recs[0]["value"] != "example.com" {
		t.Fatalf("dns records = %v", recs)
	}

	if got := fx.renew.renewed(); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("renewed = %v", got)
	}
}

func TestAddSubdomainAlreadyExists(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{"appId": "myapp", "subdomain": []any{"sub"}},
	})

	r := fx.d.Add("sub.example.com", "myapp")
	if !r.Status || !strings.Contains(msgOf(t, r.Message), "Subdomain sub already exists on example.com") {
		t.Fatalf("r = %+v", r)
	}
	if len(fx.dns.recorded()) != 0 || len(fx.renew.renewed()) != 0 {
		t.Fatal("side effects fired for an existing subdomain")
	}
}

func TestAddStripsProtocolAndWWW(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("https://WWW.Example.com", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}
	if fx.domain("example.com") == nil {
		t.Fatal("www./protocol prefix not stripped")
	}
}

func TestAddWildcardDomain(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.Add("*.example.com", "myapp")
	if !r.Status {
		t.Fatalf("add failed: %v", r.Message)
	}
	rec := fx.domain("*.example.com")
	if subs, _ := rec["subdomain"].([]any); len(subs) != 0 {
		t.Fatalf("wildcard subdomain list = %v", rec["subdomain"])
	}
	// Wildcards get only A + AAAA (no mail/MX/TXT set).
	if len(fx.dns.recorded()) != 2 {
		t.Fatalf("dns records = %v", fx.dns.recorded())
	}
	if got := fx.renew.renewed(); len(got) != 1 || got[0] != "*.example.com" {
		t.Fatalf("renewed = %v", got)
	}
}

// ─── delete() ───────────────────────────────────────────────────────────

func TestDeleteExistingDomain(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{"appId": "myapp", "created": nowMs()},
	})

	r := fx.d.Delete("example.com", false)
	if !r.Status {
		t.Fatalf("delete failed: %v", r.Message)
	}
	if fx.domain("example.com") != nil {
		t.Fatal("domain still registered")
	}
	// Full record sweep: A, AAAA, www CNAME, mail A, MX, TXT, DMARC, DKIM.
	if got := len(fx.dns.deleted()); got != 8 {
		t.Fatalf("dns deletes = %d (%v)", got, fx.dns.deleted())
	}
	// Default DKIM selector when the record carries none.
	foundDKIM := false
	for _, del := range fx.dns.deleted() {
		if del["name"] == "default._domainkey.example.com" {
			foundDKIM = true
		}
	}
	if !foundDKIM {
		t.Fatalf("DKIM TXT not deleted: %v", fx.dns.deleted())
	}
	// Mail TLS cache cleared for the deleted domain.
	if len(fx.mail.clears) != 1 || fx.mail.clears[0] != "example.com" {
		t.Fatalf("mail clears = %v", fx.mail.clears)
	}
}

func TestDeleteMissingDomain(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{"example.com": map[string]any{"appId": "myapp"}})

	r := fx.d.Delete("other.com", false)
	if r.Status || !strings.Contains(msgOf(t, r.Message), "Domain other.com not found") {
		t.Fatalf("r = %+v", r)
	}
}

func TestDeleteSubdomain(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{"appId": "myapp", "created": nowMs(), "subdomain": []any{"sub"}},
	})

	r := fx.d.Delete("sub.example.com", false)
	if !r.Status || !strings.Contains(msgOf(t, r.Message), "Subdomain sub removed from example.com") {
		t.Fatalf("r = %+v", r)
	}
	rec := fx.domain("example.com")
	if subs, _ := rec["subdomain"].([]any); len(subs) != 0 {
		t.Fatalf("subdomain = %v", rec["subdomain"])
	}
	dels := fx.dns.deleted()
	if len(dels) != 1 || dels[0]["type"] != "CNAME" || dels[0]["name"] != "sub.example.com" {
		t.Fatalf("dns deletes = %v", dels)
	}
	if got := fx.renew.renewed(); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("renewed = %v", got)
	}
}

func TestDeleteWWWSubdomainKeepsParent(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{"appId": "myapp", "created": nowMs(), "subdomain": []any{"www", "mail"}},
	})

	r := fx.d.Delete("www.example.com", false)
	if !r.Status || !strings.Contains(msgOf(t, r.Message), "Subdomain www removed from example.com") {
		t.Fatalf("r = %+v", r)
	}
	rec := fx.domain("example.com")
	if rec == nil {
		t.Fatal("parent domain deleted")
	}
	subs, _ := rec["subdomain"].([]any)
	if len(subs) != 1 || subs[0] != "mail" {
		t.Fatalf("subdomain = %v", subs)
	}
}

func TestDeleteLocalhostSkipsDNS(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{"localhost": map[string]any{"appId": "myapp"}})

	r := fx.d.Delete("localhost", false)
	if !r.Status {
		t.Fatalf("delete failed: %v", r.Message)
	}
	if len(fx.dns.deleted()) != 0 || len(fx.mail.clears) != 0 {
		t.Fatal("DNS/mail cleanup fired for localhost")
	}
}

func TestDeleteUsesRecordedDKIMSelector(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"example.com": map[string]any{
			"appId": "myapp",
			"cert":  map[string]any{"dkim": map[string]any{"selector": "odac1"}},
		},
	})

	fx.d.Delete("example.com", false)
	found := false
	for _, del := range fx.dns.deleted() {
		if del["name"] == "odac1._domainkey.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("selector-specific DKIM record not deleted: %v", fx.dns.deleted())
	}
}

// ─── list() ─────────────────────────────────────────────────────────────

func listData(t *testing.T, fx *fixture, appID any) []map[string]any {
	t.Helper()
	r := fx.d.List(appID)
	if !r.Status {
		t.Fatalf("list failed: %+v", r)
	}
	raw, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("list data = %s: %v", raw, err)
	}
	return rows
}

func TestListAllDomains(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"app1.com": map[string]any{"appId": "myapp", "created": float64(1000)},
		"app2.com": map[string]any{"appId": "otherapp", "created": float64(2000)},
	})

	rows := listData(t, fx, nil)
	if len(rows) != 2 || rows[0]["domain"] != "app1.com" || rows[1]["domain"] != "app2.com" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["app"] != "myapp" || rows[0]["created"] != float64(1000) {
		t.Fatalf("row 0 = %v", rows[0])
	}
}

func TestListFiltersByApp(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"app1.com": map[string]any{"appId": "myapp", "created": float64(1000)},
		"app2.com": map[string]any{"appId": "otherapp", "created": float64(2000)},
	})

	rows := listData(t, fx, "myapp")
	if len(rows) != 1 || rows[0]["domain"] != "app1.com" {
		t.Fatalf("rows = %v", rows)
	}

	r := fx.d.List("ghost")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "No domains found for app ghost") {
		t.Fatalf("r = %+v", r)
	}
}

func TestListEmpty(t *testing.T) {
	fx := newFixture(t)

	r := fx.d.List(nil)
	if !r.Status {
		t.Fatalf("r = %+v", r)
	}
	raw, _ := json.Marshal(r.Data)
	if string(raw) != "[]" {
		t.Fatalf("data = %s", raw)
	}
}

func TestListPayloadKeyOrderMatchesNode(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"app1.com": map[string]any{"appId": "myapp", "created": float64(1000), "subdomain": []any{"www"}},
	})

	r := fx.d.List(nil)
	raw, _ := json.Marshal(r.Data)
	want := `[{"domain":"app1.com","subdomain":["www"],"app":"myapp","created":1000}]`
	if string(raw) != want {
		t.Fatalf("payload = %s, want %s", raw, want)
	}
}

func TestListSanitizesAppIDStrings(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"app1.com": map[string]any{"appId": "myapp", "created": float64(1000)},
	})

	// "undefined"/"null" strings behave like no filter (Node's sanitize).
	for _, arg := range []any{"undefined", "NULL", float64(42)} {
		rows := listData(t, fx, arg)
		if len(rows) != 1 {
			t.Fatalf("arg %v: rows = %v", arg, rows)
		}
	}
}

// ─── deleteByApp() ──────────────────────────────────────────────────────

func TestDeleteByApp(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"site1.com": map[string]any{"appId": "myapp", "created": float64(1000)},
		"site2.com": map[string]any{"appId": "myapp", "created": float64(1100)},
		"other.com": map[string]any{"appId": "otherapp", "created": float64(2000)},
	})

	if err := fx.d.DeleteByApp("myapp"); err != nil {
		t.Fatal(err)
	}

	if fx.domain("site1.com") != nil || fx.domain("site2.com") != nil {
		t.Fatal("app domains survived")
	}
	if fx.domain("other.com") == nil {
		t.Fatal("unrelated domain deleted")
	}
	// 8 records × 2 domains, exactly like the jest expectation.
	if got := len(fx.dns.deleted()); got != 16 {
		t.Fatalf("dns deletes = %d", got)
	}
}

func TestDeleteByAppNoDomains(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"other.com": map[string]any{"appId": "otherapp"},
	})
	before := fx.proxy.syncCount()

	if err := fx.d.DeleteByApp("nonexistent-app"); err != nil {
		t.Fatal(err)
	}
	if fx.domain("other.com") == nil {
		t.Fatal("unrelated domain deleted")
	}
	if fx.proxy.syncCount() != before {
		t.Fatal("proxy synced despite no deletions")
	}
	if err := fx.d.DeleteByApp(""); err != nil {
		t.Fatal(err)
	}
}
