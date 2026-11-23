package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"odac/internal/proxy/config"
)

func newRateLimitedFirewall(t *testing.T, max int) (*Firewall, http.Handler) {
	t.Helper()
	f := NewFirewall(config.Firewall{
		Enabled:   true,
		RateLimit: config.RateLimit{Enabled: true, WindowMs: 60000, Max: max},
		Blacklist: []string{"198.51.100.0/24", "2001:db8:bad::/48", "192.0.2.7"},
	})
	t.Cleanup(func() { close(f.stopCleanup) })
	return f, f.Check(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}

func firewallStatus(h http.Handler, remote string) int {
	r := httptest.NewRequest(http.MethodGet, "http://x.test/", nil)
	r.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestFirewallCountsAnIPv6ClientByItsPrefix(t *testing.T) {
	_, h := newRateLimitedFirewall(t, 3)
	for i := range 3 {
		if code := firewallStatus(h, fmt.Sprintf("[2001:db8:1:2::%x]:1", i+1)); code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code := firewallStatus(h, "[2001:db8:1:2:ffff::1]:1"); code != http.StatusTooManyRequests {
		t.Fatalf("a fourth address in the same /64 got %d", code)
	}
	if code := firewallStatus(h, "[2001:db8:1:3::1]:1"); code != http.StatusOK {
		t.Fatalf("a neighbouring /64 got %d", code)
	}
}

func TestFullFirewallTableKeepsExistingCounters(t *testing.T) {
	f, h := newRateLimitedFirewall(t, 1)
	if code := firewallStatus(h, "203.0.113.5:1"); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := firewallStatus(h, "203.0.113.5:1"); code != http.StatusTooManyRequests {
		t.Fatal("limit not reached")
	}

	f.mu.Lock()
	for i := len(f.requestCounts); i < firewallMaxTracked; i++ {
		f.requestCounts[fmt.Sprintf("filler-%d", i)] = &requestRecord{count: 1, timestamp: 1 << 62}
	}
	f.mu.Unlock()
	if code := firewallStatus(h, "203.0.113.200:1"); code != http.StatusOK {
		t.Fatalf("an untracked client got %d", code)
	}
	if code := firewallStatus(h, "203.0.113.5:1"); code != http.StatusTooManyRequests {
		t.Fatal("a full table reset an existing client's counter")
	}
}

// A full client table once let every new client through uncounted, so one IPv6 /48
// (65,536 /64s) could fill it and then send without limit. New clients are counted by
// their block instead, which leaves other networks their own budget.
func TestFullFirewallTableCountsNewClientsByBlock(t *testing.T) {
	f, h := newRateLimitedFirewall(t, 2)
	f.mu.Lock()
	for i := len(f.requestCounts); i < firewallMaxTracked; i++ {
		f.requestCounts[fmt.Sprintf("filler-%d", i)] = &requestRecord{count: 1, timestamp: 1 << 62}
	}
	f.mu.Unlock()

	for i, remote := range []string{"[2001:db8:7:1::1]:1", "[2001:db8:7:2::1]:1"} {
		if code := firewallStatus(h, remote); code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code := firewallStatus(h, "[2001:db8:7:ffff::1]:1"); code != http.StatusTooManyRequests {
		t.Fatalf("a third /64 of a /48 over budget got %d", code)
	}
	for i, remote := range []string{"203.0.113.1:1", "203.0.113.2:1"} {
		if code := firewallStatus(h, remote); code != http.StatusOK {
			t.Fatalf("IPv4 request %d: %d", i, code)
		}
	}
	if code := firewallStatus(h, "203.0.113.3:1"); code != http.StatusTooManyRequests {
		t.Fatalf("a third address of a /24 over budget got %d", code)
	}
	for _, remote := range []string{"[2001:db8:8::1]:1", "198.18.0.1:1"} {
		if code := firewallStatus(h, remote); code != http.StatusOK {
			t.Fatalf("a client of another block got %d", code)
		}
	}

	f.mu.Lock()
	for i := len(f.blockCounts); i < firewallMaxTracked; i++ {
		f.blockCounts[fmt.Sprintf("filler-%d", i)] = &requestRecord{count: 1, timestamp: 1 << 62}
	}
	f.mu.Unlock()
	if code := firewallStatus(h, "[2001:db8:7:3::1]:1"); code != http.StatusTooManyRequests {
		t.Fatalf("a full block table forgot a counted block: %d", code)
	}
	if code := firewallStatus(h, "198.19.0.1:1"); code != http.StatusOK {
		t.Fatalf("a new block with both tables full got %d", code)
	}
}

func TestFirewallBlocklistMatchesCIDR(t *testing.T) {
	_, h := newRateLimitedFirewall(t, 100)
	for remote, want := range map[string]int{
		"198.51.100.77:1":         http.StatusForbidden,
		"[::ffff:198.51.100.8]:1": http.StatusForbidden,
		"[2001:db8:bad:1::1]:1":   http.StatusForbidden,
		"192.0.2.7:1":             http.StatusForbidden,
		"192.0.2.8:1":             http.StatusOK,
		"[2001:db8:bae::1]:1":     http.StatusOK,
	} {
		if code := firewallStatus(h, remote); code != want {
			t.Errorf("%s: %d, want %d", remote, code, want)
		}
	}
}
