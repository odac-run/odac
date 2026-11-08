package domains

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/mtls"
)

type fakeMTLSProxy struct {
	mu        sync.Mutex
	certSyncs int
	syncs     int
	tunnels   map[string][]string
}

func (f *fakeMTLSProxy) SyncClientCerts() {
	f.mu.Lock()
	f.certSyncs++
	f.syncs++
	f.mu.Unlock()
}

func (f *fakeMTLSProxy) SyncConfig() {
	f.mu.Lock()
	f.syncs++
	f.mu.Unlock()
}

func (f *fakeMTLSProxy) TunnelDomains(app string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tunnels[app]
}

func (f *fakeMTLSProxy) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs
}

type mtlsFixture struct {
	base  string
	cfg   *config.Store
	m     *MTLS
	proxy *fakeMTLSProxy
}

func newMTLSFixture(t *testing.T) *mtlsFixture {
	t.Helper()
	base := t.TempDir()
	cfg, err := config.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mutate(func() {
		cfg.Set("apps", []any{
			map[string]any{"id": "a1", "name": "myapp", "ports": []any{map[string]any{"host": "proxy", "container": float64(3000)}}},
			map[string]any{"id": "a2", "name": "other"},
		})
		cfg.Set("domains", map[string]any{
			"x.com":   map[string]any{"appId": "myapp", "subdomain": []any{"www", "auth", "admin", "*"}},
			"y.com":   map[string]any{"appId": "myapp", "subdomain": []any{"www"}},
			"z.com":   map[string]any{"appId": "other", "subdomain": []any{"www"}},
			"*.w.com": map[string]any{"appId": "other", "subdomain": []any{}},
		})
	})
	proxy := &fakeMTLSProxy{tunnels: map[string][]string{}}
	m := NewMTLS(cfg, proxy)
	t.Cleanup(m.Stop)
	return &mtlsFixture{base: base, cfg: cfg, m: m, proxy: proxy}
}

// storedCert reads a record the way the certificate logic does.
func storedCert(t *testing.T, m *MTLS, fingerprint string) map[string]any {
	t.Helper()
	store, err := m.certs()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.get(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

type proxyStore struct {
	Certs   map[string][]string `json:"certs"`
	Revoked []string            `json:"revoked"`
}

// proxyPayload is what the proxy would decode from a push.
func (fx *mtlsFixture) proxyPayload(t *testing.T) (proxyStore, string) {
	t.Helper()
	certs, err := fx.m.CertSnapshot(fx.m.now())
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	fx.cfg.View(func() { raw, err = json.Marshal(mtls.StorePayload(fx.cfg.Map("mtls"), certs)) })
	if err != nil {
		t.Fatal(err)
	}
	var out proxyStore
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return out, string(raw)
}

func newCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "requested"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func mustOK(t *testing.T, r api.Result) api.Result {
	t.Helper()
	if !r.Status {
		t.Fatalf("result failed: %v", r.Message)
	}
	return r
}

func mustFail(t *testing.T, r api.Result, want string) {
	t.Helper()
	if r.Status {
		t.Fatalf("expected a failure mentioning %q, got success: %v", want, r.Message)
	}
	if msg, _ := r.Message.(string); !strings.Contains(msg, want) {
		t.Fatalf("message = %q, want it to mention %q", msg, want)
	}
}

func (fx *mtlsFixture) issue(t *testing.T, domain, name string, hosts ...string) map[string]any {
	t.Helper()
	r := mustOK(t, fx.m.Issue(api.Operator, domain, map[string]any{"name": name, "hosts": anyList(hosts), "csr": newCSR(t)}))
	return r.Data.(map[string]any)
}

func parseIssued(t *testing.T, reply map[string]any) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(reply["cert"].(string)))
	if block == nil {
		t.Fatal("no certificate in the reply")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func (fx *mtlsFixture) record(name string) map[string]any {
	var out map[string]any
	fx.cfg.View(func() {
		rec, _ := fx.cfg.Map("domains")[name].(map[string]any)
		cfg, _ := rec["mtls"].(map[string]any)
		out = cfg
	})
	return out
}

func (fx *mtlsFixture) certRecord(t *testing.T, fingerprint string) map[string]any {
	t.Helper()
	return storedCert(t, fx.m, fingerprint)
}

func TestIssueCreatesACAAndAClientOnlyCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "auth.x.com", "ali", "auth.x.com")
	cert := parseIssued(t, reply)

	if cert.Subject.CommonName != "ali" {
		t.Fatalf("subject = %q", cert.Subject.CommonName)
	}
	if cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("usage: ca=%v eku=%v", cert.IsCA, cert.ExtKeyUsage)
	}

	block, _ := pem.Decode([]byte(reply["chain"].(string)))
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// pathlen 1: room for one per-host issuer, nothing deeper.
	if !ca.IsCA || ca.MaxPathLen != 1 {
		t.Fatalf("CA: isCA=%v pathLen=%d", ca.IsCA, ca.MaxPathLen)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("does not chain: %v", err)
	}
	if fx.proxy.syncCount() != 1 {
		t.Fatalf("syncs = %d", fx.proxy.syncCount())
	}
}

func TestCSRCannotAskForMoreThanAClientCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	basic, _ := asn1.Marshal(struct {
		IsCA bool `asn1:"optional"`
	}{true})
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:         pkix.Name{CommonName: "admin", Organization: []string{"Evil"}},
		DNSNames:        []string{"evil.com"},
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: basic}},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))

	r := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}))
	cert := parseIssued(t, r.Data.(map[string]any))
	if cert.IsCA || cert.BasicConstraintsValid && cert.IsCA {
		t.Fatal("a CSR obtained CA powers")
	}
	if cert.Subject.CommonName != "ali" || len(cert.Subject.Organization) != 0 {
		t.Fatalf("subject = %v", cert.Subject)
	}
	if len(cert.DNSNames) != 0 {
		t.Fatalf("SANs = %v", cert.DNSNames)
	}
}

