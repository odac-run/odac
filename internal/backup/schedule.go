package backup

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/sysinfo"
)

// loop adopts what the previous server left and then schedules. Nothing
// runs before the adoption has worked: a helper the previous server handed
// over still holds its target.
func (m *Manager) loop(ctx context.Context) {
	logged := false
	for !m.adoptOrphans(ctx, &logged) {
		if !sleep(ctx, m.t.adoptRetry) {
			return
		}
	}
	m.recoverRestore()
	m.mu.Lock()
	close(m.recoveredCh)
	m.mu.Unlock()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		next := m.schedule(ctx)
		wait := min(max(next.Sub(m.now()), 0), m.t.poll)
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-timer.C:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextAfter is the first slot of the job's schedule after now, counted
// from when the run was due: the schedule stays on its own clock, and slots
// missed while the server was down or busy are not made up one by one.
func nextAfter(due time.Time, every time.Duration, now time.Time) time.Time {
	next := due.Add(every)
	if !next.After(now) {
		next = next.Add(every * (now.Sub(next)/every + 1))
	}
	return next
}

// schedule starts what is due, if its slot is free, and says when to look
// again.
func (m *Manager) schedule(ctx context.Context) time.Time {
	now := m.now()
	next := now.Add(m.t.poll)
	set := m.settings()
	jobs, err := set.Jobs()
	if err != nil {
		m.log.Error("Cannot read the backup jobs: %s", err.Error())
		return next
	}
	retired, err := set.Retired()
	if err != nil {
		m.log.Error("Cannot read the deleted backup jobs: %s", err.Error())
		return next
	}
	m.reconcile(jobs, retired, now)
	m.scheduleMaintenance(ctx, jobs, retired, now, &next)
	m.scheduleBackup(ctx, jobs, now, &next)
	return next
}

// reconcile gives new jobs a first run now and drops what belongs to jobs
// and targets that are gone.
func (m *Manager) reconcile(jobs []settings.JobView, retired []settings.RetiredView, now time.Time) {
	known := map[string]bool{}
	targets := map[string]bool{}
	for _, j := range jobs {
		known[j.ID] = true
		targets[j.Target] = true
	}
	for _, r := range retired {
		targets[r.Target] = true
	}
	changed := false
	m.state.view(func(d *stateData) {
		for _, j := range jobs {
			if d.Jobs[j.ID] == nil {
				changed = true
			}
		}
		for id := range d.Jobs {
			changed = changed || !known[id]
		}
		for id, f := range d.FollowUps {
			changed = changed || !known[id] || f.Until <= ms(now)
		}
		for id := range d.Targets {
			changed = changed || !targets[id]
		}
	})
	if !changed {
		return
	}
	err := m.state.update(func(d *stateData) {
		for _, j := range jobs {
			if d.Jobs[j.ID] == nil {
				d.Jobs[j.ID] = &jobState{Next: ms(now)}
			}
		}
		for id := range d.Jobs {
			if !known[id] {
				delete(d.Jobs, id)
			}
		}
		for id, f := range d.FollowUps {
			if !known[id] || f.Until <= ms(now) {
				delete(d.FollowUps, id)
			}
		}
		for id := range d.Targets {
			if !targets[id] {
				delete(d.Targets, id)
			}
		}
		for app, r := range d.Dumps {
			if !targets[r.Target] {
				delete(d.Dumps, app)
			}
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
}

// loaded reports a host too busy for a heavy backup: a load average above
// one and a half per core, or less than a tenth of memory available.
// Unknown figures (zero) never postpone.
func loaded(l sysinfo.Load) bool {
	if l.Cores > 0 && l.Load1 > 1.5*float64(l.Cores) {
		return true
	}
	return l.MemTotal > 0 && l.MemAvailable > 0 && l.MemAvailable < l.MemTotal/10
}

type candidate struct {
	job    settings.JobView
	follow *followUp
	manual bool
	due    time.Time
}

// scheduleBackup starts the most overdue job, one at a time per server.
// The rest of a run a deploy cut short comes first, once its apps are
// idle. A busy host postpones the run, but never past maxPostpone: a host
// that is always busy still gets its backups.
func (m *Manager) scheduleBackup(ctx context.Context, jobs []settings.JobView, now time.Time, next *time.Time) {
	m.mu.Lock()
	busy := m.backupSlot || len(m.restoreWait) > 0
	m.mu.Unlock()
	if busy {
		return
	}
	var cands []candidate
	var postponed time.Time
	m.state.view(func(d *stateData) {
		for _, j := range jobs {
			if f := d.FollowUps[j.ID]; f != nil {
				c := *f
				cands = append(cands, candidate{job: j, follow: &c, due: fromMs(f.Scheduled)})
			}
			js := d.Jobs[j.ID]
			if js == nil {
				continue
			}
			if js.Manual > 0 && js.Running == "" {
				cands = append(cands, candidate{job: j, manual: true, due: fromMs(js.Manual)})
			}
			if js.Next <= ms(now) {
				cands = append(cands, candidate{job: j, due: fromMs(js.Next)})
				if js.PostponedSince > 0 && (postponed.IsZero() || fromMs(js.PostponedSince).Before(postponed)) {
					postponed = fromMs(js.PostponedSince)
				}
			} else if t := fromMs(js.Next); t.Before(*next) {
				*next = t
			}
		}
	})
	// The rest of a cut-short run first, then what the operator asked for
	// (a busy host would otherwise hold it behind an overdue run), then the
	// most overdue.
	rank := func(c candidate) int {
		switch {
		case c.follow != nil:
			return 0
		case c.manual:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), a.due.Compare(b.due))
	})

	var pick *candidate
	for i := range cands {
		c := &cands[i]
		if m.targetBusy(c.job.Target) {
			continue
		}
		if c.follow != nil {
			group := map[string]bool{}
			apps := m.cfg.Apps.BackupApps()
			for _, a := range c.follow.Apps {
				for g := range dumpGroup(a, apps) {
					group[g] = true
				}
			}
			if idle, until := m.groupIdle(group); !idle {
				if !until.IsZero() && until.Before(*next) {
					*next = until
				}
				continue
			}
		}
		pick = c
		break
	}
	if pick == nil {
		return
	}
	// The operator asked for this run, so a busy host does not hold it
	// back; it still runs at the helper's low priority.
	if !pick.manual && m.cfg.Load != nil && loaded(m.cfg.Load()) {
		if postponed.IsZero() || now.Sub(postponed) < m.t.maxPostpone {
			m.postpone(pick.job, now, postponed.IsZero())
			if t := now.Add(m.t.loadRetry); t.Before(*next) {
				*next = t
			}
			return
		}
		m.log.Warn("Running backup job %s although the server is busy: it was postponed for %s", pick.job.Name, now.Sub(postponed).Round(time.Minute))
	}
	m.startBackup(ctx, *pick, now)
}

func (m *Manager) postpone(job settings.JobView, now time.Time, first bool) {
	if first {
		m.log.Log("Postponing backup job %s: the server is busy", job.Name)
	}
	m.state.update(func(d *stateData) {
		if js := d.job(job.ID); js.PostponedSince == 0 {
			js.PostponedSince = ms(now)
		}
	})
}

func (m *Manager) targetBusy(target string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[target] != ""
}

// startBackup takes the backup slot and the target and runs the job in a
// tracked goroutine. The next run time is written first, so a run handed
// over in an update is not started again by the next server.
func (m *Manager) startBackup(ctx context.Context, c candidate, now time.Time) {
	rs := runSpec{job: c.job, run: newRunID(now), trigger: triggerSchedule, scheduled: c.due}
	switch {
	case c.follow != nil:
		rs.run, rs.trigger, rs.apps = c.follow.Run, triggerDeploy, c.follow.Apps
	case c.manual:
		rs.trigger = triggerManual
	}
	err := m.state.update(func(d *stateData) {
		js := d.job(c.job.ID)
		js.PostponedSince = 0
		js.Running = rs.run
		if c.follow != nil {
			return
		}
		// A whole run answers a manual request, and a regular slot that is
		// due as well, so neither runs again right after.
		js.Manual = 0
		if js.Next <= ms(now) {
			js.Next = ms(nextAfter(fromMs(js.Next), time.Duration(c.job.Every)*time.Minute, now))
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
	m.mu.Lock()
	m.backupSlot = true
	m.busy[c.job.Target] = KindBackup
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer m.free(c.job.Target, KindBackup)
		m.runJob(ctx, rs)
	}()
}

// free gives a slot and its target back and wakes the schedule.
func (m *Manager) free(target, kind string) {
	m.mu.Lock()
	if m.busy[target] == kind {
		delete(m.busy, target)
	}
	switch kind {
	case KindBackup, KindRestore:
		m.backupSlot = false
	case KindMaintain:
		m.maintSlot = false
	}
	m.signalFreedLocked()
	m.mu.Unlock()
	m.poke()
}

// signalFreedLocked wakes whoever waits for a slot. Caller holds m.mu.
func (m *Manager) signalFreedLocked() {
	close(m.freed)
	m.freed = make(chan struct{})
}

// scheduleMaintenance starts the most overdue maintenance, one at a time
// and never on a target a backup holds. A target that has deleted backups
// waiting to be purged is due at once.
func (m *Manager) scheduleMaintenance(ctx context.Context, jobs []settings.JobView, retired []settings.RetiredView, now time.Time, next *time.Time) {
	m.mu.Lock()
	busy := m.maintSlot
	m.mu.Unlock()
	if busy {
		return
	}
	targets := map[string]bool{}
	for _, j := range jobs {
		targets[j.Target] = true
	}
	purge := map[string]bool{}
	for _, r := range retired {
		targets[r.Target] = true
		purge[r.Target] = purge[r.Target] || r.Purge
	}
	type due struct {
		target string
		at     time.Time
	}
	var dues []due
	m.state.view(func(d *stateData) {
		for t := range targets {
			at := time.Time{}
			ts := d.Targets[t]
			if ts != nil && ts.NextMaintain > 0 {
				at = fromMs(ts.NextMaintain)
			}
			// Deletions the operator asked for do not wait for the
			// schedule, but a target that keeps failing is not tried
			// more often than a deferred run.
			if purge[t] {
				soon := now
				if ts != nil && ts.LastMaintain > 0 {
					soon = fromMs(ts.LastMaintain).Add(m.t.dockerRetry)
				}
				if at.IsZero() || soon.Before(at) {
					at = soon
				}
			}
			if at.IsZero() {
				continue
			}
			if at.After(now) {
				if at.Before(*next) {
					*next = at
				}
				continue
			}
			dues = append(dues, due{t, at})
		}
	})
	slices.SortFunc(dues, func(a, b due) int { return a.at.Compare(b.at) })
	for _, d := range dues {
		m.mu.Lock()
		if m.busy[d.target] != "" || m.restoreWait[d.target] > 0 {
			m.mu.Unlock()
			continue
		}
		m.maintSlot = true
		m.busy[d.target] = KindMaintain
		m.workers.Add(1)
		m.mu.Unlock()
		go func(target string) {
			defer m.workers.Done()
			defer m.free(target, KindMaintain)
			m.maintain(ctx, target)
		}(d.target)
		return
	}
}

// maintain runs full maintenance on a target, then tidies it with the
// space decisions, which belong after maintenance because only then is the
// garbage measured. A target with garbage still coming back, or that just
// lost backups, is maintained again sooner: under SafetyFull space returns
// only after several full runs hours apart.
func (m *Manager) maintain(ctx context.Context, target string) {
	start := m.now()
	rec := RunRecord{Kind: KindMaintain, Target: target, Start: ms(start)}
	res, err := m.call(ctx, target, true, func(req *proto.Request) {
		req.Op, req.Maintain = proto.OpMaintain, &proto.Maintain{Full: true}
	}, proto.Handler{})
	if errReleasedOr(err) || ctx.Err() != nil {
		return
	}
	var tr tidyResult
	if err == nil {
		tr = m.tidy(ctx, target, res)
		if errReleasedOr(tr.err) || ctx.Err() != nil {
			return
		}
		err = tr.err
	}
	m.afterMaintain(target, &rec, res, tr, err)
}

func errReleasedOr(err error) bool {
	return err != nil && (errors.Is(err, runner.ErrReleased) || errors.Is(err, errReleased))
}

// afterMaintain records a maintenance and schedules the next one.
func (m *Manager) afterMaintain(target string, rec *RunRecord, res *proto.Result, tr tidyResult, err error) {
	now := m.now()
	rec.End, rec.Deleted = ms(now), tr.deleted
	rec.Status = StatusOK
	if err != nil {
		rec.Status, rec.Error = StatusFailed, err.Error()
	}
	if len(tr.notes) > 0 {
		rec.Apps = nil
		msg := ""
		for i, n := range tr.notes {
			if i > 0 {
				msg += "; "
			}
			msg += n
		}
		if rec.Error == "" {
			rec.Error = msg
		} else {
			rec.Error += "; " + msg
		}
	}
	serr := m.state.update(func(d *stateData) {
		t := d.target(target)
		t.LastMaintain = ms(now)
		soon := err != nil || tr.deleted > 0
		if res != nil {
			t.Used = res.Bytes
			if sp := res.Space; sp != nil {
				t.Free, t.Total = sp.Free, sp.Total
			}
		}
		if sp := tr.space; sp != nil {
			t.Used, t.Garbage, t.Free, t.Total = tr.used, sp.Garbage, sp.Free, sp.Total
			soon = soon || sp.Garbage > 0
		}
		wait := m.t.maintainDue
		if soon {
			wait = m.t.maintainSoon
		}
		t.NextMaintain = ms(now.Add(wait))
		d.History = append(d.History, *rec)
	})
	if serr != nil {
		m.log.Error("Failed to save the backup state: %s", serr.Error())
	}
	m.report("", rec)
}
