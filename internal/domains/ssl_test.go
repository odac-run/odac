package domains

// Port of test/server/SSL.test.js plus spec tests for the paths jest left
// uncovered (backoff schedule, SAN mismatch trigger, wildcard SAN math,
// self-signed reuse, deleted-domain save guard).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrderer scripts Acme.order() like the jest mockOrderFn.
type fakeOrderer struct {
	mu      sync.Mutex
	calls   []orderOpts
	handler func(call int, o orderOpts) (string, error)
}

func (f *fakeOrderer) Order(o orderOpts) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, o)
	n := len(f.calls)
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		return handler(n, o)
	}
	return "mock-certificate", nil
}

func (f *fakeOrderer) callList() []orderOpts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]orderOpts(nil), f.calls...)
}

type sslFixture struct {
	*fixture
	s           *SSL
	orderer     *fakeOrderer
	clientCalls int
	clientMu    sync.Mutex
}

func newSSLFixture(t *testing.T) *sslFixture {
	t.Helper()
	fx := &sslFixture{fixture: newFixture(t), orderer: &fakeOrderer{}}
	fx.s = NewSSL(fx.cfg, fx.dns, fx.proxy, fx.mail)
	fx.s.newClient = func() (acmeOrderer, error) {
		fx.clientMu.Lock()
		fx.clientCalls++
		fx.clientMu.Unlock()
		return fx.orderer, nil
	}
	return fx
}

func (fx *sslFixture) waitIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fx.s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("background SSL work never settled")
	}
}

func (fx *sslFixture) clients() int {
	fx.clientMu.Lock()
	defer fx.clientMu.Unlock()
	return fx.clientCalls
}

// writeCert writes a real certificate with the given SANs where the SAN
// check will read it, and returns its path.
func writeCert(t *testing.T, dir, name string, dnsNames []string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func certRecord(expiry float64, keyPath, certPath string, subdomains ...any) map[string]any {
	ssl := map[string]any{"expiry": expiry}
	if keyPath != "" {
		ssl["key"] = keyPath
	}
	if certPath != "" {
		ssl["cert"] = certPath
	}
	return map[string]any{
		"appId":     "myapp",
		"subdomain": subdomains,
		"cert":      map[string]any{"ssl": ssl},
	}
}

// ─── check() ────────────────────────────────────────────────────────────

func TestCheckWritesKeysOwnerOnly(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{
		"expired.com": certRecord(nowMs()-100000, "", ""),
	})

	fx.s.Check()
	fx.waitIdle(t)

	// Both the self-signed bootstrap key and the per-domain ACME key must be
	// written owner-only so a sibling process/UID cannot read them.
	for _, name := range []string{"odac.key", "expired.com.key"} {
		info, err := os.Stat(filepath.Join(fx.s.certDir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
	if info, err := os.Stat(fx.s.certDir); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("certDir mode = %o, want 700", perm)
	}
}

func TestHardenCertDirTightensExistingKeys(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "cert", "ssl")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate keys left world-readable by an earlier build.
	keyFile := filepath.Join(certDir, "old.example.com.key")
	crtFile := filepath.Join(certDir, "old.example.com.crt")
	if err := os.WriteFile(keyFile, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crtFile, []byte("crt"), 0o644); err != nil {
		t.Fatal(err)
	}

	hardenCertDir(certDir)

	if info, _ := os.Stat(keyFile); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %o, want 600", info.Mode().Perm())
	}
	if info, _ := os.Stat(certDir); info.Mode().Perm() != 0o700 {
		t.Errorf("certDir mode = %o, want 700", info.Mode().Perm())
	}
	// Public cert files are left untouched.
	if info, _ := os.Stat(crtFile); info.Mode().Perm() != 0o644 {
		t.Errorf("cert mode = %o, want 644 (unchanged)", info.Mode().Perm())
	}
}

