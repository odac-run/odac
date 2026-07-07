package dataplane

import (
	"net"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, cs *controlServer, resolver ContainerIPs) (*Proxy, *fakeProc) {
	t.Helper()
	cfg := newStore(t)
	p := NewProxy(cfg, t.TempDir(), resolver)
	fp := &fakeProc{running: true, socket: cs.sock}
	p.proc = fp
	p.retryDelay = 20 * time.Millisecond
	p.readyPoll = 10 * time.Millisecond
	p.dockerWait = 200 * time.Millisecond
	p.dockerPoll = 10 * time.Millisecond
	return p, fp
}

func seedApps(p *Proxy) {
	p.cfg.Set("apps", []any{
		map[string]any{"name": "hostapp", "id": "a1", "ports": []any{map[string]any{"host": "3000"}}},
		map[string]any{"name": "ctrapp", "id": "a2", "ports": []any{map[string]any{"container": float64(8080)}}, "activeContainerId": "ctr-123"},
		map[string]any{"name": "portapp", "id": "a3", "port": float64(4000)},
		map[string]any{"name": "httpapp", "id": "a4", "http": "5000", "ip": "172.17.0.9"},
		map[string]any{"name": "noport", "id": "a5"},
	})
	p.cfg.Set("domains", map[string]any{
		"host.test":    map[string]any{"appId": "hostapp", "subdomain": []any{"www"}, "cert": map[string]any{"ssl": map[string]any{"key": "k", "cert": "c"}}},
		"ctr.test":     map[string]any{"appId": "a2"},
		"port.test":    map[string]any{"appId": "portapp", "cert": false},
		"http.test":    map[string]any{"appId": "httpapp"},
		"missing.test": map[string]any{"appId": "ghost"},
		"noport.test":  map[string]any{"appId": "noport"},
	})
}

func TestProxyPayloadAssembly(t *testing.T) {
	cs := newControlServer(t)
	resolver := &fakeResolver{ips: map[string]string{"ctr-123": "10.5.0.2"}}
	resolver.available.Store(true)
	p, _ := newTestProxy(t, cs, resolver)
	seedApps(p)

	p.SyncConfig()
	payload := cs.nextConfig(t)

	domains, _ := payload["domains"].(map[string]any)
	want := map[string]map[string]any{
		"host.test": {
			"domain": "host.test", "port": float64(3000), "containerIP": "127.0.0.1",
			"subdomain": []any{"www"},
			"cert":      map[string]any{"ssl": map[string]any{"key": "k", "cert": "c"}},
		},
		"ctr.test": {
			"domain": "ctr.test", "port": float64(8080), "containerIP": "10.5.0.2", "container": "10.5.0.2",
			"subdomain": []any{}, "cert": map[string]any{},
		},
		"port.test": { // cert:false → {} (JS record.cert || {})
			"domain": "port.test", "port": float64(4000), "containerIP": "127.0.0.1",
			"subdomain": []any{}, "cert": map[string]any{},
		},
		"http.test": { // resolver fails → cached app.ip fallback
			"domain": "http.test", "port": float64(5000), "containerIP": "172.17.0.9", "container": "172.17.0.9",
			"subdomain": []any{}, "cert": map[string]any{},
		},
	}
	if len(domains) != len(want) {
		t.Errorf("payload has %d domains (%v), want %d", len(domains), keys(domains), len(want))
	}
	for name, wantEntry := range want {
		got, _ := domains[name].(map[string]any)
		if !reflect.DeepEqual(got, map[string]any(wantEntry)) {
			t.Errorf("domains[%q] =\n%#v\nwant\n%#v", name, got, wantEntry)
		}
	}

	// Container IP cached back onto the app entry (Node: app.ip = containerIP).
	apps, _ := p.cfg.Get("apps").([]any)
	ctrapp, _ := apps[1].(map[string]any)
	if ctrapp["ip"] != "10.5.0.2" {
		t.Errorf("app.ip cache = %v, want 10.5.0.2", ctrapp["ip"])
	}

	fw, _ := payload["firewall"].(map[string]any)
	if fw["enabled"] != true {
		t.Errorf("firewall = %v, want default with enabled:true", payload["firewall"])
	}
	if _, ok := payload["memory"].(map[string]any); !ok {
		t.Errorf("memory = %v, want object", payload["memory"])
	}
	// Default config has ssl = {} (truthy in JS), sent as-is.
	if !reflect.DeepEqual(payload["ssl"], map[string]any{}) {
		t.Errorf("ssl = %v, want {}", payload["ssl"])
	}
	if !reflect.DeepEqual(payload["tunnels"], []any{}) {
		t.Errorf("tunnels = %v, want []", payload["tunnels"])
	}
}

func TestProxySSLNullWhenUnset(t *testing.T) {
	cs := newControlServer(t)
	p, _ := newTestProxy(t, cs, nil)
	p.cfg.Set("ssl", nil) // JS: config.ssl || null

	p.SyncConfig()
	payload := cs.nextConfig(t)
	if payload["ssl"] != nil {
		t.Errorf("ssl = %v, want null", payload["ssl"])
	}
}

