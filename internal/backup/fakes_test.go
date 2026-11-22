package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
	"odac/internal/docker"
	"odac/internal/logx"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeSettings struct {
	mu       sync.Mutex
	jobs     []settings.JobView
	retired  []settings.RetiredView
	seq      int64
	writes   int
	observed []error
	purged   []string
	fs       map[string]bool // targets with a filesystem
	// roots puts a target on a host directory, the way a disk target is:
	// the runner mounts it and the repository lives inside it.
	roots map[string]string
}

func (s *fakeSettings) Jobs() ([]settings.JobView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.jobs), nil
}

func (s *fakeSettings) Retired() ([]settings.RetiredView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.retired), nil
}

func (s *fakeSettings) Access(id string, opts settings.AccessOptions) (settings.Access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := settings.Access{Repo: proto.Repo{Password: "correct horse battery staple", ServerID: "0123456789abcdef0123456789abcdef"}}
	switch {
	case s.roots[id] != "":
		a.Target = &runner.Target{Root: s.roots[id]}
		a.Repo.Storage.Filesystem = &proto.FilesystemStorage{Path: "/" + a.Repo.ServerID + "/" + proto.RepositoryDir}
	case s.fs[id]:
		a.Repo.Storage.Filesystem = &proto.FilesystemStorage{Path: "/" + id}
	default:
		a.Repo.Storage.S3 = &proto.S3Storage{Bucket: id}
	}
	if opts.Write {
		s.seq++
		s.writes++
		a.Repo.Owner = &proto.Owner{Seq: s.seq}
	}
	return a, nil
}

func (s *fakeSettings) Observe(id string, err error) {
	s.mu.Lock()
	s.observed = append(s.observed, err)
	s.mu.Unlock()
}

func (s *fakeSettings) Purged(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purged = append(s.purged, id)
	s.retired = slices.DeleteFunc(s.retired, func(r settings.RetiredView) bool { return r.ID == id && r.Purge })
	return nil
}

type fakeApps struct {
	mu   sync.Mutex
	list []settings.App
	apps map[string]consistency.App
}

func (a *fakeApps) BackupApps() []settings.App {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.list)
}

func (a *fakeApps) BackupApp(name string) (consistency.App, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	app, ok := a.apps[name]
	return app, ok
}

type fakeStates struct {
	mu  sync.Mutex
	err error
}

func (s *fakeStates) BackupAppState(context.Context, string) (docker.AppState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return docker.AppState{}, s.err
	}
	return docker.AppState{Running: true, ImageID: "sha256:img"}, nil
}

// fakePlanner gives an app with "db" in its name a dump, every app its
// volumes as directories.
type fakePlanner struct{}

func (fakePlanner) Plan(_ context.Context, app consistency.App, run string) (*consistency.Plan, error) {
	plan := consistency.FilesPlan(app, consistency.Live)
	if strings.Contains(app.Name, "db") {
		plan.Engine = consistency.Postgres
		plan.Dump = &consistency.Dump{Name: "postgres", File: "dump.sql", Consistency: consistency.Consistent, Spec: docker.DumpSpec{Image: app.Image, App: app.Container, Cmd: []string{"pg_dumpall"}}}
		covered := consistency.DataVolumes(app, consistency.Postgres)
		plan.Covered = covered
		plan.Dirs = slices.DeleteFunc(plan.Dirs, func(d consistency.Dir) bool {
			return slices.ContainsFunc(covered, func(v consistency.Volume) bool { return v.Host == d.Path })
		})
	}
	return plan, nil
}

// repo is a target's repository in memory. Every snapshot holds bytes of
// its own, so what deleting a run frees is its size.
type repo struct {
	snaps []proto.Snapshot
	size  map[string]int64
	// files and data are what each snapshot stored: a directory's files
	// by relative path, a stream's bytes.
	files   map[string]map[string][]byte
	data    map[string][]byte
	garbage int64
	free    int64
	total   int64
	maint   int
}

type call struct {
	op     string
	target string
	labels map[string]string
	req    *proto.Request
	source bool
	dump   bool
}

