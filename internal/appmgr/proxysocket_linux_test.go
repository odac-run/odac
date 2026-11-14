//go:build linux

package appmgr

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odac/internal/docker"
	"odac/internal/rundir"
)

func proxySocketMount(opts docker.RunOptions) string {
	for _, v := range opts.Volumes {
		if v.Container == rundir.BackendMount {
			return v.Host
		}
	}
	return ""
}

func TestRunGivesBridgeContainerItsOwnSocketDir(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "web", "active": true, "type": "container", "image": "test:latest"},
		map[string]any{"id": float64(2), "name": "hostnet", "active": true, "type": "container", "image": "test:latest", "networkMode": "host"},
	})
	root := rundir.Backends(fx.cfg.BaseDir())
	stale := filepath.Join(root, "web", rundir.BackendSocket)
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, nil, 0o644)

	fx.checkAndSettle(t)

	byName := map[string]docker.RunOptions{}
	for i := 0; i < fx.dock.runCallCount(); i++ {
		call := fx.dock.runCallAt(i)
		byName[call.name] = call.options
	}
	web := byName["web"]
	if got := proxySocketMount(web); got != filepath.Join(root, "web") {
		t.Fatalf("web socket mount = %q", got)
	}
	if got := web.Env["ODAC_PROXY_SOCKET"]; got != rundir.BackendMount+"/"+rundir.BackendSocket {
		t.Fatalf("ODAC_PROXY_SOCKET = %q", got)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a previous container's file survived in the new socket dir")
	}
	if fi, err := os.Stat(filepath.Join(root, "web")); err != nil || fi.Mode().Perm() != 0o777 {
		t.Fatalf("socket dir mode: %v %v", fi, err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket root mode: %v %v", fi, err)
	}

	host := byName["hostnet"]
	if proxySocketMount(host) != "" || host.Env["ODAC_PROXY_SOCKET"] != "" {
		t.Fatal("a host-network app got a socket dir")
	}
}

func TestSocketDirKillSwitch(t *testing.T) {
	t.Setenv("ODAC_BACKEND_SOCKETS", "off")
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "web", "active": true, "type": "container", "image": "test:latest"},
	})
	fx.checkAndSettle(t)
	opts := fx.dock.runCallAt(0).options
	if proxySocketMount(opts) != "" || opts.Env["ODAC_PROXY_SOCKET"] != "" {
		t.Fatal("socket dir attached with ODAC_BACKEND_SOCKETS=off")
	}
}

func TestSweepProxySockets(t *testing.T) {
	fx := newFixture(t, nil)
	fx.m.socketSweep.Do(func() {}) // keep the first-pulse sweep out of the way
	root := rundir.Backends(fx.cfg.BaseDir())
	old := time.Now().Add(-2 * proxySocketGrace)
	for _, name := range []string{"live", "gone", "young"} {
		os.MkdirAll(filepath.Join(root, name), 0o777)
		if name != "young" {
			os.Chtimes(filepath.Join(root, name), old, old)
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	fx.dock.mu.Lock()
	fx.dock.socketDirsErr = errors.New("daemon stalled")
	fx.dock.mu.Unlock()
	fx.m.SweepProxySockets()
	if !exists("gone") {
		t.Fatal("a failed engine query removed a directory")
	}

	fx.dock.mu.Lock()
	fx.dock.socketDirsErr = nil
	fx.dock.socketDirs = map[string]bool{"live": true}
	fx.dock.mu.Unlock()
	fx.m.SweepProxySockets()
	if !exists("live") || !exists("young") || exists("gone") {
		t.Fatalf("after sweep: live=%v young=%v gone=%v", exists("live"), exists("young"), exists("gone"))
	}
}

func TestStartSyncsAgainOnceTheSocketExists(t *testing.T) {
	syncsFor := func(t *testing.T, listen bool) int {
		fx := newFixture(t, []any{
			map[string]any{"id": float64(1), "name": "web", "active": true, "type": "container", "image": "test:latest"},
		})
		root := rundir.Backends(fx.cfg.BaseDir())
		fx.dock.mu.Lock()
		fx.dock.runHook = func(name string) {
			if !listen {
				return
			}
			ln, err := net.Listen("unix", filepath.Join(root, name, rundir.BackendSocket))
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(func() { ln.Close() })
		}
		fx.dock.mu.Unlock()
		fx.checkAndSettle(t)
		fx.proxy.mu.Lock()
		defer fx.proxy.mu.Unlock()
		return fx.proxy.syncs
	}
	without := syncsFor(t, false)
	if with := syncsFor(t, true); with != without+1 {
		t.Fatalf("syncs with a socket = %d, without = %d; want exactly one more", with, without)
	}
}
