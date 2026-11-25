package backup

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/thin"
	"odac/internal/lang"
)

// tr is lang.T: format is the catalog key, with %s for every argument.
func tr(format string, a ...any) string { return lang.T(format, a...) }

// snapState is what the manager keeps about snapshots in its state file.
type snapState struct {
	// Periodic is the schedule of each app's periodic snapshots.
	Periodic map[string]*periodicState `json:"periodic,omitempty"`
	// Gone holds, for each deleted app with snapshots, when it was
	// deleted: its snapshots are kept for a while after.
	Gone map[string]int64 `json:"gone,omitempty"`
	// Growth is what each series' last snapshot added to the repository,
	// the estimate of the next one.
	Growth map[string]int64 `json:"growth,omitempty"`
	// Index is the repository's restore points as the last listing found
	// them, which the API answers from without a helper run.
	Index []SnapshotPoint `json:"index,omitempty"`
	// LastDeploy is how each app's last deploy went for its snapshot.
	LastDeploy map[string]*DeployMark `json:"lastDeploy,omitempty"`
	Warnings   []SnapshotWarning      `json:"warnings,omitempty"`
	History    []SnapshotRecord       `json:"history,omitempty"`
}

func (s *snapState) init() {
	if s.Periodic == nil {
		s.Periodic = map[string]*periodicState{}
	}
	if s.Gone == nil {
		s.Gone = map[string]int64{}
	}
	if s.Growth == nil {
		s.Growth = map[string]int64{}
	}
	if s.LastDeploy == nil {
		s.LastDeploy = map[string]*DeployMark{}
	}
}

type periodicState struct {
	Next int64 `json:"next"`
	// Every is the interval in milliseconds; Duration the last
	// snapshot's, which sets it.
	Every          int64 `json:"every,omitempty"`
	Duration       int64 `json:"duration,omitempty"`
	Failures       int   `json:"failures,omitempty"`
	PostponedSince int64 `json:"postponedSince,omitempty"`
}

// SnapshotRecord is one snapshot taken or given up, or one maintenance or
// verification of the snapshot repository (Kind). Times are Unix milliseconds.
type SnapshotRecord struct {
	Kind string `json:"kind"`
	Run  string `json:"run,omitempty"`
	// App is the app the snapshot is filed under; Apps are what it took.
	App    string `json:"app,omitempty"`
	Event  string `json:"event,omitempty"`
	Status string `json:"status"`
	// Reason says why a snapshot was skipped, deferred or given up.
	Reason string      `json:"reason,omitempty"`
	Start  int64       `json:"start"`
	End    int64       `json:"end"`
	Apps   []AppResult `json:"apps,omitempty"`
	Server *AppResult  `json:"server,omitempty"`
	// Snapshots and Bytes are what was stored and read; Added is what the
	// repository grew by.
	Snapshots int   `json:"snapshots,omitempty"`
	Bytes     int64 `json:"bytes,omitempty"`
	Added     int64 `json:"added,omitempty"`
	// Deleted counts the snapshots thinning and the budget removed.
	Deleted int    `json:"deleted,omitempty"`
	Error   string `json:"error,omitempty"`
	Adopted bool   `json:"adopted,omitempty"`
	// Verify is what a verification (Kind verify) checked and found.
	Verify *VerifyReport `json:"verify,omitempty"`
}