func TestProxySetTunnels(t *testing.T) {
	cs := newControlServer(t)
	p, _ := newTestProxy(t, cs, nil)
	seedApps(p)

	n := p.SetTunnels([]Tunnel{
		{Domain: "t.test", Container: "hostapp", Token: "tok"},
		{Domain: "bad.test", Container: "hostapp"}, // no token → dropped
		{Domain: "ghost.test", Container: "nosuch", Token: "tk2"},
	})
	if n != 2 {
		t.Errorf("SetTunnels = %d, want 2 (validated entries)", n)
	}

	payload := cs.nextConfig(t) // SetTunnels syncs immediately
	wantTunnels := []any{
		map[string]any{"domain": "t.test", "host": "127.0.0.1", "port": float64(3000), "token": "tok"},
		// ghost.test resolves no app → skipped from the wire payload
	}
	if !reflect.DeepEqual(payload["tunnels"], wantTunnels) {
		t.Errorf("tunnels = %#v, want %#v", payload["tunnels"], wantTunnels)
	}

	persisted := p.cfg.Map("tunnels")
	if len(persisted) != 2 || persisted["t.test"] == nil || persisted["ghost.test"] == nil {
		t.Errorf("persisted tunnels = %v", persisted)
	}
}

func TestProxyStartRestoresTunnels(t *testing.T) {
	cs := newControlServer(t)
	p, fp := newTestProxy(t, cs, nil)
	seedApps(p)
	p.cfg.Set("tunnels", map[string]any{
		"t.test":    map[string]any{"container": "hostapp", "token": "tok"},
		"junk.test": map[string]any{"container": "hostapp"}, // no token → ignored
	})

	p.Start()
	if ensures, _ := fp.counts(); ensures != 1 {
		t.Errorf("Ensure calls = %d, want 1", ensures)
	}

	p.SyncConfig()
	payload := cs.nextConfig(t)
	tunnels, _ := payload["tunnels"].([]any)
	if len(tunnels) != 1 {
		t.Fatalf("tunnels = %#v, want the single restored tunnel", payload["tunnels"])
	}
}

func TestProxyLifecycle(t *testing.T) {
	cs := newControlServer(t)
	p, fp := newTestProxy(t, cs, nil)
	fp.running = false

	p.Check() // inactive → no-op
	if ensures, _ := fp.counts(); ensures != 0 {
		t.Errorf("Check before Start ensured %d times", ensures)
	}

	p.Start()
	p.Check()
	if ensures, _ := fp.counts(); ensures != 2 {
		t.Errorf("ensures = %d, want 2 (Start + Check)", ensures)
	}

	p.Stop()
	if _, stops := fp.counts(); stops != 1 {
		t.Errorf("stops = %d, want 1", stops)
	}
	p.Check() // stopped → inactive again
	if ensures, _ := fp.counts(); ensures != 2 {
		t.Errorf("Check after Stop respawned (%d ensures)", ensures)
	}
}

func TestProxySyncSkipsWhenSocketMissing(t *testing.T) {
	cs := newControlServer(t)
	p, fp := newTestProxy(t, cs, nil)
	fp.socket = cs.sock + ".gone"

	p.SyncConfig() // must return silently, no retry loop (Node parity)
	cs.expectNoConfig(t, 100*time.Millisecond)
}

func TestProxySyncRetriesOnConnRefused(t *testing.T) {
	dir, err := os.MkdirTemp("", "odacdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := dir + "/mod.sock"

	// A socket file with no listener behind it (the binary "crashed"):
	// dialing it fails with ECONNREFUSED, which is in Node's retry set.
	dead, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()

	cfg := newStore(t)
	p := NewProxy(cfg, t.TempDir(), nil)
	p.proc = &fakeProc{running: true, socket: sock}
	p.retryDelay = 50 * time.Millisecond

	configs := make(chan []byte, 1)
	done := make(chan struct{})
	go func() {
		p.SyncConfig()
		close(done)
	}()

	// While the first attempt fails and sleeps, replace the file with a real
	// listener; the retry must succeed.
	time.Sleep(20 * time.Millisecond)
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		select {
		case configs <- body:
		default:
		}
		w.Write([]byte("OK"))
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncConfig never returned")
	}
	select {
	case <-configs:
	case <-time.After(time.Second):
		t.Fatal("retry never delivered the config")
	}
}

func TestProxyWaitForReady(t *testing.T) {
	cs := newControlServer(t)
	p, _ := newTestProxy(t, cs, nil)

	cs.ready.Store(http.StatusServiceUnavailable)
	if p.WaitForReady(80 * time.Millisecond) {
		t.Error("WaitForReady = true while /ready returns 503")
	}

	cs.ready.Store(http.StatusOK)
	if !p.WaitForReady(2 * time.Second) {
		t.Fatal("WaitForReady = false with /ready 200")
	}
	cs.nextConfig(t) // readiness push (Node: syncConfig right after 200)
}

func TestProxyDockerWaitBeforeFirstSync(t *testing.T) {
	cs := newControlServer(t)
	resolver := &fakeResolver{ips: map[string]string{"ctr-123": "10.5.0.2"}}
	p, _ := newTestProxy(t, cs, resolver)
	seedApps(p) // ctrapp has a container port with no host port

	start := time.Now()
	go func() {
		time.Sleep(60 * time.Millisecond)
		resolver.available.Store(true)
	}()
	p.SyncConfig()
	cs.nextConfig(t)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("sync did not wait for Docker (elapsed %v)", elapsed)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
