package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/docker"
)

// Triggers a snapshot is tagged with.
const (
	triggerSchedule = "schedule"
	// triggerDeploy is the rest of a run whose dumps a deploy cancelled.
	triggerDeploy = "deploy"
	// triggerManual is a run the operator asked for.
	triggerManual = "manual"
)

// serverApp is the app name ODAC's own state is stored under.
const serverApp = "odac"

// stopFinishLimit bounds undoing a plan's preparation while the server
// stops.
const stopFinishLimit = 10 * time.Second

// runSpec is one run to make.
type runSpec struct {
	job     settings.JobView
	run     string
	trigger string
	// apps restricts the run to these apps and leaves the server's state
	// out: the rest of a run a deploy cut short. Nil is the whole job.
	apps      []string
	scheduled time.Time
	// abort ends the run's helpers, whatever their deadline: a snapshot
	// given up by its deploy, by the operator or for space. It never
	// derives from the manager's context (see runContext).
	abort context.Context
	// exempt is a snapshot that is part of an operation on its apps (a
	// deploy, a delete): their deploy does not hold it back or cancel its
	// dump.
	exempt bool
	// noReuse reads every database afresh: a snapshot right before a
	// deploy or a delete stands for that moment, not for a dump stored
	// minutes before.
	noReuse bool
}

// pieceContext is a helper run's context: the deadline, and the run's
// abort when it has one.
func (m *Manager) pieceContext(rs runSpec, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := m.runContext(deadline)
	if rs.abort == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(rs.abort, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func newRunID(now time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// runLimit is a job's time limit: four intervals, within one to twelve
// hours. A run cut off by it keeps what it uploaded (Kopia's checkpoints
// stay in the repository and the next run deduplicates against them), so a
// first backup larger than one limit still completes over several runs.
func (m *Manager) runLimit(everyMinutes int) time.Duration {
	return max(m.t.runMin, min(m.t.runMax, 4*time.Duration(everyMinutes)*time.Minute))
}

// runContext is the context a helper run gets: the deadline, and nothing
// of the manager's own context. Stop releases the runner before it
// cancels, and a run that saw the cancel first would stop the helper the
// release meant to hand over. The deadline is on the manager's clock, so
// it becomes a timeout from now.
func (m *Manager) runContext(deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), deadline.Sub(m.now()))
}

// deferral is a piece that did not run for a reason that is not the
// backup's: Docker did not answer, or a deploy held the app.
type deferral struct {
	reason string
	deploy bool
}

// piece is one helper run: at most one dump, which comes first, then the
// directories.
type piece struct {
	apps  []string
	plan  *consistency.Plan // the dump's plan; nil for the files piece
	plans []*consistency.Plan
	items []proto.Item
	group map[string]bool
}

// appRun tracks one app through a run.
type appRun struct {
	res      AppResult
	plan     *consistency.Plan
	deferred *deferral
	levels   []string
}

// runJob makes one run of a job and records it. It returns early and
// records nothing when the runner was released: the helper in flight
// belongs to the next server, which reports it.
func (m *Manager) runJob(ctx context.Context, rs runSpec) {
	job := rs.job
	start := m.now()
	deadline := start.Add(m.runLimit(job.Every))
	// A timeout, not a deadline: the deadline is on the manager's clock.
	ctx, cancel := context.WithTimeout(ctx, deadline.Sub(start))
	defer cancel()

	all := m.cfg.Apps.BackupApps()
	names := rs.apps
	if names == nil {
		names = jobApps(job.Job, all)
	}
	rec := RunRecord{Kind: KindBackup, Run: rs.run, Job: job.ID, Target: job.Target, Trigger: rs.trigger, Start: ms(start)}

	apps := make([]*appRun, 0, len(names))
	for _, name := range names {
		a := &appRun{res: AppResult{App: name}}
		apps = append(apps, a)
		plan, d, err := m.plan(ctx, name, rs.run)
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return
		case d != nil:
			a.deferred = d
		case err != nil:
			a.res.Status, a.res.Error = StatusFailed, err.Error()
		case plan == nil:
			a.res.Status = StatusOK
		default:
			a.plan = plan
			a.res.Warnings = plan.Warnings
		}
	}

	var serverDirs []string
	if rs.apps == nil && job.Server {
		rec.Server = &AppResult{App: serverApp}
		for _, d := range m.cfg.ServerDirs {
			if isDir(d) {
				serverDirs = append(serverDirs, d)
			}
		}
	}

	pieces := m.pieces(job.ID, apps, all, serverDirs)
	cleanup := m.addDefinitions(rs.run, job.ID, pieces)
	defer func() {
		// A helper handed over still reads its definition directory; the
		// server that follows it removes the directory after its result.
		if !m.isReleased() {
			cleanup()
		}
	}()
	var snaps []proto.Snapshot
	failed := false
	for i, p := range pieces {
		labels := map[string]string{
			labelKind: KindBackup, labelJob: job.ID, labelTarget: job.Target,
			labelTrigger: rs.trigger, labelApps: strings.Join(p.apps, ","),
		}
		if i == len(pieces)-1 && !failed {
			labels[labelComplete] = "true"
		}
		pr := m.runPiece(ctx, rs, p, deadline, labels)
		snaps = append(snaps, pr.snaps...)
		if pr.handedOver || errors.Is(pr.err, errReleased) || ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return
		}
		if pr.err != nil || pr.deferred != nil {
			failed = true
		}
		for _, name := range p.apps {
			a := find(apps, name)
			if a == nil {
				continue
			}
			switch {
			case pr.deferred != nil:
				a.deferred = pr.deferred
			case pr.err != nil:
				a.res.Status, a.res.Error = StatusFailed, describe(ctx, pr.err)
			default:
				a.res.Status = StatusOK
				if pr.reused {
					a.res.Method = MethodReused
				}
			}
		}
		if p.plan == nil && rec.Server != nil {
			rec.Server.Status = StatusOK
			if pr.err != nil {
				rec.Server.Status, rec.Server.Error = StatusFailed, describe(ctx, pr.err)
			}
		}
	}
	if rec.Server != nil && rec.Server.Status == "" {
		rec.Server.Status = StatusOK
	}

	m.finishRun(ctx, rs, &rec, apps, snaps)
}

