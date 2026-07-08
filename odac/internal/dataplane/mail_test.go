package dataplane

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func newTestMail(t *testing.T, cs *controlServer, dns DNSService) (*Mail, *fakeProc) {
	t.Helper()
	cfg := newStore(t)
	m := NewMail(cfg, t.TempDir(), dns)
	fp := &fakeProc{running: true, socket: cs.sock}
	m.proc = fp
	m.retryDelay = 20 * time.Millisecond
	return m, fp
}

func TestMailPayload(t *testing.T) {
	cs := newControlServer(t)
	src := &fakeIPSource{
		v4:      []IPEntry{{Address: "5.6.7.8", PTR: "mx.example.com", Public: true}},
		primary: "5.6.7.8",
	}
	m, _ := newTestMail(t, cs, src)

	m.cfg.Set("domains", map[string]any{
		"example.com": map[string]any{
			"subdomain": []any{"mail"},
			"cert": map[string]any{
				"ssl":  map[string]any{"key": "k", "cert": "c"},
				"dkim": map[string]any{"private": "p", "public": "q", "selector": "default"},
			},
		},
		"nomx.test": map[string]any{"cert": false},
	})
	m.cfg.Set("dns", map[string]any{
		"example.com": map[string]any{
			"records": []any{
				map[string]any{"type": "MX", "name": "example.com", "value": "mail.example.com"},
			},
		},
		// nomx.test has no zone at all
	})

	m.SyncConfig()
	payload := cs.nextConfig(t)

	wantDomains := map[string]any{
		"example.com": map[string]any{
			"cert": map[string]any{
				"ssl":  map[string]any{"key": "k", "cert": "c"},
				"dkim": map[string]any{"private": "p", "public": "q", "selector": "default"},
			},
			"mxEnabled":  true,
			"subdomains": []any{"mail"},
		},
		"nomx.test": map[string]any{
			"cert":       map[string]any{}, // cert:false → {}
			"mxEnabled":  false,
			"subdomains": []any{},
		},
	}
	if !reflect.DeepEqual(payload["domains"], wantDomains) {
		t.Errorf("domains =\n%#v\nwant\n%#v", payload["domains"], wantDomains)
	}

	hostname, _ := os.Hostname()
	if payload["hostname"] != hostname {
		t.Errorf("hostname = %v, want %q", payload["hostname"], hostname)
	}
	if !reflect.DeepEqual(payload["accounts"], []any{}) {
		t.Errorf("accounts = %v, want []", payload["accounts"])
	}
	wantIPs := map[string]any{
		"ipv4":    []any{map[string]any{"address": "5.6.7.8", "ptr": "mx.example.com", "public": true}},
		"ipv6":    []any{},
		"primary": "5.6.7.8",
	}
	if !reflect.DeepEqual(payload["ips"], wantIPs) {
		t.Errorf("ips = %#v, want %#v", payload["ips"], wantIPs)
	}
	if !reflect.DeepEqual(payload["ssl"], map[string]any{}) {
		t.Errorf("ssl = %v, want {} (config.ssl || {})", payload["ssl"])
	}
}

func TestMailPayloadWithoutIPSource(t *testing.T) {
	cs := newControlServer(t)
	m, _ := newTestMail(t, cs, nil)

	m.SyncConfig()
	payload := cs.nextConfig(t)

	wantIPs := map[string]any{"ipv4": []any{}, "ipv6": []any{}, "primary": "127.0.0.1"}
	if !reflect.DeepEqual(payload["ips"], wantIPs) {
		t.Errorf("ips = %#v, want fallback %#v", payload["ips"], wantIPs)
	}
}

func TestMailLifecycle(t *testing.T) {
	cs := newControlServer(t)
	m, fp := newTestMail(t, cs, nil)
	fp.running = false

	m.Check()
	if e, _ := fp.counts(); e != 0 {
		t.Errorf("Check before Start ensured %d times", e)
	}

	m.Start()
	m.Start() // #active guard: second Start is a no-op
	m.Check()
	if e, _ := fp.counts(); e != 2 {
		t.Errorf("ensures = %d, want 2 (first Start + Check)", e)
	}

	m.Stop()
	if _, s := fp.counts(); s != 1 {
		t.Errorf("stops = %d, want 1", s)
	}
	m.Check()
	if e, _ := fp.counts(); e != 2 {
		t.Errorf("Check after Stop respawned (%d ensures)", e)
	}
}
