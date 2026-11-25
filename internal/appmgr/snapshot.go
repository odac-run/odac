package appmgr

import (
	"time"

	"odac/internal/applog"
)

// The events an operation names when it starts a snapshot. They are the
// backup manager's own values (backup.EventDeploy, backup.EventVersion),
// which the snapshot list reports as each point's event.
const (
	SnapshotDeploy  = "deploy"
	SnapshotVersion = "version"
)

// Snapshotter takes the snapshots that operations on an app call for: one
// before a deploy's or a version switch's new container (and its
// migration) starts, and one before a delete. The backup manager
// implements it through the composition root. Nothing it does fails a
// deploy or a delete: a snapshot that could not be taken is a line in the
// log. A version switch asked to require one is the exception.
type Snapshotter interface {
	// BeforeDeploy starts the snapshot that event (SnapshotDeploy,
	// SnapshotVersion) takes before app's container is replaced, and
	// returns at once. nil means none is taken (the app and the apps it
	// links hold no data, or snapshots are off), and the line says why.
	BeforeDeploy(app, event string) (DeploySnapshot, string)
	// BeforeDelete takes the snapshot before app is deleted, within a
	// bounded time, and returns a line for the log.
	BeforeDelete(app string) string
}

// DeploySnapshot is the snapshot a deploy or a version switch started.
type DeploySnapshot interface {
	// Wait waits for it, within a bounded time after which it is given
	// up (never longer than limit in all when limit is positive), and
	// returns the run of the snapshot taken, "" for none, with a line for
	// the log.
	Wait(limit time.Duration) (run, note string)
	// Cancel gives up a snapshot the operation ended without needing.
	Cancel()
}

// SetSnapshotter has every later deploy, version switch and delete take
// its snapshot through s.
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
		snap, _ := s.BeforeDeploy(name, SnapshotDeploy)
		return snap
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
	run, note := snap.Wait(0)
	if note == "" {
		return
	}
	if run != "" {
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
