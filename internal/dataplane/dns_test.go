package dataplane

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func newTestDNS(t *testing.T, cs *controlServer) (*DNS, *fakeProc) {
	t.Helper()
	cfg := newStore(t)
	d := NewDNS(cfg, t.TempDir())
	fp := &fakeProc{running: true, socket: cs.sock}
	d.proc = fp
	d.retryDelay = 20 * time.Millisecond
	d.readyPoll = 10 * time.Millisecond
	return d, fp
}

func TestDNSPayload(t *testing.T) {
	cs := newControlServer(t)
	d, _ := newTestDNS(t, cs)

	zones := map[string]any{
		"example.com": map[string]any{
			"soa": map[string]any{"email": "hostmaster.example.com", "serial": float64(2026070701), "ttl": float64(3600)},
			"records": []any{
				map[string]any{"id": "u1", "name": "example.com", "type": "A", "value": "", "ttl": float64(3600)},
			},
		},
	}
	d.cfg.Set("dns", zones)
	d.mu.Lock()
	d.ipv4 = []IPEntry{{Address: "5.6.7.8", PTR: "host.example.com", Public: true}, {Address: "10.0.0.5"}}
	d.ipv6 = []IPEntry{{Address: "2001:db8::1", Public: true}}
	d.primary = "5.6.7.8"
	d.mu.Unlock()

	d.SyncConfig()
	payload := cs.nextConfig(t)

	if !reflect.DeepEqual(payload["zones"], zones) {
		t.Errorf("zones not sent verbatim:\n%#v\nwant\n%#v", payload["zones"], zones)
	}
	wantIPs := map[string]any{
		"ipv4": []any{
			map[string]any{"address": "5.6.7.8", "ptr": "host.example.com", "public": true},
			map[string]any{"address": "10.0.0.5", "ptr": "", "public": false},
		},
		"ipv6": []any{
			map[string]any{"address": "2001:db8::1", "ptr": "", "public": true},
		},
		"primary": "5.6.7.8",
	}
	if !reflect.DeepEqual(payload["ips"], wantIPs) {
		t.Errorf("ips =\n%#v\nwant\n%#v", payload["ips"], wantIPs)
	}
}

func TestDNSPayloadDefaults(t *testing.T) {
	cs := newControlServer(t)
	d, _ := newTestDNS(t, cs)

	d.SyncConfig()
	payload := cs.nextConfig(t)

	// Default config: dns module {} → zones {}; nothing detected yet →
	// empty arrays (not null) and the 127.0.0.1 primary.
	if !reflect.DeepEqual(payload["zones"], map[string]any{}) {
		t.Errorf("zones = %#v, want {}", payload["zones"])
	}
	wantIPs := map[string]any{"ipv4": []any{}, "ipv6": []any{}, "primary": "127.0.0.1"}
	if !reflect.DeepEqual(payload["ips"], wantIPs) {
		t.Errorf("ips = %#v, want %#v", payload["ips"], wantIPs)
	}
}

// stubDetector wires canned data into the DNS service's detector.
func stubDetector(d *DNS, delay time.Duration) {
	d.detect.localAddrs = func() []localAddr {
		return []localAddr{{iface: "eth0", addr: "192.168.1.10"}}
	}
	d.detect.httpGet = func(url string, ipv6 bool) (string, error) {
		time.Sleep(delay)
		if ipv6 {
			return "2001:db8::99\n", nil
		}
		return "5.6.7.8\n", nil
	}
	d.detect.reverse = func(context.Context, string) ([]string, error) {
		return nil, context.Canceled
	}
	d.detect.ptrBudget = 50 * time.Millisecond
}

func TestDNSStartDetectsThenSpawns(t *testing.T) {
	cs := newControlServer(t)
	d, fp := newTestDNS(t, cs)
	fp.running = false
	stubDetector(d, 0)

	d.Start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e, _ := fp.counts(); e == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Ensure never called after detection")
		}
		time.Sleep(5 * time.Millisecond)
	}

	v4, v6, primary := d.IPInfo()
	if primary != "5.6.7.8" {
		t.Errorf("primary = %q, want external 5.6.7.8", primary)
	}
	// External address unshifted to the front, local one behind it.
	if len(v4) != 2 || v4[0].Address != "5.6.7.8" || !v4[0].Public || v4[1].Address != "192.168.1.10" || v4[1].Public {
		t.Errorf("ipv4 = %#v", v4)
	}
	if len(v6) != 1 || v6[0].Address != "2001:db8::99" {
		t.Errorf("ipv6 = %#v", v6)
	}

	// Second Start while active is a no-op (Node's #active guard).
	d.Start()
	time.Sleep(50 * time.Millisecond)
	if e, _ := fp.counts(); e != 1 {
		t.Errorf("second Start re-ran detection/spawn (%d ensures)", e)
	}
}

func TestDNSStopDuringDetectionPreventsSpawn(t *testing.T) {
	cs := newControlServer(t)
	d, fp := newTestDNS(t, cs)
	fp.running = false
	stubDetector(d, 40*time.Millisecond) // detection slow enough to Stop first

	d.Start()
	d.Stop()

	time.Sleep(500 * time.Millisecond) // let detection finish
	if e, _ := fp.counts(); e != 0 {
		t.Errorf("spawned despite Stop during detection (%d ensures)", e)
	}
}