func TestIssueRejectsBadRequests(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "hosts": []any{"x.com"}, "csr": "junk"}), "Invalid certificate request")

	csr := newCSR(t)
	mustFail(t, fx.m.Issue(api.Operator, "nope.com", map[string]any{"name": "a", "hosts": []any{"x.com"}, "csr": csr}), "not found")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "", "hosts": []any{"x.com"}, "csr": csr}), "name")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a\nb", "hosts": []any{"x.com"}, "csr": csr}), "name")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "csr": csr}), "host")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "hosts": []any{"nope.com"}, "csr": csr}), "not registered")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "hosts": []any{"x.com"}, "csr": csr, "days": float64(0)}), "Lifetime")
}

func TestParseCSRRejectsWeakRSA(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, weak)
	if err != nil {
		t.Fatal(err)
	}
	_, msg := parseCSR(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})))
	if !strings.Contains(msg, "2048") {
		t.Fatalf("message = %q", msg)
	}
}

func TestIssueIsIdempotentOnThePublicKey(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	opts := map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}
	first := mustOK(t, fx.m.Issue(api.Operator, "x.com", opts)).Data.(map[string]any)
	syncs := fx.proxy.syncCount()
	second := mustOK(t, fx.m.Issue(api.Operator, "x.com", opts)).Data.(map[string]any)

	if first["fingerprint"] != second["fingerprint"] {
		t.Fatal("a retry signed a second certificate")
	}
	if second["cert"] != first["cert"] || second["chain"] != first["chain"] {
		t.Fatal("a retry returned different material")
	}
	if fx.proxy.syncCount() != syncs {
		t.Fatal("a retry changed nothing and must not sync")
	}
}

func TestConcurrentRetriesProduceOneCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	opts := map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}

	var wg sync.WaitGroup
	results := make([]api.Result, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = fx.m.Issue(api.Operator, "x.com", opts)
		}(i)
	}
	wg.Wait()

	seen := map[any]bool{}
	for _, r := range results {
		mustOK(t, r)
		seen[r.Data.(map[string]any)["fingerprint"]] = true
	}
	if len(seen) != 1 {
		t.Fatalf("%d certificates from one key", len(seen))
	}
}

func TestRevokedKeyCannotBeReissued(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	opts := map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}
	reply := mustOK(t, fx.m.Issue(api.Operator, "x.com", opts)).Data.(map[string]any)
	mustOK(t, fx.m.Revoke(api.Operator, reply["fingerprint"]))
	mustFail(t, fx.m.Issue(api.Operator, "x.com", opts), "revoked")
}

func TestNameIsUniqueAmongLiveCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	first := fx.issue(t, "x.com", "ali", "x.com")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": newCSR(t)}), "already exists")

	mustOK(t, fx.m.Revoke(api.Operator, first["fingerprint"]))
	fx.issue(t, "x.com", "ali", "x.com")
}

func TestRenewKeepsTheIdentityAndOverlaps(t *testing.T) {
	fx := newMTLSFixture(t)
	old := fx.issue(t, "x.com", "ali", "auth.x.com")
	renewed := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": old["fingerprint"], "csr": newCSR(t)})).Data.(map[string]any)

	if renewed["name"] != "ali" {
		t.Fatalf("name = %v", renewed["name"])
	}
	if hosts := stringList(renewed["hosts"]); len(hosts) != 1 || hosts[0] != "auth.x.com" {
		t.Fatalf("hosts = %v", hosts)
	}
	if fx.certRecord(t, old["fingerprint"].(string))["revoked"] != nil {
		t.Fatal("the old certificate must stay valid until revoked")
	}
}

func TestRenewWithTheSameKeySignsANewCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	old := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"auth.x.com"}, "csr": csr})).Data.(map[string]any)
	syncs := fx.proxy.syncCount()
	fx.m.now = func() time.Time { return time.Now().Add(24 * time.Hour) }

	renewed := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": old["fingerprint"], "csr": csr})).Data.(map[string]any)

	if renewed["fingerprint"] == old["fingerprint"] {
		t.Fatal("renewing returned the old certificate")
	}
	if !parseIssued(t, renewed).NotAfter.After(parseIssued(t, old).NotAfter) {
		t.Fatal("the renewal did not extend the lifetime")
	}
	if fx.proxy.syncCount() != syncs+1 {
		t.Fatal("the new certificate must reach the proxy")
	}
	if renewed["name"] != "ali" {
		t.Fatalf("name = %v", renewed["name"])
	}
}

func TestRenewingAnExpiredCertificateWithTheSameKey(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	real := fx.m.now
	fx.m.now = func() time.Time { return time.Now().AddDate(0, 0, -10) }
	old := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr, "days": float64(1)})).Data.(map[string]any)
	fx.m.now = real

	renewed := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": old["fingerprint"], "csr": csr})).Data.(map[string]any)
	if !parseIssued(t, renewed).NotAfter.After(time.Now()) {
		t.Fatal("a renewal returned an expired certificate")
	}
}

func TestExpiredCertificateIsNeverReturnedAsASuccess(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	opts := map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr, "days": float64(1)}
	real := fx.m.now
	fx.m.now = func() time.Time { return time.Now().AddDate(0, 0, -10) }
	mustOK(t, fx.m.Issue(api.Operator, "x.com", opts))
	fx.m.now = real

	mustFail(t, fx.m.Issue(api.Operator, "x.com", opts), "expired")
}

