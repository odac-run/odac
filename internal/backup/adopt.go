package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
)

// adoptOrphans takes over the helpers the previous server handed over and
// follows each to its result, which is then handled like a run of this
// server's own. Their targets and slots are held until they end, so no run
// of this server meets them in a repository. It reports false while Docker
// does not answer.
func (m *Manager) adoptOrphans(ctx context.Context, logged *bool) bool {
	r, err := m.currentRunner()
	if err != nil {
		return errors.Is(err, errReleased)
	}
	orphans, err := r.Adopt()
	if err != nil {
		if !*logged {
			m.log.Error("Cannot look for backups the previous server left running, waiting for Docker: %s", err.Error())
			*logged = true
		}
		return false
	}
	running := map[string]bool{}
	m.mu.Lock()
	for _, o := range orphans {
		l := o.Labels()
		kind, target := l[labelKind], l[labelTarget]
		if target != "" {
			m.busy[target] = kind
		}
		switch kind {
		case KindBackup:
			m.backupSlot = true
			running[l[labelJob]] = true
		case KindRestore:
			// The backup before a restore, which the restore's own
			// recovery reports.
			m.backupSlot = true
		case KindMaintain:
			m.maintSlot = true
		case KindSnapshot:
			m.snapSlot = true
		}
	}
	m.mu.Unlock()
	// A run that was in flight and has no helper left was cut short (a
	// crash, or a release between two of its helper runs): it runs again
	// now rather than at its next slot, which can be a day away.
	now := ms(m.now())
	err = m.state.update(func(d *stateData) {
		for id, js := range d.Jobs {
			if js.Running != "" && !running[id] {
				js.Running = ""
				js.Next = min(js.Next, now)
			}
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
	if len(orphans) > 0 {
		m.log.Log("Took over %s backup runs the previous server left running", len(orphans))
	}
	keep := map[string]bool{}
	for _, o := range orphans {
		keep[o.Labels()[runner.LabelRun]] = true
	}
	m.sweepDefinitions(keep)
	for _, o := range orphans {
		m.others.Add(1)
		go func(o Orphan) {
			defer m.others.Done()
			m.follow(ctx, o)
		}(o)
	}
	return true
}

// follow waits for an adopted helper and handles its result. Stopping
// leaves the helper running for the next server.
func (m *Manager) follow(ctx context.Context, o Orphan) {
	l := o.Labels()
	kind, target := l[labelKind], l[labelTarget]
	defer m.free(target, kind)
	var (
		mu    sync.Mutex
		snaps []proto.Snapshot
	)
	res, err := o.Follow(ctx, proto.Handler{Snapshot: func(s proto.Snapshot) {
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
	}})
	if ctx.Err() != nil || errors.Is(err, runner.ErrGone) {
		return
	}
	if err != nil {
		m.observe(target, err)
	}
	mu.Lock()
	got := snaps
	mu.Unlock()
	switch kind {
	case KindBackup, KindSnapshot:
		if run := l[runner.LabelRun]; run != "" {
			os.RemoveAll(filepath.Join(m.cfg.BaseDir, "backup", "definitions", filepath.Base(run)))
		}
		if kind == KindSnapshot {
			m.adoptedSnapshot(ctx, l, got, err)
			return
		}
		m.adoptedBackup(ctx, l, got, err)
	case KindMaintain:
		rec := RunRecord{Kind: KindMaintain, Target: target, Start: ms(m.now()), Adopted: true}
		var tr tidyResult
		if err == nil {
			tr = m.tidy(ctx, target, res)
			if ctx.Err() != nil || errReleasedOr(tr.err) {
				return
			}
			err = tr.err
		}
		m.afterMaintain(target, &rec, res, tr, err)
	}
}

// adoptedBackup records a backup the previous server started. The helper
// knew nothing of the pieces before it, so the run is complete only when
// the previous server marked it as the last of a run that had gone well.
// Plans with state on the database (ClickHouse's frozen parts) are made
// again under the same run and finished, which the previous server could
// not do while the helper still read them.
func (m *Manager) adoptedBackup(ctx context.Context, l map[string]string, snaps []proto.Snapshot, err error) {
	run, job, target := l[runner.LabelRun], l[labelJob], l[labelTarget]
	now := m.now()
	rec := RunRecord{Kind: KindBackup, Run: run, Job: job, Target: target, Trigger: l[labelTrigger], Status: StatusOK, Start: ms(now), Adopted: true}
	for _, s := range snaps {
		rec.Snapshots++
		rec.Bytes += s.Bytes
		if t := ms(s.Start); t < rec.Start {
			rec.Start = t
		}
	}
	apps := []string{}
	if l[labelApps] != "" {
		apps = strings.Split(l[labelApps], ",")
	}
	for _, a := range apps {
		res := AppResult{App: a, Status: StatusOK}
		if err != nil {
			res.Status, res.Error = StatusFailed, err.Error()
		}
		rec.Apps = append(rec.Apps, res)
	}
	complete := err == nil && l[labelComplete] == "true"
	switch {
	case err != nil && len(snaps) > 0:
		rec.Status, rec.Error = StatusPartial, err.Error()
	case err != nil:
		rec.Status, rec.Error = StatusFailed, err.Error()
	case !complete:
		rec.Status, rec.Error = StatusPartial, "an update interrupted the run"
	}
	if fin := l[labelFinish]; fin != "" {
		for _, app := range strings.Split(fin, ",") {
			m.finishAdopted(ctx, app, run)
		}
	}
	serr := m.state.update(func(d *stateData) {
		if js := d.Jobs[job]; js != nil && js.Running == run {
			js.Running = ""
		}
		if t := d.target(target); !complete && len(snaps) > 0 && !slices.Contains(t.Partial, run) {
			t.Partial = append(t.Partial, run)
		}
	})
	if serr != nil {
		m.log.Error("Failed to save the backup state: %s", serr.Error())
	}
	if len(snaps) > 0 {
		rec.Deleted = m.tidy(ctx, target, nil).deleted
	}
	rec.End = ms(m.now())
	if serr := m.state.update(func(d *stateData) { d.History = append(d.History, rec) }); serr != nil {
		m.log.Error("Failed to save the backup state: %s", serr.Error())
	}
	m.report(job, &rec)
}

func (m *Manager) finishAdopted(ctx context.Context, app, run string) {
	plan, d, err := m.plan(ctx, app, run)
	if err != nil || d != nil || plan == nil {
		m.log.Warn("Could not clean up after the backup of %s the previous server started: its frozen copy for run %s stays on the database's disk", app, run)
		return
	}
	fctx, cancel := context.WithTimeout(ctx, m.t.finishLimit)
	defer cancel()
	if err := plan.Finish(fctx); err != nil {
		m.log.Error("Failed to clean up after backing up %s: %s", app, err.Error())
	}
}

// sweepDefinitions removes the definition directories of runs that no
// helper reads any more: a run cut short by a crash leaves its own.
func (m *Manager) sweepDefinitions(keep map[string]bool) {
	root := filepath.Join(m.cfg.BaseDir, "backup", "definitions")
	list, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range list {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}