// fakeRunner plays odac-backup against in-memory repositories.
type fakeRunner struct {
	clock *clock
	mu    sync.Mutex
	repos map[string]*repo
	calls []call
	n     int
	// dump produces a dump's bytes; nil gives a fixed text.
	dump     func(ctx context.Context, spec docker.DumpSpec) ([]byte, error)
	itemSize int64
	released chan struct{}
	relOnce  sync.Once
	orphans  []Orphan
	adoptErr error
	// loads are the loads run, with the bytes each read; loadErr fails
	// them.
	loads   []fakeLoad
	loadErr error
	// failOp fails every request of that operation.
	failOp string
}

type fakeLoad struct {
	spec docker.DumpSpec
	data []byte
}

func (r *fakeRunner) Load(ctx context.Context, spec docker.DumpSpec, input io.Reader, labels map[string]string) (string, error) {
	data, err := io.ReadAll(input)
	r.mu.Lock()
	r.loads = append(r.loads, fakeLoad{spec: spec, data: data})
	lerr := r.loadErr
	r.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "", lerr
}

func newFakeRunner(c *clock) *fakeRunner {
	return &fakeRunner{clock: c, repos: map[string]*repo{}, itemSize: 100, released: make(chan struct{})}
}

func (r *fakeRunner) target(req *proto.Request) string {
	st := req.Repo.Storage
	if st.Filesystem != nil {
		return strings.TrimPrefix(st.Filesystem.Path, "/")
	}
	return st.S3.Bucket
}

func (r *fakeRunner) repo(t string) *repo {
	if r.repos[t] == nil {
		r.repos[t] = &repo{size: map[string]int64{}, files: map[string]map[string][]byte{}, data: map[string][]byte{}, free: -1, total: -1}
	}
	return r.repos[t]
}

func (r *fakeRunner) Release() { r.relOnce.Do(func() { close(r.released) }) }

func (r *fakeRunner) Adopt() ([]Orphan, error) { return r.orphans, r.adoptErr }