// A lost reply leaves the caller repeating the renewal. Each round adds a record
// sharing the key, and map order must not decide between returning one and signing.
func TestRenewRetryReturnsTheSameCertificate(t *testing.T) {
	for _, tc := range []struct {
		name string
		csr  func(oldCSR string) string
	}{
		{"same key", func(oldCSR string) string { return oldCSR }},
		{"new key", func(string) string { return newCSR(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMTLSFixture(t)
			oldCSR := newCSR(t)
			old := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": oldCSR})).Data.(map[string]any)
			opts := map[string]any{"renews": old["fingerprint"], "csr": tc.csr(oldCSR)}
			first := mustOK(t, fx.m.Issue(api.Operator, "x.com", opts)).Data.(map[string]any)
			syncs := fx.proxy.syncCount()
			for range 50 {
				second := mustOK(t, fx.m.Issue(api.Operator, "x.com", opts)).Data.(map[string]any)

				if second["fingerprint"] != first["fingerprint"] || second["cert"] != first["cert"] {
					t.Fatal("a retried renewal signed another certificate")
				}
				if fx.proxy.syncCount() != syncs {
					t.Fatal("a retry changed nothing and must not sync")
				}
			}
		})
	}
}

func TestRenewChainFollowsTheNamedCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	base := time.Now()
	fx.m.now = func() time.Time { return base }
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	fx.m.now = func() time.Time { return base.Add(time.Hour) }
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)
	fx.m.now = func() time.Time { return base.Add(2 * time.Hour) }
	c := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": b["fingerprint"], "csr": csr})).Data.(map[string]any)

	if c["fingerprint"] == a["fingerprint"] || c["fingerprint"] == b["fingerprint"] {
		t.Fatal("renewing the newest certificate returned an older one")
	}
	for range 50 {
		retry := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
		if retry["fingerprint"] != c["fingerprint"] {
			t.Fatal("a plain retry must return the newest certificate for the key")
		}
		retry = mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)
		if retry["fingerprint"] != b["fingerprint"] {
			t.Fatal("a retry must return what that renewal produced")
		}
	}
}

func TestRetriedRenewalSignsAgainOnceItsResultExpired(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	old := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	real := fx.m.now
	fx.m.now = func() time.Time { return time.Now().AddDate(0, 0, -10) }
	first := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": old["fingerprint"], "csr": csr, "days": float64(1)})).Data.(map[string]any)
	fx.m.now = real

	again := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": old["fingerprint"], "csr": csr})).Data.(map[string]any)
	if again["fingerprint"] == first["fingerprint"] {
		t.Fatal("an expired renewal was returned as a retry")
	}
}

func TestRenewsIsValidatedEvenForAKnownKey(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}))
	unknown := strings.Repeat("ab", 32)

	if r := fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": "not-a-fingerprint", "csr": csr}); r.Status {
		t.Fatal("a malformed fingerprint was accepted")
	}
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": unknown, "csr": csr}), "not found")

	revoked := fx.issue(t, "x.com", "veli", "x.com")
	mustOK(t, fx.m.Revoke(api.Operator, revoked["fingerprint"]))
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": revoked["fingerprint"], "csr": csr}), "not found")

	foreign := fx.issue(t, "z.com", "ayse", "z.com")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": foreign["fingerprint"], "csr": csr}), "not found")
}

func TestAKeyCannotChangeIdentity(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}))
	veli := fx.issue(t, "x.com", "veli", "auth.x.com")

	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": veli["fingerprint"], "csr": csr}), "another certificate")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ayse", "hosts": []any{"admin.x.com"}, "csr": csr}), "another certificate")
}

func TestRevokingTheOldCertificateKeepsItsKeyRenewable(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)
	mustOK(t, fx.m.Revoke(api.Operator, a["fingerprint"]))

	c := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": b["fingerprint"], "csr": csr})).Data.(map[string]any)
	if c["fingerprint"] == b["fingerprint"] {
		t.Fatal("the renewal did not sign a new certificate")
	}
	for range 50 {
		retry := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": b["fingerprint"], "csr": csr})).Data.(map[string]any)
		if retry["fingerprint"] != c["fingerprint"] {
			t.Fatal("a retry after revoking the old certificate must return the renewal")
		}
	}
}

func TestAKeyWithNoLiveCertificateStaysDead(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)
	mustOK(t, fx.m.Revoke(api.Operator, a["fingerprint"]))
	mustOK(t, fx.m.Revoke(api.Operator, b["fingerprint"]))

	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr}), "revoked")

	live := fx.issue(t, "x.com", "ali", "x.com")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": live["fingerprint"], "csr": csr}), "revoked")
}

func TestRevokeNamesOtherCertificatesOnTheSameKey(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)

	r := mustOK(t, fx.m.Revoke(api.Operator, a["fingerprint"]))
	if msg, _ := r.Message.(string); !strings.Contains(msg, b["fingerprint"].(string)) {
		t.Fatalf("message = %q, want the certificate still holding the key", msg)
	}
	data, _ := r.Data.(map[string]any)
	if same := stringList(data["sameKey"]); len(same) != 1 || same[0] != b["fingerprint"] {
		t.Fatalf("sameKey = %v", data["sameKey"])
	}
	if fx.certRecord(t, b["fingerprint"].(string))["revoked"] != nil {
		t.Fatal("revoking one certificate must not revoke its renewal")
	}

	r = mustOK(t, fx.m.Revoke(api.Operator, b["fingerprint"]))
	if r.HasData {
		t.Fatalf("no other live certificate, unexpected data: %v", r.Data)
	}
}

