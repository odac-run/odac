package appmgr

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"odac/internal/docker"
	"odac/internal/netmode"
	"odac/internal/rundir"
)

// proxySocketGrace keeps a directory the sweep finds unmounted for a while: it
// is created just before its container, so a young one may be about to be used.
const proxySocketGrace = 5 * time.Minute

func (m *Manager) proxySocketRoot() string {
	return rundir.Backends(m.cfg.BaseDir())
}

// attachProxySocket gives a bridge container an empty directory of its own,
// mounted at rundir.BackendMount, and tells the app where to listen through
// ODAC_PROXY_SOCKET. The directory is named after the container at create
// and keeps that name: a Blue-Green rename does not move the mount, so the
// proxy payload reads the directory back from the engine instead.
func (m *Manager) attachProxySocket(name string, opts *docker.RunOptions) {
	root := m.proxySocketRoot()
	if root == "" || netmode.IsHost(opts.NetworkMode) || !singleElement(name) {
		return
	}
	if err := rundir.Prepare(root); err != nil {
		m.log.Error("Proxy socket root %s: %s", root, err.Error())
		return
	}
	dir := filepath.Join(root, name)
	if err := os.RemoveAll(dir); err != nil {
		m.log.Error("Proxy socket dir %s: %s", dir, err.Error())
		return
	}
	// World-writable because the app may run as any uid. The root above it is
	// 0700, so nothing on the host but root reaches it, and no other container
	// mounts it.
	if err := os.Mkdir(dir, 0o777); err != nil {
		m.log.Error("Proxy socket dir %s: %s", dir, err.Error())
		return
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		m.log.Error("Proxy socket dir %s: %s", dir, err.Error())
		return
	}
	opts.Volumes = append(opts.Volumes, docker.Mount{Host: dir, Container: rundir.BackendMount})
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	opts.Env["ODAC_PROXY_SOCKET"] = rundir.BackendMount + "/" + rundir.BackendSocket
}

// syncOnProxySocket pushes the proxy config once more when the app creates its
// socket. A start syncs right away, before the app had time to listen, and the
// payload only names a socket that exists. Blue-Green does not need this: it
// syncs after the green container passed its HTTP probe.
func (m *Manager) syncOnProxySocket(id any, name string) {
	root := m.proxySocketRoot()
	if root == "" || !singleElement(name) {
		return
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	sock := filepath.Join(dir, rundir.BackendSocket)
	for attempts := 0; attempts < 60; attempts++ {
		if attempts > 0 {
			m.sleep(m.pollInterval)
		}
		if m.appDeleted(id) {
			return
		}
		if fi, err := os.Lstat(sock); err == nil && fi.Mode().Type() == fs.ModeSocket {
			m.proxySync()
			return
		}
	}
}

// SweepProxySockets removes socket directories no container mounts any more,
// once they are older than proxySocketGrace. A failed engine query removes
// nothing, since reading it as "no container" would empty live directories.
func (m *Manager) SweepProxySockets() {
	root := m.proxySocketRoot()
	if root == "" || !m.deps.Docker.Available() {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		return
	}
	used, err := m.deps.Docker.ProxySocketDirs()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-proxySocketGrace)
	for _, ent := range entries {
		if used[ent.Name()] {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, ent.Name())); err != nil {
			m.log.Error("Failed to remove proxy socket dir %s: %s", ent.Name(), err.Error())
		}
	}
}

func singleElement(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
}