func (r *fakeRunner) Run(ctx context.Context, inv runner.Invocation) (*proto.Result, error) {
	req := inv.Request
	check := *req
	check.Version = proto.Version
	if err := check.Validate(); err != nil {
		return nil, err
	}
	t := r.target(req)
	r.mu.Lock()
	r.calls = append(r.calls, call{op: req.Op, target: t, labels: inv.Labels, req: req, source: inv.Source != nil, dump: inv.Dump != nil})
	failOp := r.failOp
	r.mu.Unlock()
	if failOp == req.Op {
		return nil, &proto.Error{Code: proto.CodeFailed, Message: "the fake fails " + failOp}
	}
	select {
	case <-r.released:
		return nil, runner.ErrReleased
	default:
	}
	switch req.Op {
	case proto.OpBackup:
		return r.backup(ctx, t, inv)
	case proto.OpList:
		r.mu.Lock()
		snaps := slices.Clone(r.repo(t).snaps)
		r.mu.Unlock()
		f := req.List
		n := 0
		for _, s := range snaps {
			if f.Run != "" && s.Run != f.Run || f.App != "" && s.Source.App != f.App || f.Job != "" && s.Source.Job != f.Job {
				continue
			}
			inv.Handler.Snapshot(s)
			n++
		}
		return &proto.Result{Count: n}, nil
	case proto.OpServers:
		return &proto.Result{Servers: []proto.ServerDir{{ID: req.Repo.ServerID, Name: "srv", Seq: 1, Repository: true}}}, nil
	case proto.OpDelete:
		r.mu.Lock()
		defer r.mu.Unlock()
		rp := r.repo(t)
		rp.snaps = slices.DeleteFunc(rp.snaps, func(s proto.Snapshot) bool {
			if slices.Contains(req.Delete.IDs, s.ID) {
				rp.garbage += rp.size[s.ID]
				return true
			}
			return false
		})
		return &proto.Result{}, nil
	case proto.OpMaintain:
		r.mu.Lock()
		defer r.mu.Unlock()
		rp := r.repo(t)
		rp.maint++
		return &proto.Result{Bytes: r.usedLocked(rp), Space: &proto.Space{Free: rp.free, Total: rp.total}}, nil
	case proto.OpUsage:
		r.mu.Lock()
		defer r.mu.Unlock()
		rp := r.repo(t)
		sp := &proto.Space{Free: rp.free, Total: rp.total}
		if u := req.Usage; u != nil {
			sp.Detailed, sp.Garbage, sp.Jobs = true, rp.garbage, map[string]int64{}
			for _, s := range rp.snaps {
				sp.Jobs[s.Source.Job] += rp.size[s.ID]
			}
			var repoSum int64
			jobSum := map[string]int64{}
			for _, run := range u.Order {
				job := ""
				for _, id := range run {
					for _, s := range rp.snaps {
						if s.ID == id {
							job = s.Source.Job
							repoSum += rp.size[id]
							jobSum[job] += rp.size[id]
						}
					}
				}
				sp.Freed = append(sp.Freed, proto.Freed{Repo: repoSum, Job: jobSum[job]})
			}
		}
		return &proto.Result{Bytes: r.usedLocked(rp), Space: sp}, nil
	case proto.OpRestore:
		r.mu.Lock()
		var found *proto.Snapshot
		var files map[string][]byte
		var stored []byte
		for _, rp := range r.repos {
			for i := range rp.snaps {
				if rp.snaps[i].ID == req.Restore.ID {
					found = &rp.snaps[i]
					files, stored = rp.files[found.ID], rp.data[found.ID]
				}
			}
		}
		r.mu.Unlock()
		if found == nil {
			return nil, &proto.Error{Code: proto.CodeNotFound, Message: "no such snapshot"}
		}
		if d := req.Restore.Dir; d != "" {
			if found.Kind != "dir" {
				return nil, &proto.Error{Code: proto.CodeKind, Message: "not a directory snapshot"}
			}
			if names, _ := os.ReadDir(d); len(names) > 0 {
				return nil, &proto.Error{Code: proto.CodeTargetNotEmpty, Message: d}
			}
			for rel, b := range files {
				p := filepath.Join(d, rel)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, b, 0o644); err != nil {
					return nil, err
				}
			}
			return &proto.Result{Files: int64(len(files))}, nil
		}
		data := []byte("stored dump " + found.ID)
		if stored != nil {
			data = stored
		}
		if _, err := inv.Handler.Output.Write(data); err != nil {
			return nil, err
		}
		return &proto.Result{Bytes: int64(len(data))}, nil
	case proto.OpInit:
		return &proto.Result{Created: true}, nil
	}
	return nil, fmt.Errorf("fake runner: %s", req.Op)
}

func (r *fakeRunner) usedLocked(rp *repo) int64 {
	var n int64
	for _, s := range rp.snaps {
		n += rp.size[s.ID]
	}
	return n + rp.garbage
}

func (r *fakeRunner) backup(ctx context.Context, t string, inv runner.Invocation) (*proto.Result, error) {
	b := inv.Request.Backup
	for i, it := range b.Items {
		size := r.itemSize
		kind := "dir"
		var streamData []byte
		if it.Stream != "" {
			kind = "stream"
			var data []byte
			var err error
			switch {
			case inv.Source != nil:
				var rc io.ReadCloser
				if rc, err = inv.Source(ctx); err == nil {
					data, err = io.ReadAll(rc)
					rc.Close()
				}
			case r.dump != nil:
				data, err = r.dump(ctx, *inv.Dump)
			default:
				data = []byte("dump of " + it.Source.App)
			}
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				select {
				case <-r.released:
					return nil, runner.ErrReleased
				default:
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, &proto.Error{Code: proto.CodeStream, Message: fmt.Sprintf("item %d: %v", i, err)}
			}
			size = int64(len(data))
			streamData = data
		}
		var files map[string][]byte
		if it.Dir != "" {
			files = readTree(it.Dir)
		}
		now := r.clock.Now()
		r.mu.Lock()
		r.n++
		s := proto.Snapshot{ID: fmt.Sprintf("s%d", r.n), Host: "srv", Source: it.Source, Kind: kind, Run: b.Run, Trigger: b.Trigger, Consistency: it.Consistency, Start: now, End: now, Bytes: size}
		rp := r.repo(t)
		rp.snaps = append(rp.snaps, s)
		rp.size[s.ID] = size
		rp.files[s.ID] = files
		if streamData != nil {
			rp.data[s.ID] = streamData
		}
		if rp.free >= 0 {
			rp.free -= size
		}
		r.mu.Unlock()
		inv.Handler.Snapshot(s)
	}
	return &proto.Result{Count: len(b.Items)}, nil
}