func TestRevokeIgnoresExpiredCertificatesOnTheSameKey(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	real := fx.m.now
	fx.m.now = func() time.Time { return time.Now().AddDate(0, 0, -10) }
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr, "days": float64(1)})).Data.(map[string]any)
	fx.m.now = real
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)

	if r := mustOK(t, fx.m.Revoke(api.Operator, b["fingerprint"])); r.HasData {
		t.Fatalf("an expired certificate opens nothing, unexpected data: %v", r.Data)
	}
}

func TestSameKeyRenewalReachesTheProxyAsItsOwnCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"auth.x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)
	mustOK(t, fx.m.Revoke(api.Operator, a["fingerprint"]))

	payload, _ := fx.proxyPayload(t)
	if len(payload.Revoked) != 1 || payload.Revoked[0] != a["fingerprint"] {
		t.Fatalf("revoked = %v", payload.Revoked)
	}
	if hosts := payload.Certs[b["fingerprint"].(string)]; len(hosts) != 1 || hosts[0] != "auth.x.com" {
		t.Fatalf("scope of the renewal = %v", hosts)
	}
}

func TestConcurrentRenewalsProduceOneCertificate(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	old := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	opts := map[string]any{"renews": old["fingerprint"], "csr": csr}

	var wg sync.WaitGroup
	results := make([]api.Result, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = fx.m.Issue(api.Operator, "x.com", opts)
		}(i)
	}
	wg.Wait()

	seen := map[any]bool{}
	for _, r := range results {
		mustOK(t, r)
		seen[r.Data.(map[string]any)["fingerprint"]] = true
	}
	if len(seen) != 1 || seen[old["fingerprint"]] {
		t.Fatalf("got %d certificates, old one included: %v", len(seen), seen[old["fingerprint"]])
	}
}

func TestRenewedFromStaysOutOfTheList(t *testing.T) {
	fx := newMTLSFixture(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr})).Data.(map[string]any)

	if fx.certRecord(t, b["fingerprint"].(string))["renewedFrom"] != a["fingerprint"] {
		t.Fatal("the renewal must record what it replaced")
	}
	for _, row := range fx.m.List(api.Operator, nil).Data.([]any) {
		if _, ok := row.(map[string]any)["renewedFrom"]; ok {
			t.Fatal("the dashboard diffs these rows, this field adds nothing to them")
		}
	}
}

func TestScopeValidates(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "x.com", "ali", "auth.x.com")
	fp := reply["fingerprint"]

	mustFail(t, fx.m.Scope(fp, []any{}), "host")
	mustFail(t, fx.m.Scope(fp, []any{"nope.com"}), "not registered")
	mustFail(t, fx.m.Scope(fp, []any{"z.com"}), "does not trust")
	mustFail(t, fx.m.Scope("not-a-fingerprint", []any{"x.com"}), "fingerprint")
	mustFail(t, fx.m.Scope(strings.Repeat("a", 64), []any{"x.com"}), "not found")

	syncs := fx.proxy.syncCount()
	mustOK(t, fx.m.Scope(fp, []any{"ADMIN.x.com", "admin.x.com", "www.auth.x.com"}))
	hosts := stringList(fx.certRecord(t, fp.(string))["hosts"])
	if len(hosts) != 2 || hosts[0] != "admin.x.com" || hosts[1] != "auth.x.com" {
		t.Fatalf("hosts = %v", hosts)
	}
	if fx.proxy.syncCount() != syncs+1 {
		t.Fatal("a scope change must reach the proxy")
	}
}

func TestWildcardScopeNeedsAWildcardOnTheRecord(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "x.com", "ali", "*.x.com")
	if hosts := stringList(reply["hosts"]); hosts[0] != "*.x.com" {
		t.Fatalf("hosts = %v", hosts)
	}
	mustFail(t, fx.m.Issue(api.Operator, "y.com", map[string]any{"name": "b", "hosts": []any{"*.y.com"}, "csr": newCSR(t)}), "not registered")
	fx.issue(t, "*.w.com", "c", "foo.w.com")
}

func TestRequiredIsRefusedUntilSomeoneCanEnter(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.m.Set("x.com", "required", nil), "lock everyone out")
	var cas map[string]any
	fx.cfg.View(func() { cas, _ = fx.cfg.Map("mtls")["cas"].(map[string]any) })
	if len(cas) != 0 {
		t.Fatal("a refused request left a CA behind")
	}

	fx.issue(t, "x.com", "ali", "auth.x.com")
	mustFail(t, fx.m.Set("admin.x.com", "required", nil), "lock everyone out")
	mustOK(t, fx.m.Set("auth.x.com", "required", nil))
	mustOK(t, fx.m.Set("x.com", "required", nil))
}

func TestRevokedOrExpiredCertificatesDoNotCountAsAWayIn(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "x.com", "ali", "x.com")
	mustOK(t, fx.m.Revoke(api.Operator, reply["fingerprint"]))
	mustFail(t, fx.m.Set("x.com", "required", nil), "lock everyone out")

	fx.issue(t, "x.com", "veli", "x.com")
	fx.m.now = func() time.Time { return time.Now().Add(2 * 365 * 24 * time.Hour) }
	mustFail(t, fx.m.Set("x.com", "required", nil), "lock everyone out")
}