func describe(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "the run reached its time limit: " + err.Error()
	}
	return err.Error()
}

func find(apps []*appRun, name string) *appRun {
	for _, a := range apps {
		if a.res.App == name {
			return a
		}
	}
	return nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// jobApps are the apps a job takes, in order.
func jobApps(j settings.Job, all []settings.App) []string {
	if !j.AllApps {
		return slices.Clone(j.Apps)
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		out = append(out, a.Name)
	}
	slices.Sort(out)
	return out
}

// plan decides how one app is read. A Docker failure defers the app, since
// a stalled daemon must not turn a database into a torn file copy; an app
// whose container is not running is read as files, which nothing writes.
func (m *Manager) plan(ctx context.Context, name, run string) (*consistency.Plan, *deferral, error) {
	app, ok := m.cfg.Apps.BackupApp(name)
	if !ok {
		return nil, nil, fmt.Errorf("app %s no longer exists", name)
	}
	st, err := m.cfg.States.BackupAppState(ctx, app.Container)
	if err != nil {
		return nil, &deferral{reason: "Docker did not answer: " + err.Error()}, nil
	}
	var plan *consistency.Plan
	if !st.Running {
		plan = consistency.FilesPlan(app, consistency.CrashConsistent)
	} else {
		app.ImageID, app.Cmd, app.Env, app.MemoryLimit = st.ImageID, st.Cmd, st.Env, st.MemoryLimit
		if plan, err = m.cfg.Planner.Plan(ctx, app, run); err != nil {
			return nil, &deferral{reason: "the app could not be inspected: " + err.Error()}, nil
		}
	}
	dirs := plan.Dirs[:0]
	for _, d := range plan.Dirs {
		if d.Prepared || isDir(d.Path) {
			dirs = append(dirs, d)
			continue
		}
		plan.Warnings = append(plan.Warnings, consistency.Warning{Code: "missing_directory", Message: d.Container + " is not a directory this server can read"})
	}
	plan.Dirs = dirs
	if plan.Dump == nil && len(plan.Dirs) == 0 {
		// Nothing to read, but the declaration's state (ClickHouse) is
		// undone all the same.
		plan.Finish(ctx)
		return nil, nil, nil
	}
	return plan, nil, nil
}

// pieces splits a run into helper runs: one per dump, since a run reads at
// most one stream, with its app's directories after it; then one for every
// other directory, ODAC's own state included.
func (m *Manager) pieces(job string, apps []*appRun, all []settings.App, serverDirs []string) []*piece {
	var out []*piece
	files := &piece{}
	for _, a := range apps {
		p := a.plan
		if p == nil {
			continue
		}
		if p.Dump != nil {
			dp := &piece{apps: []string{a.res.App}, plan: p, plans: []*consistency.Plan{p}, group: dumpGroup(a.res.App, all)}
			dp.items = append(dp.items, proto.Item{Source: proto.Source{Job: job, App: a.res.App, Name: p.Dump.Name}, Consistency: p.Dump.Consistency, Stream: p.Dump.File})
			a.levels = append(a.levels, p.Dump.Consistency)
			for _, d := range p.Dirs {
				dp.items = append(dp.items, dirItem(job, a.res.App, d))
				a.levels = append(a.levels, d.Consistency)
			}
			a.res.Method = MethodDump
			out = append(out, dp)
			continue
		}
		files.apps = append(files.apps, a.res.App)
		files.plans = append(files.plans, p)
		for _, d := range p.Dirs {
			files.items = append(files.items, dirItem(job, a.res.App, d))
			a.levels = append(a.levels, d.Consistency)
		}
		a.res.Method = MethodFiles
	}
	for _, d := range serverDirs {
		files.items = append(files.items, proto.Item{
			Source:      proto.Source{Job: job, App: serverApp, Name: "server-" + filepath.Base(d)},
			Consistency: consistency.Live, Dir: d,
		})
	}
	if len(files.items) > 0 {
		out = append(out, files)
	}
	for _, a := range apps {
		a.res.Consistency = weakest(a.levels)
	}
	return out
}

// preparedItems drops the directories Prepare was to make and did not:
// ClickHouse makes no shadow directory for tables without parts, and
// there is nothing of theirs to read.
func preparedItems(items []proto.Item, plans []*consistency.Plan) []proto.Item {
	missing := map[string]bool{}
	for _, pl := range plans {
		for _, d := range pl.Dirs {
			if d.Prepared && !isDir(d.Path) {
				missing[d.Path] = true
			}
		}
	}
	if len(missing) == 0 {
		return items
	}
	return slices.DeleteFunc(slices.Clone(items), func(it proto.Item) bool { return it.Dir != "" && missing[it.Dir] })
}

func dirItem(job, app string, d consistency.Dir) proto.Item {
	return proto.Item{Source: proto.Source{Job: job, App: app, Name: d.Name}, Consistency: d.Consistency, Dir: d.Path}
}

// weakest is the level a set of pieces can promise together.
func weakest(levels []string) string {
	rank := map[string]int{consistency.Consistent: 0, consistency.CrashConsistent: 1, consistency.Live: 2}
	out := ""
	for _, l := range levels {
		if out == "" || rank[l] > rank[out] {
			out = l
		}
	}
	return out
}

// pieceResult is how one helper run went.
type pieceResult struct {
	snaps    []proto.Snapshot
	err      error
	deferred *deferral
	reused   bool
	// handedOver: the runner was released while the helper ran.
	handedOver bool
}

// runPiece prepares the piece's plans, runs it, and undoes the plans'
// preparation, unless the helper was handed over: it still reads what was
// prepared, and the next server finishes those plans after it.
func (m *Manager) runPiece(ctx context.Context, rs runSpec, p *piece, deadline time.Time, labels map[string]string) (out pieceResult) {
	if p.plan != nil && !rs.exempt {
		if idle, _ := m.groupIdle(p.group); !idle {
			out.deferred = &deferral{reason: "a deploy is in progress", deploy: true}
			return out
		}
	}
	var finish []string
	for _, pl := range p.plans {
		if pl.Engine == consistency.ClickHouse {
			finish = append(finish, pl.App)
		}
	}
	if len(finish) > 0 {
		labels[labelFinish] = strings.Join(finish, ",")
	}
	defer func() {
		if out.handedOver {
			return
		}
		// A server being stopped for an update has the handshake's time
		// limit to meet.
		limit := m.t.finishLimit
		if m.isReleased() {
			limit = min(limit, stopFinishLimit)
		}
		fctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		for _, pl := range p.plans {
			if err := pl.Finish(fctx); err != nil {
				m.log.Error("Failed to clean up after backing up %s: %s", pl.App, err.Error())
			}
		}
	}()
	for _, pl := range p.plans {
		if err := pl.Prepare(ctx); err != nil {
			out.err = err
			return out
		}
	}
	p.items = preparedItems(p.items, p.plans)
	run := func(runCtx context.Context, items []proto.Item, dump *docker.DumpSpec, source func(context.Context) (io.ReadCloser, error)) {
		out.snaps, out.err = m.invoke(runCtx, rs, items, dump, source, labels)
		out.handedOver = errors.Is(out.err, runner.ErrReleased)
	}

	if p.plan == nil {
		runCtx, cancel := m.pieceContext(rs, deadline)
		defer cancel()
		run(runCtx, p.items, nil, nil)
		return out
	}

	app := p.apps[0]
	if rec := m.reusable(app, p.plan, rs, p.group); rec != nil {
		items := slices.Clone(p.items)
		items[0].Consistency, items[0].Stream = rec.Consistency, rec.File
		runCtx, cancel := m.pieceContext(rs, deadline)
		run(runCtx, items, nil, m.storedDump(rec))
		cancel()
		if out.err == nil {
			out.reused = true
			m.log.Log("Reused the dump of %s stored %s ago instead of dumping it again", app, m.now().Sub(fromMs(rec.At)).Round(time.Second))
			return out
		}
		if out.handedOver || ctx.Err() != nil {
			return out
		}
		m.log.Warn("Could not reuse the stored dump of %s, dumping it again: %s", app, out.err.Error())
		out = pieceResult{snaps: out.snaps}
	}

	runCtx, cancel := m.pieceContext(rs, deadline)
	defer cancel()
	end := func() bool { return false }
	if !rs.exempt {
		var ok bool
		if end, ok = m.beginDump(app, p.group, cancel); !ok {
			out.deferred = &deferral{reason: "a deploy is in progress", deploy: true}
			return out
		}
	}
	spec := p.plan.Dump.Spec
	stored := out.snaps
	run(runCtx, p.items, &spec, nil)
	out.snaps = append(stored, out.snaps...)
	if end() && out.err != nil && !out.handedOver {
		out.err, out.deferred = nil, &deferral{reason: "a deploy cancelled the dump", deploy: true}
		return out
	}
	if out.err == nil {
		m.rememberDump(app, rs.job.Target, p.plan.Dump, out.snaps)
	}
	return out
}

// invoke runs one backup request on the job's target, claiming the next
// owner sequence number for it.
func (m *Manager) invoke(runCtx context.Context, rs runSpec, items []proto.Item, dump *docker.DumpSpec, source func(context.Context) (io.ReadCloser, error), labels map[string]string) ([]proto.Snapshot, error) {
	r, err := m.currentRunner()
	if err != nil {
		return nil, err
	}
	a, err := m.access(rs.job.Target, settings.AccessOptions{Write: true})
	if err != nil {
		return nil, err
	}
	req, err := m.request(proto.OpBackup, a, false)
	if err != nil {
		return nil, err
	}
	req.Backup = &proto.Backup{Run: rs.run, Trigger: rs.trigger, Items: items}
	var (
		mu    sync.Mutex
		snaps []proto.Snapshot
	)
	_, err = r.Run(runCtx, runner.Invocation{
		Request: req, Dump: dump, Source: source, Target: a.Target, Labels: labels,
		Handler: proto.Handler{Snapshot: func(s proto.Snapshot) {
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
		}},
	})
	if err != nil && !errors.Is(err, runner.ErrReleased) {
		m.observe(rs.job.Target, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return snaps, err
}
