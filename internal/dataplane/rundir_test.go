package dataplane

import (
	"path/filepath"
	"testing"

	"odac/internal/rundir"
)

// The app-mounted run dir is bind-mounted read-only at /odac, and a read-only
// mount does not stop connect(). A control socket there is reachable by any
// root app with an API grant.
func TestControlSocketsStayOutOfAppMountedDir(t *testing.T) {
	cfg := newStore(t)
	appDir := rundir.App(cfg.BaseDir())
	procs := map[string]process{
		"proxy": NewProxy(cfg, t.TempDir(), nil).proc,
		"dns":   NewDNS(cfg, t.TempDir()).proc,
		"mail":  NewMail(cfg, t.TempDir(), nil).proc,
	}
	for name, p := range procs {
		if dir := filepath.Dir(p.SocketPath()); dir == appDir {
			t.Errorf("%s control socket is in the app-mounted dir %s", name, dir)
		}
	}
}