func TestOptionalNeedsNoCertificateAndCreatesACA(t *testing.T) {
	fx := newMTLSFixture(t)
	r := mustOK(t, fx.m.Set("x.com", "optional", nil))
	if msg := r.Message.(string); !strings.Contains(msg, mtls.KeyEnv) {
		t.Fatalf("first policy must ask for a restart: %q", msg)
	}
	cfg := fx.record("x.com")
	if cfg["mode"] != mtls.ModeOptional || len(stringList(cfg["ca"])) != 1 {
		t.Fatalf("record = %v", cfg)
	}
	if fx.proxy.syncCount() != 1 {
		t.Fatalf("syncs = %d", fx.proxy.syncCount())
	}

	r = mustOK(t, fx.m.Set("auth.x.com", "optional", nil))
	if msg := r.Message.(string); strings.Contains(msg, mtls.KeyEnv) {
		t.Fatalf("the app already has its key: %q", msg)
	}
}

func TestSetRejectsWhatCannotWork(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.m.Set("x.com", "maybe", nil), "Mode")
	mustFail(t, fx.m.Set("nope.com", "optional", nil), "not found")
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"ca": "missing"}), "Unknown CA")
	mustFail(t, fx.m.Set("*.x.com", "optional", nil), "x.com")

	fx.cfg.Mutate(func() {
		fx.cfg.Map("domains")["localhost"] = map[string]any{"appId": "myapp"}
		fx.cfg.Map("domains")["10.0.0.1"] = map[string]any{"appId": "myapp"}
	})
	mustFail(t, fx.m.Set("localhost", "optional", nil), "no TLS")
	mustFail(t, fx.m.Set("10.0.0.1", "optional", nil), "no TLS")
}

func TestTunnelBlocksAPolicyOnTheWholeApp(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.proxy.tunnels["myapp"] = []string{"t.example"}
	mustFail(t, fx.m.Set("x.com", "optional", nil), "tunnel")
	mustFail(t, fx.m.Set("y.com", "optional", nil), "tunnel")
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "hosts": []any{"x.com"}, "csr": newCSR(t)}), "tunnel")
	mustOK(t, fx.m.Set("z.com", "optional", nil))
}

func TestWarnsAboutWhatStaysOpen(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.cfg.Mutate(func() {
		apps := fx.cfg.Get("apps").([]any)
		apps[0].(map[string]any)["ports"] = []any{map[string]any{"host": "8080", "container": float64(3000)}}
	})
	msg := mustOK(t, fx.m.Set("x.com", "optional", nil)).Message.(string)
	if !strings.Contains(msg, "8080") {
		t.Fatalf("published port not mentioned: %q", msg)
	}
	if !strings.Contains(msg, "y.com") {
		t.Fatalf("unprotected sibling not mentioned: %q", msg)
	}
}

// The dashboard shows each warning as its own callout and offers the restart, so
// they travel as data and not only inside the sentence.
func TestSetReportsWhatStaysOpenAsData(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.cfg.Mutate(func() {
		apps := fx.cfg.Get("apps").([]any)
		apps[0].(map[string]any)["ports"] = []any{
			map[string]any{"host": "8080", "container": float64(3000)},
			map[string]any{"host": float64(10022), "container": float64(22)},
			map[string]any{"host": "proxy", "container": float64(3001)},
		}
	})
	data := mustOK(t, fx.m.Set("x.com", "optional", nil)).Data.(map[string]any)
	if got := data["published"].([]int); !reflect.DeepEqual(got, []int{8080, 10022}) {
		t.Fatalf("published = %#v", data["published"])
	}
	if got := data["siblings"].([]string); !reflect.DeepEqual(got, []string{"y.com"}) {
		t.Fatalf("siblings = %#v", data["siblings"])
	}
	if data["restart"] != true {
		t.Fatal("the app's first mode needs a restart to deliver its signing key")
	}

	// The app already holds its key, and y.com is now protected too.
	data = mustOK(t, fx.m.Set("y.com", "optional", nil)).Data.(map[string]any)
	if data["restart"] != false || len(data["siblings"].([]string)) != 0 {
		t.Fatalf("second mode = %#v", data)
	}

	raw, _ := json.Marshal(mustOK(t, fx.m.Set("x.com", "off", nil)).Data)
	if string(raw) != `{"published":[],"restart":false,"siblings":[]}` {
		t.Fatalf("off = %s, want empty lists rather than null", raw)
	}
}

func TestOffIsTheOneCommandRecovery(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "x.com", "ali", "x.com", "auth.x.com")
	mustOK(t, fx.m.Set("x.com", "required", nil))
	mustOK(t, fx.m.Set("auth.x.com", "required", nil))

	mustOK(t, fx.m.Set("x.com", "off", nil))
	cfg := fx.record("x.com")
	if cfg["mode"] != nil || cfg["hosts"] != nil {
		t.Fatalf("record = %v", cfg)
	}
	if len(stringList(cfg["ca"])) != 1 {
		t.Fatal("the CA must survive so issued certificates still work when turned back on")
	}
	var apps map[string]bool
	fx.cfg.View(func() { apps = mtls.AppIDs(fx.cfg.Map("domains")) })
	if apps["myapp"] {
		t.Fatal("a record with nothing active must not be strict")
	}
}

func TestSubdomainOffKeepsTheRecordMode(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "x.com", "ali", "x.com")
	mustOK(t, fx.m.Set("x.com", "required", nil))
	mustOK(t, fx.m.Set("auth.x.com", "off", nil))

	hosts := fx.record("x.com")["hosts"].(map[string]any)
	if hosts["auth.x.com"].(map[string]any)["mode"] != mtls.ModeOff {
		t.Fatalf("hosts = %v", hosts)
	}

	// With no record mode there is nothing to override, so it stores nothing.
	mustOK(t, fx.m.Set("y.com", "optional", nil))
	mustOK(t, fx.m.Set("y.com", "off", nil))
	mustOK(t, fx.m.Set("www.y.com", "off", nil))
	if got := fx.record("y.com")["hosts"]; got != nil {
		t.Fatalf("hosts = %v", got)
	}
}

