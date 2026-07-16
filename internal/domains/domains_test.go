package domains

// Shared test harness: fakes for the DNS/SSL/Proxy/Mail seams plus a config
// fixture. The Domain suite ports test/server/Domain.test.js; the SSL suite
// ports test/server/SSL.test.js (ssl_test.go).

import (
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/logx"
)

func TestMain(m *testing.M) {
	logx.Stdout = io.Discard
	logx.Stderr = io.Discard
	os.Exit(m.Run())
}

// fakeDNS records Record/Delete calls (the jest DNS mock).
type fakeDNS struct {
	mu      sync.Mutex
	records []map[string]any
	deletes []map[string]any
	v4, v6  []dataplane.IPEntry
	primary string
}

func (f *fakeDNS) Record(args ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, args...)
}

func (f *fakeDNS) Delete(args ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, args...)
}

func (f *fakeDNS) IPInfo() ([]dataplane.IPEntry, []dataplane.IPEntry, string) {
	return f.v4, f.v6, f.primary
}

func (f *fakeDNS) recorded() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.records...)
}

func (f *fakeDNS) deleted() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.deletes...)
}

// fakeProxy records syncs and ACME challenge pushes.
type fakeProxy struct {
	mu       sync.Mutex
	syncs    int
	setCalls [][2]string // token, keyAuthorization
	delCalls []string    // token
	setErr   error
}

func (f *fakeProxy) SyncConfig() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
}

func (f *fakeProxy) SetACMEChallenge(token, keyAuthorization string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, [2]string{token, keyAuthorization})
	return nil
}

func (f *fakeProxy) DeleteACMEChallenge(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls = append(f.delCalls, token)
}

func (f *fakeProxy) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs
}

// fakeMail records TLS cache clears.
type fakeMail struct {
	mu     sync.Mutex
	clears []string
}

func (f *fakeMail) ClearSSLCache(domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears = append(f.clears, domain)
}

// fakeRenewer records SSL.renew calls (the jest SSL mock in Domain tests).
type fakeRenewer struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRenewer) Renew(domain any) api.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := domain.(string)
	f.calls = append(f.calls, s)
	return api.Res(true, nil)
}

func (f *fakeRenewer) renewed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fixture bundles a Domain manager over an isolated config store.
type fixture struct {
	cfg   *config.Store
	dns   *fakeDNS
	renew *fakeRenewer
	proxy *fakeProxy
	mail  *fakeMail
	d     *Domain
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{
		cfg:   cfg,
		dns:   &fakeDNS{primary: "1.2.3.4", v6: []dataplane.IPEntry{{Address: "2001:db8::1", Public: true}}},
		renew: &fakeRenewer{},
		proxy: &fakeProxy{},
		mail:  &fakeMail{},
	}
	fx.d = NewDomain(cfg, fx.dns, fx.renew, fx.proxy, fx.mail)

	// The jest fixture's two apps.
	cfg.Mutate(func() {
		cfg.Set("apps", []any{
			map[string]any{"id": "app-1", "name": "myapp"},
			map[string]any{"id": "app-2", "name": "otherapp"},
		})
		cfg.Set("domains", map[string]any{})
	})
	return fx
}

func (fx *fixture) setDomains(domains map[string]any) {
	fx.cfg.Mutate(func() {
		fx.cfg.Set("domains", domains)
	})
}

func (fx *fixture) domain(name string) map[string]any {
	var out map[string]any
	fx.cfg.View(func() {
		domains, _ := fx.cfg.Get("domains").(map[string]any)
		rec, _ := domains[name].(map[string]any)
		if rec != nil {
			out = copyShallow(rec)
		}
	})
	return out
}

func nowMs() float64 { return float64(time.Now().UnixMilli()) }
