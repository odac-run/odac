package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
)

// tidyLimit bounds the helper runs of one tidy pass.
const tidyLimit = 2 * time.Hour

// runInfo is one job run as the repository holds it: the unit thinning
// keeps or deletes, so an app and its linked database never come back from
// different moments.
type runInfo struct {
	id      string
	job     string
	trigger string
	time    time.Time
	ids     []string
	bytes   int64
}

// groupRuns turns snapshots into runs, each at the start of its first
// snapshot.
func groupRuns(snaps []proto.Snapshot) map[string]*runInfo {
	out := map[string]*runInfo{}
	for _, s := range snaps {
		if s.Run == "" {
			continue
		}
		r := out[s.Run]
		if r == nil {
			r = &runInfo{id: s.Run, job: s.Source.Job, trigger: s.Trigger, time: s.Start}
			out[s.Run] = r
		}
		if s.Start.Before(r.time) {
			r.time = s.Start
		}
		r.ids = append(r.ids, s.ID)
		r.bytes += s.Bytes
	}
	return out
}

// tidyResult is what a tidy pass did.
type tidyResult struct {
	deleted int
	space   *proto.Space
	used    int64
	// jobs is what each job of the target references after the space
	// decisions, set only when they measured the repository.
	jobs  map[string]int64
	notes []string
	err   error
}

// tidy deletes what the rules say goes from a target: every run of a job
// whose deletion the operator asked for, what each job's density and
// maximum age thin away, and partial runs a newer complete run of their
// job supersedes. After maintenance (maint is its result) it then measures
// the target and deletes for its free space floor and for the jobs'
// budgets, when one of them can be at stake or garbage was still coming
// back at the last measurement: that walk of every snapshot costs about
// what garbage collection costs. A listing that fails deletes nothing,
// and runs of jobs without a policy
// (kept backups of deleted jobs, jobs this server no longer knows) are
// never touched.
func (m *Manager) tidy(ctx context.Context, target string, maint *proto.Result) (out tidyResult) {
	if target == SnapshotTarget {
		return m.tidySnapshots(ctx, maint, 0)
	}
	set := m.settings()
	jobs, err := set.Jobs()
	if err != nil {
		out.err = err
		return out
	}
	retired, err := set.Retired()
	if err != nil {
		out.err = err
		return out
	}
	policies := map[string]thin.Policy{}
	budgets := map[string]int64{}
	names := map[string]string{}
	for _, j := range jobs {
		if j.Target == target {
			policies[j.ID] = j.Policy()
			names[j.ID] = j.Name
			if j.Budget > 0 {
				budgets[j.ID] = j.Budget
			}
		}
	}
	purge := map[string]bool{}
	for _, r := range retired {
		if r.Target == target && r.Purge {
			purge[r.ID] = true
		}
	}

	snaps, err := m.list(ctx, target)
	if err != nil {
		out.err = err
		return out
	}
	runs := groupRuns(snaps)
	var garbage int64
	partial, held := map[string]bool{}, map[string]bool{}
	m.state.view(func(d *stateData) {
		if t := d.Targets[target]; t != nil {
			garbage = t.Garbage
			for _, r := range t.Partial {
				partial[r] = true
			}
		}
		for _, f := range d.FollowUps {
			held[f.Run] = true
		}
	})

	now := m.now()
	doomed := map[string]bool{}
	var series []thin.Backup
	newest := map[string]time.Time{}
	for _, r := range runs {
		_, thinned := policies[r.job]
		switch {
		case purge[r.job]:
			doomed[r.id] = true
		case partial[r.id]:
		case thinned:
			series = append(series, thin.Backup{ID: r.id, Job: r.job, Time: r.time})
			if r.time.After(newest[r.job]) {
				newest[r.job] = r.time
			}
		}
	}
	for _, id := range thin.Thin(now, series, policies) {
		doomed[id] = true
	}
	for _, id := range legacySafety(now, runs) {
		doomed[id] = true
	}
	for _, r := range runs {
		// A run whose rest a deploy held back is not partial for good.
		if _, thinned := policies[r.job]; thinned && partial[r.id] && !held[r.id] && r.time.Before(newest[r.job]) {
			doomed[r.id] = true
		}
	}
	if err := m.deleteRuns(ctx, target, runs, doomed); err != nil {
		out.err = err
		return out
	}
	out.deleted = len(doomed)
	for id := range purge {
		if err := set.Purged(id); err != nil {
			m.log.Error("Failed to record the deleted backups of job %s: %s", id, err.Error())
		}
	}

	if maint != nil && (len(budgets) > 0 || garbage > 0 || belowFloor(maint.Space)) {
		kept := slices.DeleteFunc(series, func(b thin.Backup) bool { return doomed[b.ID] })
		sq := m.squeeze(ctx, target, kept, runs, policies, budgets, names)
		sq.deleted += out.deleted
		out = sq
	} else if maint == nil {
		m.checkFloor(ctx, target)
	}

	m.state.update(func(d *stateData) {
		t := d.target(target)
		t.Partial = slices.DeleteFunc(t.Partial, func(r string) bool {
			_, there := runs[r]
			return doomed[r] || !there && !d.followUpHolds(r)
		})
	})
	return out
}