func newExternalCA(t *testing.T, isCA bool) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "Corp CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  isCA,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestExternalCA(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"caPem": "junk"}), "Invalid CA")
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"caPem": newExternalCA(t, false)}), "not a CA")
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"caPem": newExternalCA(t, true) + "-----BEGIN PRIVATE KEY-----"}), "private key")

	mustOK(t, fx.m.Set("x.com", "required", map[string]any{"caPem": newExternalCA(t, true)}))
	ids := stringList(fx.record("x.com")["ca"])
	if len(ids) != 1 {
		t.Fatalf("ca = %v", ids)
	}
	var ca map[string]any
	fx.cfg.View(func() { ca, _ = fx.cfg.Map("mtls")["cas"].(map[string]any)[ids[0]].(map[string]any) })
	if ca["managed"] != false || ca["name"] != "Corp CA" {
		t.Fatalf("ca = %v", ca)
	}
	mustFail(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "a", "hosts": []any{"x.com"}, "csr": newCSR(t)}), "external CA")
}

func TestRevokeIsPersistedAndSynced(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "x.com", "ali", "x.com")
	syncs := fx.proxy.syncCount()
	mustOK(t, fx.m.Revoke(api.Operator, reply["fingerprint"]))
	if fx.proxy.syncCount() != syncs+1 {
		t.Fatal("a revocation must reach the proxy")
	}

	reopened, err := config.Open(fx.base)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewMTLS(reopened, nil)
	t.Cleanup(restarted.Stop)
	cert := storedCert(t, restarted, reply["fingerprint"].(string))
	if cert == nil || cert["revoked"] == nil {
		t.Fatalf("a crash right after revoking must not undo it: %v", cert)
	}
}

func TestPrivateKeyStaysOutOfConfig(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "x.com", "ali", "x.com")

	entries, err := os.ReadDir(filepath.Join(fx.base, "cert", "mtls"))
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, entry := range entries {
		path := filepath.Join(fx.base, "cert", "mtls", entry.Name())
		if strings.HasSuffix(entry.Name(), ".key") {
			keys++
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("key mode = %v", info.Mode().Perm())
			}
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "PRIVATE KEY") {
			t.Fatalf("a private key reached %s", entry.Name())
		}
	}
	if keys != 1 {
		t.Fatalf("key files: %v", entries)
	}

	raw, err := os.ReadFile(filepath.Join(fx.base, "config", "mtls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("a private key reached the config file")
	}
	if _, dump := fx.proxyPayload(t); strings.Contains(dump, "PRIVATE KEY") {
		t.Fatal("a private key reached the proxy payload")
	}
}

func TestListLeavesOutMaterialAndFilters(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "x.com", "ali", "x.com")
	fx.issue(t, "z.com", "veli", "z.com")

	all := mustOK(t, fx.m.List(api.Operator, nil)).Data.([]any)
	if len(all) != 2 {
		t.Fatalf("rows = %v", all)
	}
	row := all[0].(map[string]any)
	if _, leaked := row["cert"]; leaked {
		t.Fatal("list runs on the hub's interval and must not carry PEMs")
	}
	if _, leaked := row["spki"]; leaked {
		t.Fatal("spki is internal")
	}

	filtered := mustOK(t, fx.m.List(api.Operator, "www.x.com")).Data.([]any)
	if len(filtered) != 1 || filtered[0].(map[string]any)["name"] != "ali" {
		t.Fatalf("filtered = %v", filtered)
	}
	mustFail(t, fx.m.List(api.Operator, "nope.com"), "not found")
}

func TestPruneDropsWhatADeletedDomainLeftBehind(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "z.com", "ali", "z.com")
	ids := stringList(fx.record("z.com")["ca"])
	keyPath := filepath.Join(fx.base, "cert", "mtls", ids[0]+".key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal(err)
	}

	fx.cfg.Mutate(func() { delete(fx.cfg.Map("domains"), "z.com") })
	fx.m.Prune()

	if fx.certRecord(t, reply["fingerprint"].(string)) != nil {
		t.Fatal("a certificate survived its CA's last domain")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("the signing key of a CA nothing uses must go")
	}
}

func TestPruneKeepsASharedCAButNarrowsScope(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "x.com", "ali", "x.com")
	ids := stringList(fx.record("x.com")["ca"])
	mustOK(t, fx.m.Set("y.com", "optional", map[string]any{"ca": ids[0]}))
	mustOK(t, fx.m.Scope(reply["fingerprint"], []any{"x.com", "y.com"}))

	fx.cfg.Mutate(func() { delete(fx.cfg.Map("domains"), "y.com") })
	fx.m.Prune()

	cert := fx.certRecord(t, reply["fingerprint"].(string))
	if hosts := stringList(cert["hosts"]); len(hosts) != 1 || hosts[0] != "x.com" {
		t.Fatalf("hosts = %v", hosts)
	}
}

func TestPruneRemovesOverridesOfDeletedSubdomains(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("auth.x.com", "optional", nil))
	fx.cfg.Mutate(func() {
		rec := fx.cfg.Map("domains")["x.com"].(map[string]any)
		rec["subdomain"] = []any{"www"}
	})
	fx.m.Prune()
	if got := fx.record("x.com")["hosts"]; got != nil {
		t.Fatalf("hosts = %v", got)
	}
}

