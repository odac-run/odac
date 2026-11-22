package appmgr

import "odac/internal/applog"

// Snapshotter takes the snapshots that operations on an app call for: one
// beside a deploy's build, finished before the new container (and its
// migration) starts, and one before a delete. The backup manager
// implements it through the composition root. Nothing it does fails the
// operation: a snapshot that could not be taken is a line in the log.
type Snapshotter interface {
	// BeforeDeploy starts the snapshot of a deploy of app and returns at
	// once; nil means the deploy takes none (the app and the apps it links
	// hold no data, or snapshots are off).
	BeforeDeploy(app string) DeploySnapshot
	// BeforeDelete takes the snapshot before app is deleted, within a
	// bounded time, and returns a line for the log.
	BeforeDelete(app string) string
}

// DeploySnapshot is the snapshot a deploy started.
type DeploySnapshot interface {
	// Wait waits for it, within a bounded time after which it is given
	// up, and reports whether it was taken, with a line for the build log.
	Wait() (taken bool, note string)
	// Cancel gives up a snapshot the deploy ended without needing.
	Cancel()
}

// SetSnapshotter has every later deploy and delete take its snapshot
// through s.
func (m *Manager) SetSnapshotter(s Snapshotter) {
	m.mu.Lock()
	m.snapshots = s
	m.mu.Unlock()
}

func (m *Manager) snapshotter() Snapshotter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshots
}

// startDeploySnapshot starts the snapshot of a deploy, beside its build.
func (m *Manager) startDeploySnapshot(name string) DeploySnapshot {
	if s := m.snapshotter(); s != nil {
		return s.BeforeDeploy(name)
	}
	return nil
}

// awaitDeploySnapshot waits for it before the new container starts, and
// notes in the build log whether the deploy goes on with a snapshot or
// without one. The deploy goes on either way.
func (m *Manager) awaitDeploySnapshot(name string, snap DeploySnapshot, logCtrl *applog.BuildControl) {
	if snap == nil {
		return
	}
	taken, note := snap.Wait()
	if note == "" {
		return
	}
	if taken {
		m.dlog.Log("%s: %s", name, note)
	} else {
		m.dlog.Warn("%s: %s", name, note)
	}
	if logCtrl != nil {
		logCtrl.Write([]byte("[Snapshot] " + note + "\n"))
	}
}

// deleteSnapshot takes the snapshot before an app is deleted.
func (m *Manager) deleteSnapshot(name string) {
	s := m.snapshotter()
	if s == nil {
		return
	}
	if note := s.BeforeDelete(name); note != "" {
		m.log.Log("%s", note)
	}
}
