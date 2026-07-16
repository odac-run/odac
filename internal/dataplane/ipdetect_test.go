package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"odac/internal/logx"
)

func TestIsPrivateIPv4(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.1":      true,
		"100.64.0.1":    true, // CGNAT
		"100.128.0.1":   false,
		"127.0.0.1":     true,
		"169.254.10.10": true,
		"172.15.0.1":    false,
		"172.16.0.1":    true,
		"172.31.255.1":  true,
		"172.32.0.1":    false,
		"192.168.1.1":   true,
		"8.8.8.8":       false,
		"not.an.ip.x":   false,
	}
	for ip, want := range cases {
		if got := isPrivateIPv4(ip); got != want {
			t.Errorf("isPrivateIPv4(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestIsPrivateIPv6(t *testing.T) {
	cases := map[string]bool{
		"fe80::1":     true,
		"FC00::1":     true,
		"fd12::34":    true,
		"::1":         true,
		"2001:db8::1": false,
	}
	for ip, want := range cases {
		if got := isPrivateIPv6(ip); got != want {
			t.Errorf("isPrivateIPv6(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestDetectRun(t *testing.T) {
	det := newIPDetector()
	det.localAddrs = func() []localAddr {
		return []localAddr{
			{iface: "eth0", addr: "192.168.1.10"},
			{iface: "eth0", addr: "203.0.113.7"},       // public local v4
			{iface: "eth0", addr: "fe80::1", v6: true}, // link-local → skipped
			{iface: "eth0", addr: "2001:db8::5", v6: true},
		}
	}
	calls := []string{}
	det.httpGet = func(url string, ipv6 bool) (string, error) {
		calls = append(calls, url)
		switch url {
		case "https://curlmyip.org/":
			return "<html>oops</html>", nil // invalid → next service
		case "https://ipv4.icanhazip.com/":
			return " 5.6.7.8\n", nil
		case "https://ipv6.icanhazip.com/":
			return "", errors.New("network unreachable")
		case "https://api64.ipify.org/":
			return "2001:db8::99\n", nil
		}
		t.Fatalf("unexpected service call after a valid answer: %s", url)
		return "", nil
	}
	det.reverse = func(_ context.Context, addr string) ([]string, error) {
		if addr == "5.6.7.8" {
			return []string{"host.example.com."}, nil // trailing dot like LookupAddr
		}
		return nil, errors.New("NXDOMAIN")
	}
	det.ptrBudget = 200 * time.Millisecond

	log := logx.New("DNS")
	v4, v6, primary := det.run(log, nil, nil, "127.0.0.1")

	if primary != "5.6.7.8" {
		t.Errorf("primary = %q, want 5.6.7.8", primary)
	}
	if len(v4) != 3 || v4[0].Address != "5.6.7.8" || !v4[0].Public {
		t.Fatalf("v4 = %#v, want external unshifted first", v4)
	}
	if v4[0].PTR != "host.example.com" {
		t.Errorf("PTR = %q, want trailing dot stripped", v4[0].PTR)
	}
	if v4[1].Address != "192.168.1.10" || v4[1].Public || !v4[2].Public {
		t.Errorf("local v4 classification wrong: %#v", v4)
	}
	if len(v6) != 2 || v6[0].Address != "2001:db8::99" || v6[1].Address != "2001:db8::5" {
		t.Errorf("v6 = %#v (fe80 must be skipped, external first)", v6)
	}

	// Re-run with the previous set: everything dedupes, nothing grows.
	v4b, v6b, _ := det.run(log, v4, v6, primary)
	if len(v4b) != len(v4) || len(v6b) != len(v6) {
		t.Errorf("re-run grew the sets: v4 %d→%d, v6 %d→%d", len(v4), len(v4b), len(v6), len(v6b))
	}
}

func TestDetectRunAllServicesFail(t *testing.T) {
	det := newIPDetector()
	det.localAddrs = func() []localAddr { return nil }
	det.httpGet = func(string, bool) (string, error) { return "", errors.New("offline") }
	det.reverse = func(context.Context, string) ([]string, error) { return nil, errors.New("offline") }
	det.ptrBudget = 20 * time.Millisecond

	v4, v6, primary := det.run(logx.New("DNS"), nil, nil, "127.0.0.1")
	if len(v4) != 0 || len(v6) != 0 {
		t.Errorf("sets not empty: %#v %#v", v4, v6)
	}
	if primary != "127.0.0.1" {
		t.Errorf("primary = %q, want untouched 127.0.0.1", primary)
	}
}

func TestPTRBudgetCapsLookups(t *testing.T) {
	det := newIPDetector()
	det.ptrBudget = 30 * time.Millisecond
	det.reverse = func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done() // resolver hangs until the budget cancels it
		return nil, ctx.Err()
	}

	set := []IPEntry{{Address: "5.6.7.8"}}
	start := time.Now()
	det.lookupPTRs(logx.New("DNS"), set)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("lookupPTRs ignored its budget (%v)", elapsed)
	}
	if set[0].PTR != "" {
		t.Errorf("PTR = %q, want empty on timeout", set[0].PTR)
	}
}