func TestDomainDeleteRunsPrune(t *testing.T) {
	base := newFixture(t)
	mx := NewMTLS(base.cfg, &fakeMTLSProxy{})
	base.d.SetClientAuth(mx)
	mustOK(t, base.d.Add("sec.example.com", "myapp"))
	mustOK(t, base.d.Add("auth.sec.example.com", "myapp"))

	reply := mustOK(t, mx.Issue(api.Operator, "sec.example.com", map[string]any{"name": "ali", "hosts": []any{"auth.sec.example.com"}, "csr": newCSR(t)})).Data.(map[string]any)

	mustOK(t, base.d.Delete("auth.sec.example.com", false))
	if hosts := stringList(storedCert(t, mx, reply["fingerprint"].(string))["hosts"]); len(hosts) != 0 {
		t.Fatalf("hosts = %v", hosts)
	}

	mustOK(t, base.d.Delete("sec.example.com", false))
	if left := storedCert(t, mx, reply["fingerprint"].(string)); left != nil {
		t.Fatalf("cert = %v", left)
	}
}

func TestDomainListShowsPolicyOnlyWhenSet(t *testing.T) {
	base := newFixture(t)
	mx := NewMTLS(base.cfg, &fakeMTLSProxy{})
	mustOK(t, base.d.Add("plain.example.com", "myapp"))
	mustOK(t, base.d.Add("sec.example.org", "otherapp"))
	mustOK(t, mx.Set("sec.example.org", "optional", nil))

	raw, _ := json.Marshal(base.d.List(nil).Data)
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		_, has := row["mtls"]
		if want := row["domain"] == "sec.example.org"; has != want {
			t.Errorf("%v: has mtls = %v", row["domain"], has)
		}
	}
}

