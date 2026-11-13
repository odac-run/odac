//go:build linux

package dataplane

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"odac/internal/rundir"
)

func listenAt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
}

func TestProxyPayloadCarriesLiveBackendSocket(t *testing.T) {
	cs := newControlServer(t)
	resolver := &fakeResolver{
		ips:     map[string]string{"ctr-123": "10.5.0.2", "other": "10.5.0.3"},
		sockets: map[string]string{"ctr-123": "web-green-ab12", "other": "other"},
	}
	resolver.available.Store(true)
	p, _ := newTestProxy(t, cs, resolver)
	root := rundir.Backends(p.cfg.BaseDir())
	sock := filepath.Join(root, "web-green-ab12", rundir.BackendSocket)
	listenAt(t, sock)
	// "other" mounts a directory with no socket in it: a non-framework app.
	os.MkdirAll(filepath.Join(root, "other"), 0o777)

	p.cfg.Set("apps", []any{
		map[string]any{"name": "web", "id": "a1", "ports": []any{map[string]any{"container": float64(8080)}}, "activeContainerId": "ctr-123"},
		map[string]any{"name": "other", "id": "a2", "ports": []any{map[string]any{"container": float64(8080)}}},
		map[string]any{"name": "hostapp", "id": "a3", "ports": []any{map[string]any{"host": "3000"}}},
	})
	p.cfg.Set("domains", map[string]any{
		"web.test":   map[string]any{"appId": "web"},
		"other.test": map[string]any{"appId": "other"},
		"host.test":  map[string]any{"appId": "hostapp"},
	})
	p.SetTunnels([]Tunnel{{Domain: "t.test", Container: "web", Token: "x"}})

	p.SyncConfig()
	payload := cs.nextConfig(t)
	domains := payload["domains"].(map[string]any)
	if got := domains["web.test"].(map[string]any)["socket"]; got != sock {
		t.Errorf("web.test socket = %v, want %s", got, sock)
	}
	for _, name := range []string{"other.test", "host.test"} {
		if s, ok := domains[name].(map[string]any)["socket"]; ok {
			t.Errorf("%s carries socket %v", name, s)
		}
	}
	for _, tn := range payload["tunnels"].([]any) {
		if _, ok := tn.(map[string]any)["socket"]; ok {
			t.Error("a tunnel carries a socket; tunnels dial TCP")
		}
	}
}

func TestProxyPayloadIgnoresSymlinkedSocket(t *testing.T) {
	cs := newControlServer(t)
	resolver := &fakeResolver{ips: map[string]string{"web": "10.5.0.2"}, sockets: map[string]string{"web": "web"}}
	resolver.available.Store(true)
	p, _ := newTestProxy(t, cs, resolver)
	root := rundir.Backends(p.cfg.BaseDir())
	target := filepath.Join(t.TempDir(), "s.sock")
	listenAt(t, target)
	os.MkdirAll(filepath.Join(root, "web"), 0o777)
	os.Symlink(target, filepath.Join(root, "web", rundir.BackendSocket))
	p.cfg.Set("apps", []any{map[string]any{"name": "web", "id": "a1", "ports": []any{map[string]any{"container": float64(8080)}}}})
	p.cfg.Set("domains", map[string]any{"web.test": map[string]any{"appId": "web"}})

	p.SyncConfig()
	if s, ok := cs.nextConfig(t)["domains"].(map[string]any)["web.test"].(map[string]any)["socket"]; ok {
		t.Fatalf("symlink sent as socket %v", s)
	}
}

func TestProxyGetsSocketRootOnlyWhenEnabled(t *testing.T) {
	base := t.TempDir()
	want := "ODAC_BACKEND_SOCKET_DIR=" + rundir.Backends(base)
	if env := proxyEnv(base); !slices.Equal(env, []string{want}) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	t.Setenv("ODAC_BACKEND_SOCKETS", "off")
	if env := proxyEnv(base); env != nil {
		t.Fatalf("kill switch left env %q", env)
	}
}