// legacySafety are the safety backups before an in-place restore that
// went to the restore's target before snapshots existed, under the job
// safetyJob, which has no policy. They are kept the way snapshots of an
// event are: the last few, for as long as a deleted app's snapshots.
func legacySafety(now time.Time, runs map[string]*runInfo) []string {
	var list []*runInfo
	for _, r := range runs {
		if r.job == safetyJob {
			list = append(list, r)
		}
	}
	slices.SortFunc(list, func(a, b *runInfo) int { return a.time.Compare(b.time) })
	rules := thin.DefaultSnapshotRules
	var out []string
	for i, r := range list {
		if i < len(list)-rules.KeepEvents || now.Sub(r.time) > rules.Gone {
			out = append(out, r.id)
		}
	}
	return out
}

// followUpHolds reports a run whose rest is still to come.
func (d *stateData) followUpHolds(run string) bool {
	for _, f := range d.FollowUps {
		if f.Run == run {
			return true
		}
	}
	return false
}

// squeeze deletes for space: what the free space floor lacks once the
// garbage is back, and what each job references above its budget. The
// candidates come in thin.Squeeze's order, least harmful first, and the
// engine reports what each prefix of that order frees, so only the
// shortest prefix that is enough goes. Nothing is decided on a measurement
// that ignores the garbage, which under SafetyFull takes days to come back:
// deciding again on the measured size alone would delete again for space
// already on its way back.
func (m *Manager) squeeze(ctx context.Context, target string, series []thin.Backup, runs map[string]*runInfo, policies map[string]thin.Policy, budgets map[string]int64, names map[string]string) (out tidyResult) {
	jobs := map[string]bool{}
	for j := range policies {
		jobs[j] = true
	}
	order := thin.Squeeze(m.now(), series, jobs)
	groups := make([][]string, len(order))
	for i, id := range order {
		groups[i] = runs[id].ids
	}
	res, err := m.call(ctx, target, m.sharesTarget(target), func(req *proto.Request) {
		req.Op, req.Usage = proto.OpUsage, &proto.Usage{Order: groups}
		req.Share = shares(req)
	}, proto.Handler{})
	if err != nil {
		out.err = err
		return out
	}
	sp := res.Space
	out.space, out.used = sp, res.Bytes
	if sp == nil || !sp.Detailed || len(sp.Freed) != len(order) {
		out.err = errors.New("backup: odac-backup did not measure the repository")
		return out
	}

	var notes []string
	doomed := map[string]bool{}
	space := thin.Space{Used: res.Bytes, Garbage: sp.Garbage, Free: sp.Free, Total: sp.Total, Peers: m.activePeers(sp.Peers, m.now())}
	excess := space.Excess()
	floorNote := "deleted %d backups to keep the target's free space floor"
	if len(space.Peers) > 0 {
		floorNote = "deleted %d backups to keep the target's free space floor: this server holds more than its fair share of the target"
	}
	if excess > 0 {
		n := slices.IndexFunc(sp.Freed, func(f proto.Freed) bool { return f.Repo >= excess }) + 1
		if n == 0 {
			n = len(order)
			notes = append(notes, fmt.Sprintf("the target stays %d bytes short of its free space floor with only each job's newest backup left", excess-lastRepo(sp.Freed)))
		}
		for _, id := range order[:n] {
			doomed[id] = true
		}
		if n > 0 {
			notes = append(notes, fmt.Sprintf(floorNote, n))
		}
	}
	for job, budget := range budgets {
		over := sp.Jobs[job] - budget
		if over <= 0 {
			continue
		}
		n, enough := 0, false
		for i, id := range order {
			if runs[id].job != job {
				continue
			}
			doomed[id] = true
			n++
			if sp.Freed[i].Job >= over {
				enough = true
				break
			}
		}
		notes = append(notes, fmt.Sprintf("deleted %d backups of job %s to keep it within its budget", n, names[job]))
		if !enough {
			notes = append(notes, fmt.Sprintf("job %s stays above its budget with only its newest backup left", names[job]))
		}
	}
	out.notes = notes
	if err := m.deleteRuns(ctx, target, runs, doomed); err != nil {
		out.err = err
		out.jobs = jobUsage(sp, order, runs, nil, policies)
		return out
	}
	out.deleted = len(doomed)
	out.jobs = jobUsage(sp, order, runs, doomed, policies)
	return out
}