// A9: the dashboard cannot tell a managed CA from an external one by id, and
// only an external one lets a host be required with no certificate record.
func TestDomainListNamesExternalCAs(t *testing.T) {
	base := newFixture(t)
	mx := NewMTLS(base.cfg, &fakeMTLSProxy{})
	t.Cleanup(mx.Stop)
	mustOK(t, base.d.Add("sec.example.org", "otherapp"))
	mustOK(t, base.d.Add("auth.sec.example.org", "otherapp"))
	mustOK(t, base.d.Add("plain.example.com", "myapp"))
	mustOK(t, mx.Set("plain.example.com", "optional", nil))
	mustOK(t, mx.Set("sec.example.org", "optional", nil))
	managed := stringList(mx.recordFor(t, "sec.example.org")["ca"])[0]

	external := newExternalCA(t, true)
	mustOK(t, mx.Set("auth.sec.example.org", "required", map[string]any{"caPem": external}))
	block, _ := pem.Decode([]byte(external))
	externalID := mtls.Fingerprint(block.Bytes)[:16]
	// The record trusts both kinds at once.
	mustOK(t, mx.Set("sec.example.org", "optional", map[string]any{"ca": []any{managed, externalID}}))

	raw, _ := json.Marshal(base.d.List(nil).Data)
	var rows []struct {
		Domain string `json:"domain"`
		MTLS   struct {
			CA       []string `json:"ca"`
			External []string `json:"external"`
			Hosts    map[string]struct {
				CA       []string `json:"ca"`
				External []string `json:"external"`
			} `json:"hosts"`
		} `json:"mtls"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		switch row.Domain {
		case "sec.example.org":
			if !reflect.DeepEqual(row.MTLS.External, []string{externalID}) || len(row.MTLS.CA) != 2 {
				t.Errorf("record = %+v", row.MTLS)
			}
			if host := row.MTLS.Hosts["auth.sec.example.org"]; !reflect.DeepEqual(host.External, []string{externalID}) {
				t.Errorf("override = %+v", host)
			}
		case "plain.example.com":
			if len(row.MTLS.CA) != 1 || row.MTLS.External != nil {
				t.Errorf("a managed CA is reported external: %+v", row.MTLS)
			}
		}
	}
	if strings.Count(string(raw), `"external"`) != 2 {
		t.Fatalf("external must appear only where an external CA is trusted: %s", raw)
	}

	// Computed for the reply, never written back: the proxy payload and the
	// config file stay as they were.
	var stored map[string]any
	base.cfg.View(func() { stored = base.cfg.Map("domains")["sec.example.org"].(map[string]any)["mtls"].(map[string]any) })
	if _, leaked := stored["external"]; leaked {
		t.Fatal("external reached the stored record")
	}
}

func (m *MTLS) recordFor(t *testing.T, name string) map[string]any {
	t.Helper()
	var out map[string]any
	m.cfg.View(func() { out = recordConfig(m.domainsLocked(), name) })
	return out
}

func TestCleanHostNormalizes(t *testing.T) {
	host, msg := cleanHost(" HTTPS://WWW.Auth.X.com ")
	if msg != "" || host != "auth.x.com" {
		t.Fatalf("host = %q msg = %q", host, msg)
	}
}

func TestRevokeAcceptsAForeignFingerprint(t *testing.T) {
	fx := newMTLSFixture(t)
	fp := strings.Repeat("ab", 32)
	colons := strings.ToUpper(strings.TrimSuffix(strings.Repeat("ab:", 32), ":"))
	mustOK(t, fx.m.Revoke(api.Operator, colons))
	if fx.certRecord(t, fp)["revoked"] == nil {
		t.Fatalf("no revocation stored under %s", fp)
	}
	out, _ := fx.proxyPayload(t)
	if len(out.Revoked) != 1 || out.Revoked[0] != fp {
		t.Fatalf("revoked = %v", out.Revoked)
	}
}

func TestRecreatedDomainIsNotOpenedByOldCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "z.com", "ali", "z.com")

	fx.cfg.Mutate(func() { delete(fx.cfg.Map("domains"), "z.com") })
	fx.m.Prune()
	fx.cfg.Mutate(func() {
		fx.cfg.Map("domains")["z.com"] = map[string]any{"appId": "myapp", "subdomain": []any{"www"}}
	})
	mustOK(t, fx.m.Set("z.com", "optional", nil))

	payload, _ := fx.proxyPayload(t)
	if _, opened := payload.Certs[reply["fingerprint"].(string)]; opened {
		t.Fatal("a certificate from the domain's previous life still has a scope")
	}
}

func TestLogNameCannotForgeLines(t *testing.T) {
	if got := logSafeName("a\nb\r"); got != "ab" {
		t.Fatalf("got %q", got)
	}
}

func TestCertificatesStayOutOfTheConfigFile(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "auth.x.com", "ali", "auth.x.com")
	if err := fx.cfg.ForceSave(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(fx.base, "config", "mtls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), reply["fingerprint"].(string)) || strings.Contains(string(raw), "certs") {
		t.Fatalf("a config rewrite would cost every certificate again: %s", raw)
	}
}

func TestSnapshotCarriesWhatTheProxyNeeds(t *testing.T) {
	fx := newMTLSFixture(t)
	live := fx.issue(t, "auth.x.com", "live", "auth.x.com")["fingerprint"].(string)
	gone := fx.issue(t, "auth.x.com", "gone", "auth.x.com")["fingerprint"].(string)
	mustOK(t, fx.m.Revoke(api.Operator, gone))
	foreign := strings.Repeat("a", 64)
	mustOK(t, fx.m.Revoke(api.Operator, foreign))

	out, raw := fx.proxyPayload(t)
	if hosts := out.Certs[live]; len(hosts) != 1 || hosts[0] != "auth.x.com" || len(out.Certs) != 1 {
		t.Fatalf("certs = %v", out.Certs)
	}
	if len(out.Revoked) != 2 || !contains(out.Revoked, foreign) || !contains(out.Revoked, gone) {
		t.Fatalf("revoked = %v", out.Revoked)
	}
	for _, leaked := range []string{"spki", "renewedFrom", `"name"`} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("payload leaked %q: %s", leaked, raw)
		}
	}

	// Past its expiry the chain check refuses it, so the list stops carrying it.
	future := time.Now().Add(20 * 365 * 24 * time.Hour)
	certs, err := fx.m.CertSnapshot(future)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs.Revoked) != 1 || certs.Revoked[0] != foreign {
		t.Fatalf("an external revocation has no expiry to rely on: %v", certs.Revoked)
	}
}

func TestEmptySnapshotDecodes(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "auth.x.com", "ali", "auth.x.com")
	store, _ := fx.m.certs()
	if _, err := store.db.Exec("DELETE FROM certs"); err != nil {
		t.Fatal(err)
	}
	out, _ := fx.proxyPayload(t)
	if out.Certs == nil || len(out.Certs) != 0 || out.Revoked == nil {
		t.Fatalf("payload = %+v", out)
	}
}

func TestCertificateChangesUseTheNarrowPush(t *testing.T) {
	fx := newMTLSFixture(t)
	first := fx.issue(t, "auth.x.com", "ali", "auth.x.com")
	if fx.proxy.certSyncs != 0 {
		t.Fatal("a new CA changes a domain record, which only the full push carries")
	}
	fx.issue(t, "auth.x.com", "veli", "auth.x.com")
	mustOK(t, fx.m.Scope(first["fingerprint"], []any{"x.com"}))
	mustOK(t, fx.m.Revoke(api.Operator, first["fingerprint"]))
	if fx.proxy.certSyncs != 3 {
		t.Fatalf("narrow pushes = %d", fx.proxy.certSyncs)
	}
}

func TestStoreFailureIsReportedNotSwallowed(t *testing.T) {
	fx := newMTLSFixture(t)
	reply := fx.issue(t, "auth.x.com", "ali", "auth.x.com")
	store, _ := fx.m.certs()
	store.close()

	mustFail(t, fx.m.Revoke(api.Operator, reply["fingerprint"]), "unavailable")
	mustFail(t, fx.m.Scope(reply["fingerprint"], []any{"x.com"}), "unavailable")
	mustFail(t, fx.m.Issue(api.Operator, "auth.x.com", map[string]any{"name": "new", "hosts": []any{"auth.x.com"}, "csr": newCSR(t)}), "unavailable")
	mustFail(t, fx.m.List(api.Operator, nil), "unavailable")
	if _, err := fx.m.CertSnapshot(time.Now()); err == nil {
		t.Fatal("a failed read must reach the push")
	}
	fx.m.Stop()
	if fx.certRecord(t, reply["fingerprint"].(string)) == nil {
		t.Fatal("the store should reopen after a failure")
	}
}

func TestKeyAndNameLookupsUseAnIndex(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.issue(t, "auth.x.com", "seed", "auth.x.com")
	store, _ := fx.m.certs()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		fp := fmt.Sprintf("%064x", i)
		if _, err := tx.Exec("INSERT INTO certs (fingerprint, ca, name, spki, hosts) VALUES (?, 'other', ?, ?, '[]')", fp, fp, fp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var plan string
	for _, q := range []string{
		"SELECT 1 FROM certs WHERE ca = 'x' AND name = 'y' AND revoked IS NULL LIMIT 1",
		"SELECT fingerprint FROM certs WHERE ca = 'x' AND spki = 'y'",
	} {
		var id, parent, unused int
		if err := store.db.QueryRow("EXPLAIN QUERY PLAN "+q).Scan(&id, &parent, &unused, &plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, "USING INDEX") {
			t.Fatalf("%s scans the table: %s", q, plan)
		}
	}
}
