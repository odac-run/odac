package backup

import (
	"context"
	"time"

	"odac/internal/backup/settings"
)

// activeDump is the dump in flight and the apps whose deploy cancels it:
// the dumped app and every app that links it, whose migration changes it.
type activeDump struct {
	app      string
	group    map[string]bool
	cancel   context.CancelFunc
	byDeploy bool
}

// DeployStarted cancels the dump in flight when the app, or an app that
// links the dumped one, is being deployed. A migration that waits behind a
// dump's lock queues every later query behind itself, so the deploy goes
// first and the dump is taken again once it has ended. It returns at once,
// since the app manager calls it on its check tick.
func (m *Manager) DeployStarted(app string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deploying[app]++
	m.changed[app] = m.now()
	for d := range m.dumps {
		if d.group[app] && !d.byDeploy {
			d.byDeploy = true
			d.cancel()
			m.log.Log("Cancelled the dump of %s: %s is being deployed; it is taken again after the deploy", d.app, app)
		}
	}
}

// DeployEnded lets the dumps a deploy held back run again, after a short
// settle so that a restart's stop and start count as one deploy.
func (m *Manager) DeployEnded(app string) {
	m.mu.Lock()
	if m.deploying[app] > 1 {
		m.deploying[app]--
	} else {
		delete(m.deploying, app)
	}
	m.deployEnded[app] = m.now()
	m.changed[app] = m.now()
	m.mu.Unlock()
	m.poke()
}

// dumpGroup is the apps whose deploy affects app's data.
func dumpGroup(app string, apps []settings.App) map[string]bool {
	g := map[string]bool{app: true}
	for _, a := range apps {
		for _, l := range a.Linked {
			if l == app {
				g[a.Name] = true
			}
		}
	}
	return g
}

// groupIdle reports whether no app of the group is being deployed or has
// just been, and if not, when to look again.
func (m *Manager) groupIdle(group map[string]bool) (bool, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var until time.Time
	for app := range group {
		if m.deploying[app] > 0 {
			return false, now.Add(m.t.deploySettle)
		}
		if end, ok := m.deployEnded[app]; ok {
			if t := end.Add(m.t.deploySettle); t.After(now) && t.After(until) {
				until = t
			} else if !t.After(now) {
				delete(m.deployEnded, app)
			}
		}
	}
	return until.IsZero(), until
}

// beginDump registers a dump in flight, unless an app of its group is being
// deployed right now; checking and registering under one lock leaves no
// moment in which a deploy could start unseen. The returned function ends
// the registration and reports whether a deploy cancelled the dump.
func (m *Manager) beginDump(app string, group map[string]bool, cancel context.CancelFunc) (func() bool, bool) {
	d := &activeDump{app: app, group: group, cancel: cancel}
	m.mu.Lock()
	for a := range group {
		if m.deploying[a] > 0 {
			m.mu.Unlock()
			return nil, false
		}
	}
	m.dumps[d] = true
	m.mu.Unlock()
	return func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.dumps, d)
		return d.byDeploy
	}, true
}