// jobUsage is what each job of the target references once the doomed runs
// are gone: the measured size less what the longest deleted prefix of the
// job's runs in the squeeze order frees. A run deleteRuns spared ends that
// prefix, so the figure errs high, never low.
func jobUsage(sp *proto.Space, order []string, runs map[string]*runInfo, doomed map[string]bool, policies map[string]thin.Policy) map[string]int64 {
	out := make(map[string]int64, len(policies))
	for job := range policies {
		out[job] = sp.Jobs[job]
	}
	ended := map[string]bool{}
	for i, id := range order {
		job := runs[id].job
		if ended[job] {
			continue
		}
		if !doomed[id] {
			ended[job] = true
			continue
		}
		out[job] = max(sp.Jobs[job]-sp.Freed[i].Job, 0)
	}
	return out
}

func lastRepo(f []proto.Freed) int64 {
	if len(f) == 0 {
		return 0
	}
	return f[len(f)-1].Repo
}

// checkFloor measures a filesystem target after a backup and brings its
// maintenance forward when its free space is under the floor, which is
// where space decisions are made.
func (m *Manager) checkFloor(ctx context.Context, target string) {
	a, err := m.access(target, settings.AccessOptions{})
	if err != nil || a.Repo.Storage.Filesystem == nil {
		return
	}
	res, err := m.usage(ctx, target, nil)
	if err != nil || res.Space == nil {
		return
	}
	sp := res.Space
	now := m.now()
	m.state.update(func(d *stateData) {
		t := d.target(target)
		t.Used, t.Free, t.Total = res.Bytes, sp.Free, sp.Total
		// On a shared target, a server the last maintenance found within
		// its share has nothing to give; another server's growth does not
		// call for this one's maintenance after every backup.
		within := t.Share > 0 && t.Used-t.Garbage <= t.Share && now.Sub(fromMs(t.LastMaintain)) < m.t.maintainSoon
		if belowFloor(sp) && !within {
			t.NextMaintain = ms(now)
		}
	})
}

func belowFloor(sp *proto.Space) bool {
	return sp != nil && sp.Free >= 0 && sp.Total > 0 && sp.Free < thin.Floor(sp.Total)
}

// list returns every snapshot of this server on the target.
func (m *Manager) list(ctx context.Context, target string) ([]proto.Snapshot, error) {
	var (
		mu    sync.Mutex
		snaps []proto.Snapshot
	)
	_, err := m.call(ctx, target, false, func(req *proto.Request) { req.Op, req.List = proto.OpList, &proto.List{} }, proto.Handler{
		Snapshot: func(s proto.Snapshot) {
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
		},
	})
	mu.Lock()
	defer mu.Unlock()
	return snaps, err
}

func (m *Manager) usage(ctx context.Context, target string, u *proto.Usage) (*proto.Result, error) {
	return m.call(ctx, target, false, func(req *proto.Request) { req.Op, req.Usage = proto.OpUsage, u }, proto.Handler{})
}

// deleteRuns deletes every snapshot of the doomed runs in one request,
// except a run a restore waits for or reads: thinning between a restore's
// plan and its turn would leave it nothing to restore.
func (m *Manager) deleteRuns(ctx context.Context, target string, runs map[string]*runInfo, doomed map[string]bool) error {
	m.mu.Lock()
	if r := m.restore; r != nil {
		r.mu.Lock()
		if r.result.Target == target {
			delete(doomed, r.result.Run)
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	var ids []string
	for id := range doomed {
		if r := runs[id]; r != nil {
			ids = append(ids, r.ids...)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	_, err := m.call(ctx, target, true, func(req *proto.Request) { req.Op, req.Delete = proto.OpDelete, &proto.Delete{IDs: ids} }, proto.Handler{})
	return err
}

// call runs one short request on a target. A write claims the next owner
// sequence number.
func (m *Manager) call(ctx context.Context, target string, write bool, shape func(*proto.Request), h proto.Handler) (*proto.Result, error) {
	return m.callFor(ctx, target, write, tidyLimit, shape, h)
}

// callFor is call with its own time limit.
func (m *Manager) callFor(ctx context.Context, target string, write bool, limit time.Duration, shape func(*proto.Request), h proto.Handler) (*proto.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := m.currentRunner()
	if err != nil {
		return nil, err
	}
	a, err := m.access(target, settings.AccessOptions{Write: write})
	if err != nil {
		return nil, err
	}
	req, err := m.request("", a, false)
	if err != nil {
		return nil, err
	}
	shape(req)
	runCtx, cancel := m.runContext(m.now().Add(limit))
	defer cancel()
	res, err := r.Run(runCtx, runner.Invocation{Request: req, Target: a.Target, Handler: h, Labels: map[string]string{labelKind: req.Op, labelTarget: target}})
	if err != nil && !errors.Is(err, runner.ErrReleased) {
		m.observe(target, err)
	}
	return res, err
}
