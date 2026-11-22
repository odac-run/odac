package backup

import (
	"context"
	"slices"
	"time"

	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
)

// SnapshotStatus is where snapshots stand: the settings, the disk as
// their budget sees it, what runs and what is due. Sizes are bytes, times
// Unix milliseconds.
type SnapshotStatus struct {
	Enabled    bool `json:"enabled"`
	MaxDiskPct int  `json:"maxDiskPct"`
	// Available is a server that can take snapshots at all (it has an id,
	// and its repository opens).
	Available bool `json:"available"`
	// Used is what the repository takes on disk, Garbage the part of it
	// maintenance is giving back, Limit the most it may hold now (absent
	// when the disk cannot be measured). Free and Total describe ODAC's
	// data disk, Floor the free space nothing may take.
	Used         int64              `json:"used"`
	Garbage      int64              `json:"garbage,omitempty"`
	Limit        *int64             `json:"limit,omitempty"`
	Free         int64              `json:"free,omitempty"`
	Total        int64              `json:"total,omitempty"`
	Floor        int64              `json:"floor,omitempty"`
	NextMaintain int64              `json:"nextMaintain,omitempty"`
	LastMaintain int64              `json:"lastMaintain,omitempty"`
	Running      *SnapshotRunning   `json:"running,omitempty"`
	Periodic     []PeriodicSnapshot `json:"periodic"`
	Deploys      []DeployMark       `json:"deploys"`
	Warnings     []SnapshotWarning  `json:"warnings"`
}

// PeriodicSnapshot is an app's periodic schedule; Every is in minutes.
type PeriodicSnapshot struct {
	App            string `json:"app"`
	Next           int64  `json:"next"`
	Every          int    `json:"every,omitempty"`
	Failures       int    `json:"failures,omitempty"`
	PostponedSince int64  `json:"postponedSince,omitempty"`
}

// SnapshotStatus reports where snapshots stand.
func (m *Manager) SnapshotStatus() SnapshotStatus {
	out := SnapshotStatus{MaxDiskPct: settings.DefaultMaxDiskPct, Periodic: []PeriodicSnapshot{}, Deploys: []DeployMark{}, Warnings: []SnapshotWarning{}}
	if set := m.settings(); set != nil {
		if s, err := set.Snapshots(); err == nil {
			out.Enabled, out.MaxDiskPct = s.Enabled, s.MaxDiskPct
		}
	}
	out.Available = m.cfg.ServerID != ""
	space := m.snapSpace()
	out.Used, out.Garbage = space.Used, space.Garbage
	if space.ok {
		out.Free, out.Total, out.Floor = space.Free, space.Total, thin.Floor(space.Total)
		limit := snapshotLimit(space, out.MaxDiskPct)
		out.Limit = &limit
	}
	m.mu.Lock()
	if r := m.snapRunning; r != nil && r.Run != "" {
		c := *r
		out.Running = &c
	}
	m.mu.Unlock()
	m.state.view(func(d *stateData) {
		if t := d.Targets[SnapshotTarget]; t != nil {
			out.NextMaintain, out.LastMaintain = t.NextMaintain, t.LastMaintain
		}
		for app, p := range d.Snap.Periodic {
			out.Periodic = append(out.Periodic, PeriodicSnapshot{App: app, Next: p.Next, Every: int(p.Every / int64(time.Minute/time.Millisecond)), Failures: p.Failures, PostponedSince: p.PostponedSince})
		}
		for _, mark := range d.Snap.LastDeploy {
			out.Deploys = append(out.Deploys, *mark)
		}
		out.Warnings = append(out.Warnings, d.Snap.Warnings...)
		for _, w := range d.Snap.Warnings {
			if w.Code == ReasonUnavailable {
				out.Available = false
			}
		}
	})
	slices.SortFunc(out.Periodic, func(a, b PeriodicSnapshot) int { return cmpString(a.App, b.App) })
	slices.SortFunc(out.Deploys, func(a, b DeployMark) int { return cmpString(a.App, b.App) })
	slices.SortFunc(out.Warnings, func(a, b SnapshotWarning) int { return cmpString(a.Code, b.Code) })
	return out
}

// SnapshotHistory returns the newest snapshot records first, of one app
// when app is set (filed under it or taking it), at most limit of them
// (zero for all that are kept).
func (m *Manager) SnapshotHistory(app string, limit int) []SnapshotRecord {
	var out []SnapshotRecord
	m.state.view(func(d *stateData) {
		h := d.Snap.History
		for i := len(h) - 1; i >= 0; i-- {
			r := h[i]
			if app != "" && r.App != app && !slices.ContainsFunc(r.Apps, func(a AppResult) bool { return a.App == app }) {
				continue
			}
			r.Apps = slices.Clone(r.Apps)
			out = append(out, r)
			if limit > 0 && len(out) == limit {
				return
			}
		}
	})
	return out
}