func TestCheckRenewsExpiredCertificates(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{
		"expired.com": certRecord(nowMs()-100000, "", ""),
	})

	fx.s.Check()
	fx.waitIdle(t)

	calls := fx.orderer.callList()
	if len(calls) == 0 {
		t.Fatal("no ACME order fired")
	}
	if calls[0].Domains[0] != "expired.com" {
		t.Fatalf("order domains = %v", calls[0].Domains)
	}

	// Certificate saved to <certDir>/expired.com.crt.
	raw, err := os.ReadFile(filepath.Join(fx.s.certDir, "expired.com.crt"))
	if err != nil || string(raw) != "mock-certificate" {
		t.Fatalf("saved cert = %q, %v", raw, err)
	}

	// Config expiry pushed into the future.
	rec := fx.domain("expired.com")
	cert := rec["cert"].(map[string]any)
	ssl := cert["ssl"].(map[string]any)
	if exp, _ := ssl["expiry"].(float64); exp <= nowMs() {
		t.Fatalf("expiry = %v", ssl["expiry"])
	}

	// The self-signed bootstrap cert was ensured on the same pass.
	if !fileExists(filepath.Join(fx.s.certDir, "odac.crt")) {
		t.Fatal("self-signed cert missing")
	}
	var selfExpiry float64
	fx.cfg.View(func() {
		ssl, _ := fx.cfg.Get("ssl").(map[string]any)
		selfExpiry, _ = ssl["expiry"].(float64)
	})
	if selfExpiry <= nowMs() {
		t.Fatalf("config.ssl.expiry = %v", selfExpiry)
	}

	// Mail cache cleared + proxy synced after the save.
	if len(fx.mail.clears) == 0 || fx.proxy.syncCount() == 0 {
		t.Fatal("mail/proxy not notified after cert save")
	}
}

func TestCheckSkipsValidCertificates(t *testing.T) {
	fx := newSSLFixture(t)
	dir := t.TempDir()
	valid := nowMs() + 1000*60*60*24*40 // > 30 days

	exCrt := writeCert(t, dir, "example.com.crt", []string{"example.com", "www.example.com"})
	expCrt := writeCert(t, dir, "expired.com.crt", []string{"expired.com"})
	fx.setDomains(map[string]any{
		"example.com": certRecord(valid, exCrt, exCrt, "www"),
		"expired.com": certRecord(valid, expCrt, expCrt),
	})

	fx.s.Check()
	fx.waitIdle(t)

	if fx.clients() != 0 {
		t.Fatalf("ACME client created %d times", fx.clients())
	}
}

func TestCheckSkipsOptedOutDomains(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{
		"optout.com": map[string]any{"appId": "myapp", "cert": false},
	})

	fx.s.Check()
	fx.waitIdle(t)

	if fx.clients() != 0 {
		t.Fatal("cert === false domain was renewed")
	}
}

func TestCheckSANMismatchTriggersRenewal(t *testing.T) {
	fx := newSSLFixture(t)
	dir := t.TempDir()
	valid := nowMs() + 1000*60*60*24*40

	// Cert covers only the apex; the record demands www too.
	crt := writeCert(t, dir, "example.com.crt", []string{"example.com"})
	fx.setDomains(map[string]any{
		"example.com": certRecord(valid, crt, crt, "www"),
	})

	fx.s.Check()
	fx.waitIdle(t)

	calls := fx.orderer.callList()
	if len(calls) != 1 {
		t.Fatalf("order calls = %d", len(calls))
	}
	want := []string{"example.com", "www.example.com"}
	if len(calls[0].Domains) != 2 || calls[0].Domains[0] != want[0] || calls[0].Domains[1] != want[1] {
		t.Fatalf("order domains = %v", calls[0].Domains)
	}
}

func TestCheckOverlappingTicksSkip(t *testing.T) {
	fx := newSSLFixture(t)
	block := make(chan struct{})
	fx.orderer.handler = func(_ int, _ orderOpts) (string, error) {
		<-block
		return "mock-certificate", nil
	}
	fx.setDomains(map[string]any{
		"expired.com": certRecord(nowMs()-100000, "", ""),
	})

	fx.s.Check()
	// Wait until the first scan claims the domain.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fx.s.mu.Lock()
		busy := len(fx.s.processing) > 0
		fx.s.mu.Unlock()
		if busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	fx.s.Check() // overlapping tick: checking/processing guard skips
	close(block)
	fx.waitIdle(t)

	if len(fx.orderer.callList()) != 1 {
		t.Fatalf("order calls = %d", len(fx.orderer.callList()))
	}
}