func (r *fakeRunner) ops(kind string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []call
	for _, c := range r.calls {
		if c.op == kind {
			out = append(out, c)
		}
	}
	return out
}

// seed stores a run of job made at time at.
func (r *fakeRunner) seed(target, job, run string, at time.Time, size int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	s := proto.Snapshot{ID: fmt.Sprintf("s%d", r.n), Source: proto.Source{Job: job, App: "web", Name: "files"}, Kind: "dir", Run: run, Start: at, End: at, Bytes: size}
	rp := r.repo(target)
	rp.snaps = append(rp.snaps, s)
	rp.size[s.ID] = size
}

func (r *fakeRunner) runs(target string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.repo(target).snaps {
		if !slices.Contains(out, s.Run) {
			out = append(out, s.Run)
		}
	}
	slices.Sort(out)
	return out
}

type harness struct {
	t      *testing.T
	m      *Manager
	set    *fakeSettings
	apps   *fakeApps
	states *fakeStates
	run    *fakeRunner
	clock  *clock
	base   string
	recs   chan RunRecord
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		set:    &fakeSettings{fs: map[string]bool{}},
		apps:   &fakeApps{apps: map[string]consistency.App{}},
		states: &fakeStates{},
		clock:  &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)},
		base:   t.TempDir(),
		recs:   make(chan RunRecord, 64),
	}
	h.run = newFakeRunner(h.clock)
	h.m = h.manager()
	return h
}

func (h *harness) manager() *Manager {
	h.t.Helper()
	server := filepath.Join(h.base, "config")
	os.MkdirAll(server, 0o700)
	m, err := New(Config{
		Apps: h.apps, States: h.states, Planner: fakePlanner{},
		NewRunner: func() (Runner, error) { return h.run, nil },
		BaseDir:   h.base, ServerDirs: []string{server},
		OnRecord: func(r RunRecord) { h.recs <- r },
		Log:      logx.New("Backup", "Test"),
		Now:      h.clock.Now,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	m.SetSettings(h.set)
	m.t.finishLimit = time.Second
	return m
}

// app adds an app with one data directory.
func (h *harness) app(name string, linked ...string) {
	dir := filepath.Join(h.base, "apps", name, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.apps.mu.Lock()
	h.apps.list = append(h.apps.list, settings.App{Name: name, Stateful: true, Linked: linked})
	h.apps.apps[name] = consistency.App{Name: name, Container: name, Image: name + ":1", Volumes: []consistency.Volume{{Host: dir, Container: "/data"}}}
	h.apps.mu.Unlock()
}

func (h *harness) job(id, target string, every int, apps ...string) settings.JobView {
	j := settings.JobView{ID: id, Job: settings.Job{Name: "job-" + id, Target: target, Apps: apps, Every: every, Density: thin.Normal}}
	h.set.mu.Lock()
	h.set.jobs = append(h.set.jobs, j)
	h.set.mu.Unlock()
	return j
}

func (h *harness) record() RunRecord {
	h.t.Helper()
	select {
	case r := <-h.recs:
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("no run was recorded")
	}
	return RunRecord{}
}

func (h *harness) runNow(j settings.JobView) RunRecord {
	h.t.Helper()
	h.m.runJob(context.Background(), runSpec{job: j, run: newRunID(h.clock.Now()), trigger: triggerSchedule, scheduled: h.clock.Now()})
	return h.record()
}

var errDocker = errors.New("Cannot connect to the Docker daemon")

// readTree reads every regular file under dir.
func readTree(dir string) map[string][]byte {
	out := map[string][]byte{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	return out
}