// SnapshotPoints lists the snapshots a restore can return to, newest
// first, of one app when app is set. A deleted app's snapshots say when
// it was deleted and when they go.
func (m *Manager) SnapshotPoints(app string) []SnapshotPoint {
	var out []SnapshotPoint
	m.state.view(func(d *stateData) {
		for _, p := range d.Snap.Index {
			if app != "" && p.App != app && !slices.ContainsFunc(p.Apps, func(a RestorePointApp) bool { return a.App == app }) {
				continue
			}
			p.Apps = slices.Clone(p.Apps)
			if at := d.Snap.Gone[p.App]; at > 0 {
				p.Gone, p.Expires = at, at+int64(thin.DefaultSnapshotRules.Gone/time.Millisecond)
			}
			out = append(out, p)
		}
	})
	return out
}

// SnapshotRestoreRequest is a restore from the snapshot repository, as a
// RestoreRequest without the target, server and key it does not have.
type SnapshotRestoreRequest struct {
	Run        string            `json:"run"`
	Apps       []string          `json:"apps,omitempty"`
	Mode       string            `json:"mode,omitempty"`
	Names      map[string]string `json:"names,omitempty"`
	SkipSafety bool              `json:"skipSafety,omitempty"`
}

// snapshotRestore turns a snapshot restore into a restore. A snapshot is
// restored in place unless the mode says otherwise, since quick return is
// what it is for, except when an app it would restore no longer exists
// (a deleted app brought back): that comes back as a new app, which
// touches nothing that lives.
func (m *Manager) snapshotRestore(req SnapshotRestoreRequest) RestoreRequest {
	mode := req.Mode
	if mode == "" {
		mode = RestoreInPlace
		apps := req.Apps
		if len(apps) == 0 {
			m.state.view(func(d *stateData) {
				for _, p := range d.Snap.Index {
					if p.Run == req.Run {
						for _, a := range p.Apps {
							apps = append(apps, a.App)
						}
					}
				}
			})
		}
		live := map[string]bool{}
		for _, a := range m.cfg.Apps.BackupApps() {
			live[a.Name] = true
		}
		for _, a := range apps {
			if !live[a] {
				mode = RestoreNewApp
			}
		}
	}
	return RestoreRequest{Target: SnapshotTarget, Run: req.Run, Apps: req.Apps, Mode: mode, Names: req.Names, SkipSafety: req.SkipSafety}
}

// PreviewSnapshotRestore works a snapshot restore out without changing
// anything, as PreviewRestore does.
func (m *Manager) PreviewSnapshotRestore(ctx context.Context, req SnapshotRestoreRequest) (*RestoreResult, error) {
	return m.PreviewRestore(ctx, m.snapshotRestore(req))
}

// StartSnapshotRestore starts a snapshot restore in the background, as
// StartRestore does; it is listed with the other restores, under the
// target SnapshotTarget.
func (m *Manager) StartSnapshotRestore(ctx context.Context, req SnapshotRestoreRequest) (*RestoreResult, error) {
	return m.StartRestore(ctx, m.snapshotRestore(req))
}

// DeleteSnapshot deletes one snapshot run, which is how an operator gives
// back the disk of snapshots taken before they were turned off. Its space
// comes back with maintenance.
func (m *Manager) DeleteSnapshot(ctx context.Context, run string) error {
	if run == "" {
		return refusal(settings.CodeInvalid, "A snapshot is named by its run.")
	}
	reading := false
	m.mu.Lock()
	if r := m.restore; r != nil {
		r.mu.Lock()
		reading = r.result.Target == SnapshotTarget && r.result.Run == run
		r.mu.Unlock()
	}
	m.mu.Unlock()
	if reading {
		return refusal(settings.CodeInUse, "Snapshot %s is being restored.", run)
	}
	if !m.snapshotStoreExists() {
		return refusal(settings.CodeNotFound, "Snapshot %s not found.", run)
	}
	release, err := m.acquireSnapshot(ctx, false, SnapshotRunning{})
	if err != nil {
		return err
	}
	defer release()
	snaps, err := m.list(ctx, SnapshotTarget)
	if err != nil {
		return err
	}
	runs := groupRuns(snaps)
	if runs[run] == nil {
		return refusal(settings.CodeNotFound, "Snapshot %s not found.", run)
	}
	if err := m.deleteRuns(ctx, SnapshotTarget, runs, map[string]bool{run: true}); err != nil {
		return err
	}
	m.maintainSoon(false)
	m.tidySnapshots(ctx, nil, 0)
	return nil
}