// ─── renew() ────────────────────────────────────────────────────────────

func TestRenewValidDomain(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{
		"example.com": certRecord(nowMs()+1000*60*60*24*40, "", "", "www"),
	})

	r := fx.s.Renew("example.com")
	if !r.Status || !strings.Contains(msgOf(t, r.Message), "renewed successfully") {
		t.Fatalf("r = %+v", r)
	}
	fx.waitIdle(t)

	calls := fx.orderer.callList()
	if len(calls) != 1 {
		t.Fatalf("order calls = %d", len(calls))
	}
	// HTTP-01 is the primary challenge.
	if calls[0].ChallengeType != "http-01" {
		t.Fatalf("challenge type = %s", calls[0].ChallengeType)
	}
	// SAN list covers the subdomain.
	if len(calls[0].Domains) != 2 || calls[0].Domains[1] != "www.example.com" {
		t.Fatalf("domains = %v", calls[0].Domains)
	}
}

func TestRenewSubdomainRenewsParent(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{
		"example.com": certRecord(nowMs()+1000, "", "", "www"),
	})

	r := fx.s.Renew("www.example.com")
	if !r.Status {
		t.Fatalf("r = %+v", r)
	}
	fx.waitIdle(t)

	if !fileExists(filepath.Join(fx.s.certDir, "example.com.crt")) {
		t.Fatal("parent cert not saved")
	}
	if fileExists(filepath.Join(fx.s.certDir, "www.example.com.crt")) {
		t.Fatal("subdomain saved as its own cert")
	}
}

func TestRenewFallsBackToDNS01(t *testing.T) {
	fx := newSSLFixture(t)
	fx.orderer.handler = func(call int, o orderOpts) (string, error) {
		if o.ChallengeType == "http-01" {
			return "", errors.New("HTTP-01 challenge validation failed")
		}
		return "mock-certificate-dns", nil
	}
	fx.setDomains(map[string]any{
		"expired.com": certRecord(nowMs()-100000, "", ""),
	})

	fx.s.Check()
	fx.waitIdle(t)

	calls := fx.orderer.callList()
	if len(calls) != 2 || calls[0].ChallengeType != "http-01" || calls[1].ChallengeType != "dns-01" {
		t.Fatalf("calls = %+v", calls)
	}
	raw, err := os.ReadFile(filepath.Join(fx.s.certDir, "expired.com.crt"))
	if err != nil || string(raw) != "mock-certificate-dns" {
		t.Fatalf("saved cert = %q, %v", raw, err)
	}
}

func TestHTTP01ChallengeCallbacksUseProxy(t *testing.T) {
	fx := newSSLFixture(t)
	fx.orderer.handler = func(_ int, o orderOpts) (string, error) {
		if err := o.ChallengeCreate("example.com", "http-01", "test-token-abc123", "test-key-authorization"); err != nil {
			return "", err
		}
		o.ChallengeRemove("example.com", "http-01", "test-token-abc123", "test-key-authorization")
		return "mock-certificate", nil
	}
	fx.setDomains(map[string]any{"example.com": certRecord(nowMs()+1000, "", "")})

	fx.s.Renew("example.com")
	fx.waitIdle(t)

	fx.proxy.mu.Lock()
	defer fx.proxy.mu.Unlock()
	if len(fx.proxy.setCalls) != 1 || fx.proxy.setCalls[0] != [2]string{"test-token-abc123", "test-key-authorization"} {
		t.Fatalf("setACMEChallenge calls = %v", fx.proxy.setCalls)
	}
	if len(fx.proxy.delCalls) != 1 || fx.proxy.delCalls[0] != "test-token-abc123" {
		t.Fatalf("deleteACMEChallenge calls = %v", fx.proxy.delCalls)
	}
	// The DNS zone table must stay untouched on the http-01 path.
	if len(fx.dns.recorded()) != 0 || len(fx.dns.deleted()) != 0 {
		t.Fatal("DNS records touched during http-01")
	}
}