// DeployMark is how an app's last deploy went for its snapshot: a deploy
// that went on without one is marked so.
type DeployMark struct {
	App    string `json:"app"`
	At     int64  `json:"at"`
	Status string `json:"status"`
	Run    string `json:"run,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SnapshotWarning is a condition the Hub shows until it clears.
type SnapshotWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

// SnapshotPoint is one snapshot a restore can return to. Times are Unix
// milliseconds.
type SnapshotPoint struct {
	Run   string            `json:"run"`
	App   string            `json:"app"`
	Event string            `json:"event"`
	Time  int64             `json:"time"`
	Apps  []RestorePointApp `json:"apps"`
	// Server says it holds ODAC's own state.
	Server bool  `json:"server,omitempty"`
	Bytes  int64 `json:"bytes,omitempty"`
	// Gone is when the app was deleted, and Expires when its snapshots
	// go: until then it can be brought back as a new app.
	Gone    int64 `json:"gone,omitempty"`
	Expires int64 `json:"expires,omitempty"`
}

// recordSnapshot keeps a record, reports it and marks a deploy's outcome.
func (m *Manager) recordSnapshot(rec SnapshotRecord) {
	// Snapshots that are off, and a schedule finding nothing on disk,
	// are not news.
	if rec.Status == StatusSkipped && (rec.Reason == ReasonDisabled || rec.Reason == ReasonNoData && rec.Event == EventPeriodic) {
		return
	}
	err := m.state.update(func(d *stateData) {
		d.Snap.History = append(d.Snap.History, rec)
		if rec.Kind == KindSnapshot && rec.Event == EventDeploy {
			mark := &DeployMark{App: rec.App, At: rec.Start, Status: rec.Status, Reason: rec.Reason}
			if rec.Status == StatusOK {
				mark.Run = rec.Run
			}
			d.Snap.LastDeploy[rec.App] = mark
		}
		if p := d.Snap.Periodic[rec.App]; p != nil && rec.Kind == KindSnapshot && rec.Status == StatusOK && rec.Event != EventPeriodic && p.Every > 0 {
			// A snapshot of the same group stands for the periodic one.
			p.Next = max(p.Next, rec.End+p.Every)
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
	what := "Snapshot " + rec.Run + " (" + rec.Event + ") of " + rec.App
	switch rec.Kind {
	case KindMaintain:
		what = "Maintenance of the snapshot repository"
	case KindVerify:
		what = "Verification of the snapshot repository"
	}
	switch rec.Status {
	case StatusOK:
		m.log.Log("%s finished", what)
	case StatusSkipped:
		if rec.Reason != ReasonNoData {
			m.log.Log("%s not taken: %s", what, rec.Error)
		}
	default:
		m.log.Warn("%s %s: %s", what, rec.Status, rec.Error)
	}
	if m.cfg.OnSnapshot != nil {
		m.cfg.OnSnapshot(rec)
	}
}

// warn raises a warning for the Hub, replacing one of the same code.
func (m *Manager) warn(code, message string) {
	m.state.update(func(d *stateData) {
		d.Snap.Warnings = slices.DeleteFunc(d.Snap.Warnings, func(w SnapshotWarning) bool { return w.Code == code })
		d.Snap.Warnings = append(d.Snap.Warnings, SnapshotWarning{Code: code, Message: message, At: ms(m.now())})
	})
}

func (m *Manager) clearWarning(code string) {
	found := false
	m.state.view(func(d *stateData) {
		found = slices.ContainsFunc(d.Snap.Warnings, func(w SnapshotWarning) bool { return w.Code == code })
	})
	if found {
		m.state.update(func(d *stateData) {
			d.Snap.Warnings = slices.DeleteFunc(d.Snap.Warnings, func(w SnapshotWarning) bool { return w.Code == code })
		})
	}
}

// thinEvent is a snapshot's event as thinning sees it: periodic snapshots
// are one series, the rest one series per event.
func thinEvent(trigger string) string {
	if trigger == EventPeriodic {
		return ""
	}
	return trigger
}

// tidySnapshots applies the snapshot rules to the repository: the last
// few of each event per app, the periodic ones thinned for a week, a
// deleted app's for a while after its delete. Then the budget: what the
// repository holds beyond its share of the disk, with extra more bytes
// about to be written, goes in SqueezeSnapshots' order, and below the
// floor every snapshot can go, since the apps come first. The budget's
// measurement walks every snapshot, so it runs only when the budget is at
// stake or, after maintenance, when garbage was still coming back. The
// listing becomes the index the API answers from.
func (m *Manager) tidySnapshots(ctx context.Context, maint *proto.Result, extra int64) (out tidyResult) {
	snaps, err := m.list(ctx, SnapshotTarget)
	if err != nil {
		out.err = err
		return out
	}
	runs := groupRuns(snaps)
	now := m.now()
	live := map[string]bool{serverApp: true}
	for _, a := range m.cfg.Apps.BackupApps() {
		live[a.Name] = true
	}
	series := map[string]bool{}
	for _, r := range runs {
		series[r.job] = true
	}
	gone := map[string]time.Time{}
	m.state.update(func(d *stateData) {
		for s := range series {
			if !live[s] && d.Snap.Gone[s] == 0 {
				d.Snap.Gone[s] = ms(now)
			}
		}
		for s, at := range d.Snap.Gone {
			if live[s] || !series[s] {
				delete(d.Snap.Gone, s)
				continue
			}
			gone[s] = fromMs(at)
		}
		for s := range d.Snap.Growth {
			if !series[s] && !live[s] {
				delete(d.Snap.Growth, s)
			}
		}
		for app := range d.Snap.LastDeploy {
			if !live[app] {
				delete(d.Snap.LastDeploy, app)
			}
		}
	})

	list := make([]thin.Snapshot, 0, len(runs))
	for _, r := range runs {
		list = append(list, thin.Snapshot{ID: r.id, App: r.job, Event: thinEvent(r.trigger), Time: r.time})
	}
	doomed := map[string]bool{}
	for _, id := range thin.ThinSnapshots(now, list, gone, thin.DefaultSnapshotRules) {
		doomed[id] = true
	}
	if err := m.deleteRuns(ctx, SnapshotTarget, runs, doomed); err != nil {
		out.err = err
		return out
	}
	out.deleted = len(doomed)
	kept := slices.DeleteFunc(list, func(s thin.Snapshot) bool { return doomed[s.ID] })

	pct := m.snapshotPct()
	space := m.snapSpace()
	if space.ok && space.Free < thin.Floor(space.Total) {
		m.warn(ReasonFloor, tr("The disk is below its free space floor; snapshots are deleted to give the apps room."))
	} else {
		m.clearWarning(ReasonFloor)
	}
	if snapshotExcess(space, pct, extra) > 0 || maint != nil && space.Garbage > 0 {
		n, notes, err := m.squeezeSnapshots(ctx, now, kept, runs, pct, extra)
		out.deleted += n
		out.notes = notes
		if err != nil {
			out.err = err
		}
	}
	m.writeIndex(snaps, doomed)
	return out
}

// squeezeSnapshots measures the repository and deletes the shortest
// prefix of SqueezeSnapshots' order that brings it within its budget, with
// extra more bytes to come. The measurement's garbage replaces the
// estimate, and what the deletion turns into garbage is added to it, so
// the next decision does not delete again for space on its way back.
func (m *Manager) squeezeSnapshots(ctx context.Context, now time.Time, kept []thin.Snapshot, runs map[string]*runInfo, pct int, extra int64) (int, []string, error) {
	order := thin.SqueezeSnapshots(now, kept)
	groups := make([][]string, len(order))
	for i, id := range order {
		groups[i] = runs[id].ids
	}
	res, err := m.usage(ctx, SnapshotTarget, &proto.Usage{Order: groups})
	if err != nil {
		return 0, nil, err
	}
	sp := res.Space
	if sp == nil || !sp.Detailed || len(sp.Freed) != len(order) {
		return 0, nil, errors.New("backup: odac-backup did not measure the snapshot repository")
	}
	garbage := sp.Garbage
	space := m.snapSpace()
	space.Garbage = min(garbage, space.Used)
	excess := snapshotExcess(space, pct, extra)
	var notes []string
	n := 0
	if excess > 0 && len(order) > 0 {
		n = slices.IndexFunc(sp.Freed, func(f proto.Freed) bool { return f.Repo >= excess }) + 1
		if n == 0 {
			n = len(order)
		}
		doomed := map[string]bool{}
		for _, id := range order[:n] {
			doomed[id] = true
		}
		if err := m.deleteRuns(ctx, SnapshotTarget, runs, doomed); err != nil {
			return 0, notes, err
		}
		garbage += sp.Freed[n-1].Repo
		notes = append(notes, tr("deleted %s snapshots to keep them within their share of the disk", strconv.Itoa(n)))
		if n == len(order) {
			m.warn(ReasonNoSpace, tr("Every snapshot was deleted to keep the disk's free space; new ones are taken again once there is room."))
		}
	}
	m.state.update(func(d *stateData) {
		t := d.target(SnapshotTarget)
		t.Garbage, t.Used = garbage, res.Bytes
	})
	return n, notes, nil
}

// writeIndex keeps the repository's restore points, without the doomed
// runs, for the API.
func (m *Manager) writeIndex(snaps []proto.Snapshot, doomed map[string]bool) {
	byRun := map[string]*SnapshotPoint{}
	levels := map[string]map[string][]string{}
	for _, s := range snaps {
		if s.Run == "" || doomed[s.Run] {
			continue
		}
		p := byRun[s.Run]
		if p == nil {
			p = &SnapshotPoint{Run: s.Run, App: s.Source.Job, Event: s.Trigger, Time: ms(s.Start)}
			byRun[s.Run] = p
			levels[s.Run] = map[string][]string{}
		}
		p.Time = min(p.Time, ms(s.Start))
		p.Bytes += s.Bytes
		if s.Source.App == serverApp {
			p.Server = true
			continue
		}
		if s.Source.Name == definitionSource {
			continue
		}
		levels[s.Run][s.Source.App] = append(levels[s.Run][s.Source.App], s.Consistency)
		i := slices.IndexFunc(p.Apps, func(a RestorePointApp) bool { return a.App == s.Source.App })
		if i < 0 {
			p.Apps = append(p.Apps, RestorePointApp{App: s.Source.App})
			i = len(p.Apps) - 1
		}
		if s.Kind == "stream" {
			p.Apps[i].Dump = true
		}
	}
	index := make([]SnapshotPoint, 0, len(byRun))
	for run, p := range byRun {
		for i := range p.Apps {
			p.Apps[i].Consistency = weakest(levels[run][p.Apps[i].App])
		}
		slices.SortFunc(p.Apps, func(a, b RestorePointApp) int { return strings.Compare(a.App, b.App) })
		index = append(index, *p)
	}
	slices.SortFunc(index, func(a, b SnapshotPoint) int {
		return cmp.Or(cmp.Compare(b.Time, a.Time), strings.Compare(b.Run, a.Run))
	})
	m.state.update(func(d *stateData) { d.Snap.Index = index })
	m.mu.Lock()
	m.snapListed = true
	m.mu.Unlock()
}

// scheduleSnapshots starts the periodic snapshot that is due, one at a
// time and never beside a restore. Each app with data gets them (see
// periodicRoots), about ten times its last one's duration apart. The
// first look in a start reads the repository's index instead.
func (m *Manager) scheduleSnapshots(ctx context.Context, now time.Time, next *time.Time) {
	if m.cfg.ServerID == "" || m.settings() == nil {
		return
	}
	m.mu.Lock()
	busy := m.snapSlot || m.snapQueued || m.busy[SnapshotTarget] != "" || m.restore != nil || m.restoreWait[SnapshotTarget] > 0 || m.released
	listed := m.snapListed
	m.mu.Unlock()
	if busy {
		return
	}
	if !listed && m.snapshotStoreExists() && now.After(m.snapListRetry) {
		m.queueSnapshotWork(ctx, func(ctx context.Context) {
			release, err := m.acquireSnapshot(ctx, false, SnapshotRunning{})
			if err != nil {
				return
			}
			defer release()
			if tr := m.tidySnapshots(ctx, nil, 0); tr.err != nil && !errReleasedOr(tr.err) {
				m.log.Warn("Cannot read the snapshot repository: %s", tr.err.Error())
				m.mu.Lock()
				m.snapListRetry = m.now().Add(m.t.dockerRetry)
				m.mu.Unlock()
			}
		})
		return
	}
	if reason, _ := m.snapshotsOff(); reason != "" {
		return
	}
	all := m.cfg.Apps.BackupApps()
	roots := periodicRoots(all)
	isRoot := map[string]bool{}
	for _, r := range roots {
		isRoot[r] = true
	}
	changed := false
	m.state.view(func(d *stateData) {
		for _, r := range roots {
			changed = changed || d.Snap.Periodic[r] == nil
		}
		for app := range d.Snap.Periodic {
			changed = changed || !isRoot[app]
		}
	})
	if changed {
		m.state.update(func(d *stateData) {
			for _, r := range roots {
				if d.Snap.Periodic[r] == nil {
					d.Snap.Periodic[r] = &periodicState{Next: ms(now.Add(m.t.snap.periodicFirst))}
				}
			}
			for app := range d.Snap.Periodic {
				if !isRoot[app] {
					delete(d.Snap.Periodic, app)
				}
			}
		})
	}
	var pick string
	var due, postponed int64
	m.state.view(func(d *stateData) {
		for _, r := range roots {
			p := d.Snap.Periodic[r]
			if p.Next > ms(now) {
				if t := fromMs(p.Next); t.Before(*next) {
					*next = t
				}
				continue
			}
			if pick == "" || p.Next < due {
				pick, due, postponed = r, p.Next, p.PostponedSince
			}
		}
	})
	if pick == "" {
		return
	}
	if m.cfg.Load != nil && loaded(m.cfg.Load()) && (postponed == 0 || now.Sub(fromMs(postponed)) < m.t.maxPostpone) {
		if postponed == 0 {
			m.state.update(func(d *stateData) { d.Snap.Periodic[pick].PostponedSince = ms(now) })
		}
		if t := now.Add(m.t.loadRetry); t.Before(*next) {
			*next = t
		}
		return
	}
	var every int64
	m.state.view(func(d *stateData) { every = d.Snap.Periodic[pick].Every })
	minutes := int(max(time.Duration(every), m.t.snap.periodicMin) / time.Minute)
	group := snapshotGroup(pick, all)
	m.queueSnapshotWork(ctx, func(ctx context.Context) {
		rec := m.takeSnapshot(ctx, snapRequest{series: pick, event: EventPeriodic, apps: group, reuse: true, every: max(minutes, 1)})
		m.afterPeriodic(pick, rec)
	})
}

// queueSnapshotWork runs fn in a tracked worker; the scheduler starts no
// other snapshot work until it returns.
func (m *Manager) queueSnapshotWork(ctx context.Context, fn func(context.Context)) {
	m.mu.Lock()
	if m.released {
		m.mu.Unlock()
		return
	}
	m.snapQueued = true
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer func() {
			m.mu.Lock()
			m.snapQueued = false
			m.mu.Unlock()
			m.poke()
		}()
		fn(ctx)
	}()
}

// afterPeriodic schedules an app's next periodic snapshot: about ten
// times this one's duration later, within the bounds, so a large
// database is not under a dump most of the time.
func (m *Manager) afterPeriodic(app string, rec SnapshotRecord) {
	now := m.now()
	st := m.t.snap
	m.state.update(func(d *stateData) {
		p := d.Snap.Periodic[app]
		if p == nil {
			return
		}
		p.PostponedSince = 0
		switch rec.Status {
		case StatusOK:
			p.Failures = 0
			p.Duration = rec.End - rec.Start
			every := min(max(10*time.Duration(p.Duration)*time.Millisecond, st.periodicMin), st.periodicMax)
			p.Every = int64(every / time.Millisecond)
			p.Next = rec.End + p.Every
		case StatusDeferred:
			wait := m.t.dockerRetry
			if rec.Reason == ReasonDeploy {
				wait = m.t.deploySettle
			}
			p.Next = ms(now.Add(wait))
		case StatusFailed:
			p.Failures++
			p.Next = ms(now.Add(min(backoff(p.Failures), st.periodicMax)))
		default:
			p.Next = ms(now.Add(st.periodicMin))
		}
	})
}

// adoptedSnapshot records a snapshot the previous server started. It is
// whole only when that server marked its last piece as the end of a
// snapshot that had gone well; anything less is deleted.
func (m *Manager) adoptedSnapshot(ctx context.Context, l map[string]string, snaps []proto.Snapshot, err error) {
	run := l[runner.LabelRun]
	now := m.now()
	rec := SnapshotRecord{Kind: KindSnapshot, Run: run, App: l[labelJob], Event: l[labelTrigger], Status: StatusOK, Start: ms(now), Adopted: true}
	for _, s := range snaps {
		rec.Snapshots++
		rec.Bytes += s.Bytes
		rec.Start = min(rec.Start, ms(s.Start))
	}
	for _, a := range strings.Split(l[labelApps], ",") {
		if a != "" {
			rec.Apps = append(rec.Apps, AppResult{App: a, Status: StatusOK})
		}
	}
	if fin := l[labelFinish]; fin != "" {
		for _, app := range strings.Split(fin, ",") {
			m.finishAdopted(ctx, app, run)
		}
	}
	switch {
	case err != nil:
		rec.Status, rec.Error = StatusFailed, err.Error()
	case l[labelComplete] != "true":
		rec.Status, rec.Error = StatusFailed, tr("an update interrupted the snapshot")
	}
	if rec.Status != StatusOK {
		if len(snaps) > 0 {
			m.deleteRuns(ctx, SnapshotTarget, groupRuns(snaps), map[string]bool{run: true})
		}
		rec.Snapshots, rec.Bytes = 0, 0
		m.maintainSoon(true)
	} else {
		rec.Deleted = m.tidySnapshots(ctx, nil, 0).deleted
	}
	rec.End = ms(m.now())
	m.recordSnapshot(rec)
}