func TestDNS01ChallengeCallbacksUseZoneTable(t *testing.T) {
	fx := newSSLFixture(t)
	fx.orderer.handler = func(_ int, o orderOpts) (string, error) {
		if o.ChallengeType == "http-01" {
			return "", errors.New("HTTP-01 failed")
		}
		if err := o.ChallengeCreate("example.com", "dns-01", "tok", "dns-key-auth"); err != nil {
			return "", err
		}
		o.ChallengeRemove("example.com", "dns-01", "tok", "dns-key-auth")
		return "mock-cert", nil
	}
	fx.setDomains(map[string]any{"example.com": certRecord(nowMs()+1000, "", "")})

	fx.s.Renew("example.com")
	fx.waitIdle(t)

	recs := fx.dns.recorded()
	if len(recs) != 1 {
		t.Fatalf("dns records = %v", recs)
	}
	want := map[string]any{"name": "_acme-challenge.example.com", "type": "TXT",
		"value": "dns-key-auth", "ttl": float64(100), "unique": true}
	for k, v := range want {
		if recs[0][k] != v {
			t.Fatalf("record[%s] = %v, want %v", k, recs[0][k], v)
		}
	}
	dels := fx.dns.deleted()
	if len(dels) != 1 || dels[0]["name"] != "_acme-challenge.example.com" || dels[0]["value"] != "dns-key-auth" {
		t.Fatalf("dns deletes = %v", dels)
	}
	fx.proxy.mu.Lock()
	defer fx.proxy.mu.Unlock()
	if len(fx.proxy.setCalls) != 0 {
		t.Fatal("proxy touched during dns-01")
	}
}

func TestWildcardOrdersSkipHTTP01(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{"*.example.com": certRecord(nowMs()+1000, "", "")})

	fx.s.Renew("*.example.com")
	fx.waitIdle(t)

	calls := fx.orderer.callList()
	if len(calls) != 1 || calls[0].ChallengeType != "dns-01" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRenewUnknownDomain(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{"example.com": certRecord(nowMs()+1000, "", "")})

	r := fx.s.Renew("unknown.com")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "Domain unknown.com not found") {
		t.Fatalf("r = %+v", r)
	}
	fx.waitIdle(t)
	if fx.clients() != 0 {
		t.Fatal("ACME client created for unknown domain")
	}
}

func TestRenewRejectsIPAddresses(t *testing.T) {
	fx := newSSLFixture(t)

	r := fx.s.Renew("1.2.3.4")
	if r.Status || !strings.Contains(msgOf(t, r.Message), "SSL renewal is not available for IP addresses") {
		t.Fatalf("r = %+v", r)
	}
}

// ─── cancellation ───────────────────────────────────────────────────────

func TestCancellationDiscardsStaleCertificate(t *testing.T) {
	fx := newSSLFixture(t)
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	fx.orderer.handler = func(call int, _ orderOpts) (string, error) {
		if call == 1 {
			close(firstStarted)
			<-release
			return "mock-certificate-stale", nil
		}
		return "mock-certificate-v2", nil
	}
	fx.setDomains(map[string]any{"expired.com": certRecord(nowMs()-100000, "", "")})

	fx.s.Renew("expired.com")
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first order never started")
	}

	// Second renew for the same domain cancels the first and queues.
	fx.s.Renew("expired.com")
	// Wait for the cancellation to land before releasing the stale order.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fx.s.mu.Lock()
		queued := fx.s.queued["expired.com"]
		fx.s.mu.Unlock()
		if queued || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	fx.waitIdle(t)

	raw, err := os.ReadFile(filepath.Join(fx.s.certDir, "expired.com.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "mock-certificate-v2" {
		t.Fatalf("saved cert = %q (stale certificate not discarded)", raw)
	}
}

// ─── error backoff ──────────────────────────────────────────────────────

func TestErrorBackoffSchedule(t *testing.T) {
	fx := newSSLFixture(t)
	fx.orderer.handler = func(_ int, _ orderOpts) (string, error) {
		return "", errors.New("boom")
	}
	fx.setDomains(map[string]any{"expired.com": certRecord(nowMs()-100000, "", "")})

	clock := time.Now()
	fx.s.now = func() time.Time { return clock }

	// Attempt while backed off must not even create a client.
	expect := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, backoff := range expect {
		fx.s.ssl("expired.com")
		fx.s.mu.Lock()
		st := fx.s.checked["expired.com"]
		fx.s.mu.Unlock()
		if st == nil || st.errorCount != i+1 {
			t.Fatalf("attempt %d: state = %+v", i+1, st)
		}
		if got := st.interval.Sub(clock); got != backoff {
			t.Fatalf("attempt %d: backoff = %v, want %v", i+1, got, backoff)
		}

		// Retrying inside the window is a no-op.
		before := fx.clients()
		fx.s.ssl("expired.com")
		if fx.clients() != before {
			t.Fatalf("attempt %d: retried inside the backoff window", i+1)
		}

		clock = clock.Add(backoff + time.Second)
	}
}

// ─── SAN math + save guard ──────────────────────────────────────────────

func TestExpectedNamesWildcardCoverage(t *testing.T) {
	got := expectedNames("example.com", []any{"*", "www", "mail", "deep.internal"})
	want := []string{"example.com", "*.example.com", "deep.internal.example.com"}
	if len(got) != len(want) {
		t.Fatalf("names = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
}

func TestSaveCertificateSkipsDeletedDomain(t *testing.T) {
	fx := newSSLFixture(t)
	fx.orderer.handler = func(_ int, _ orderOpts) (string, error) {
		// The domain vanishes while the order is in flight.
		fx.setDomains(map[string]any{})
		return "mock-certificate", nil
	}
	fx.setDomains(map[string]any{"gone.com": certRecord(nowMs()-100000, "", "")})

	fx.s.Renew("gone.com")
	fx.waitIdle(t)

	if len(fx.mail.clears) != 0 {
		t.Fatal("mail cache cleared for a deleted domain")
	}
	if fx.domain("gone.com") != nil {
		t.Fatal("record resurrected")
	}
}

// ─── self-signed bootstrap ──────────────────────────────────────────────

func TestSelfSignedGenerationAndReuse(t *testing.T) {
	fx := newSSLFixture(t)
	fx.setDomains(map[string]any{})

	fx.s.Check()
	fx.waitIdle(t)

	keyPath := filepath.Join(fx.s.certDir, "odac.key")
	crtPath := filepath.Join(fx.s.certDir, "odac.crt")
	raw, err := os.ReadFile(crtPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "Odac" {
		t.Fatalf("CN = %s", cert.Subject.CommonName)
	}
	if !fileExists(keyPath) {
		t.Fatal("key missing")
	}

	var expiry float64
	fx.cfg.View(func() {
		ssl, _ := fx.cfg.Get("ssl").(map[string]any)
		expiry, _ = ssl["expiry"].(float64)
	})
	// Node quirk kept: config expiry is 1 day even on a 365-day cert.
	if expiry <= nowMs() || expiry > nowMs()+86400000+60000 {
		t.Fatalf("expiry = %v", expiry)
	}

	// Second pass reuses the valid pair (no rewrite).
	before, _ := os.Stat(crtPath)
	fx.s.Check()
	fx.waitIdle(t)
	after, _ := os.Stat(crtPath)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("self-signed cert regenerated while still valid")
	}
}

func TestAcmeDirectoryEnvOverride(t *testing.T) {
	t.Setenv("ODAC_ACME_URL", "https://acme-staging-v02.api.letsencrypt.org/directory")
	if got := acmeDirectory(); got != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Fatalf("acmeDirectory() = %q, want the env override", got)
	}

	t.Setenv("ODAC_ACME_URL", "")
	if got := acmeDirectory(); got != letsEncryptURL {
		t.Fatalf("acmeDirectory() = %q, want production default", got)
	}
}
