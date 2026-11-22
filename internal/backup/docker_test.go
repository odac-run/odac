package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/docker"
)

// TestDockerManager runs the manager against a real daemon: helpers from a
// throwaway image holding this tree's odac-backup, a dump container in a
// stand-in database app, two disk targets. It covers what the fakes
// cannot: the requests as odac-backup reads them, a stored dump read back
// into a second repository, and the detailed usage behind the space
// decisions. Off unless ODAC_DOCKER_TESTS=1.
func TestDockerManager(t *testing.T) {
	if os.Getenv("ODAC_DOCKER_TESTS") != "1" {
		t.Skip("set ODAC_DOCKER_TESTS=1 to run against a Docker daemon")
	}
	c, tag := dockerHelperImage(t)
	app := fmt.Sprintf("odac-backup-test-db-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", app, "alpine:3.22", "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("start app: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", app).Run() })

	// macOS's TMPDIR is a symlink Docker Desktop resolves.
	base, _ := filepath.EvalSymlinks(t.TempDir())
	h := newHarness(t)
	h.base = base
	data := filepath.Join(base, "apps", "db", "data")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "table"), []byte(strings.Repeat("row\n", 10000)), 0o644)
	h.apps.list = []settings.App{{Name: "db", Stateful: true}}
	h.apps.apps = map[string]consistency.App{"db": {Name: "db", Container: app, Image: "alpine:3.22", Volumes: []consistency.Volume{{Host: data, Container: "/data"}}}}
	h.set.roots = map[string]string{"t1": filepath.Join(base, "t1"), "t2": filepath.Join(base, "t2")}
	for _, r := range h.set.roots {
		os.MkdirAll(r, 0o755)
	}
	helper, dump := runner.DefaultLimits(2<<30, 2)
	rn, err := runner.New(runner.Config{Docker: runner.FromClient(c), Binary: "/app/bin/odac-backup", Image: tag, Limits: helper, DumpLimits: dump, DumpRate: runner.DefaultDumpRate, DumpIdle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{
		Apps: h.apps, States: c, Planner: seqPlanner{},
		NewRunner: func() (Runner, error) { return FromRunner(rn), nil },
		BaseDir:   base, ServerDirs: []string{},
		OnRecord: func(r RunRecord) { h.recs <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSettings(h.set)
	h.m = m
	ctx := context.Background()
	for _, id := range []string{"t1", "t2"} {
		a, _ := h.set.Access(id, settings.AccessOptions{Write: true})
		if _, err := m.Init(ctx, a); err != nil {
			t.Fatalf("init %s: %v", id, err)
		}
	}

	a := h.job("a", "t1", 24*60, "db")
	b := h.job("b", "t2", 24*60, "db")
	h.clock = &clock{t: time.Now()}
	m.cfg.Now = time.Now
	if rec := h.runNow(a); rec.Status != StatusOK || rec.Snapshots != 2 {
		t.Fatalf("job a: %+v", rec)
	}
	rec := h.runNow(b)
	if rec.Status != StatusOK || rec.Snapshots != 2 || rec.Apps[0].Method != MethodReused {
		t.Fatalf("job b: %+v", rec)
	}
	snaps, err := m.list(ctx, "t2")
	if err != nil || len(snaps) != 2 {
		t.Fatalf("list t2: %+v %v", snaps, err)
	}
	for _, s := range snaps {
		if s.Kind == "stream" && (s.Bytes != int64(len(seqOutput(1000))) || s.Consistency != consistency.Consistent) {
			t.Fatalf("reused dump %+v", s)
		}
	}

	res, err := m.usage(ctx, "t1", &proto.Usage{Order: [][]string{{snaps[0].ID}}})
	if err != nil || res.Space == nil || !res.Space.Detailed || res.Space.Free <= 0 || len(res.Space.Freed) != 1 || res.Space.Jobs["a"] == 0 {
		t.Fatalf("detailed usage: %+v %v", res, err)
	}
	m.maintain(ctx, "t1")
	if rec := h.record(); rec.Kind != KindMaintain || rec.Status != StatusOK {
		t.Fatalf("maintenance: %+v", rec)
	}
	list, err := c.BackupContainers()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		t.Errorf("left behind: %s (%s)", b.Name, b.Role)
	}
}

// dockerHelperImage connects to the daemon and builds a throwaway image
// holding this tree's odac-backup, which helpers run from.
func dockerHelperImage(t *testing.T) (*docker.Client, string) {
	t.Helper()
	c, err := docker.Connect(docker.Options{})
	if err != nil || !c.Available() {
		t.Fatalf("Docker: %v", err)
	}
	arch := runtime.GOARCH
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output(); err == nil {
		arch = strings.TrimSpace(string(out))
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "odac-backup"), "odac/cmd/odac-backup")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build odac-backup: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.22\nCOPY odac-backup /app/bin/odac-backup\n"), 0o644)
	tag := fmt.Sprintf("odac-backup-test:%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", tag).Run() })
	return c, tag
}

// seqPlanner dumps the stand-in database with seq.
type seqPlanner struct{}

func (seqPlanner) Plan(_ context.Context, app consistency.App, _ string) (*consistency.Plan, error) {
	plan := consistency.FilesPlan(app, consistency.Live)
	plan.Engine = consistency.Postgres
	plan.Dump = &consistency.Dump{Name: "postgres", File: "dump.sql", Consistency: consistency.Consistent,
		Spec: docker.DumpSpec{Image: "alpine:3.22", App: app.Container, Cmd: []string{"seq", "1", "1000"}}}
	return plan, nil
}

func seqOutput(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

// roomyDisk measures the repository for real and reports a disk with
// room, since the machine running the test may be fuller than the floor.
type roomyDisk struct{}

func (roomyDisk) Space(string) (int64, int64, bool) { return 500 << 30, 1000 << 30, true }
func (roomyDisk) Size(dir string) int64             { return hostDisk{}.Size(dir) }

// TestDockerSnapshot takes a snapshot against a real daemon: the snapshot
// repository is made in the data directory with its own key, the dump is
// stored there, a backup job right after reads that dump back instead of
// dumping again, and the repository is maintained. Off unless
// ODAC_DOCKER_TESTS=1.
func TestDockerSnapshot(t *testing.T) {
	if os.Getenv("ODAC_DOCKER_TESTS") != "1" {
		t.Skip("set ODAC_DOCKER_TESTS=1 to run against a Docker daemon")
	}
	c, tag := dockerHelperImage(t)
	app := fmt.Sprintf("odac-backup-test-db-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", app, "alpine:3.22", "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("start app: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", app).Run() })

	base, _ := filepath.EvalSymlinks(t.TempDir())
	h := newHarness(t)
	h.base = base
	data := filepath.Join(base, "apps", "db", "data")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "table"), []byte(strings.Repeat("row\n", 10000)), 0o644)
	h.apps.list = []settings.App{{Name: "db", Stateful: true}}
	h.apps.apps = map[string]consistency.App{"db": {Name: "db", Container: app, Image: "alpine:3.22", Volumes: []consistency.Volume{{Host: data, Container: "/data"}}}}
	h.set.roots = map[string]string{"t1": filepath.Join(base, "t1")}
	os.MkdirAll(h.set.roots["t1"], 0o755)
	helper, dump := runner.DefaultLimits(2<<30, 2)
	rn, err := runner.New(runner.Config{Docker: runner.FromClient(c), Binary: "/app/bin/odac-backup", Image: tag, Limits: helper, DumpLimits: dump, DumpRate: runner.DefaultDumpRate, DumpIdle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	snaps := make(chan SnapshotRecord, 8)
	m, err := New(Config{
		Apps: h.apps, States: c, Planner: seqPlanner{},
		NewRunner: func() (Runner, error) { return FromRunner(rn), nil },
		BaseDir:   base, ServerDirs: []string{}, ServerID: testServerID, Disk: roomyDisk{},
		OnRecord:   func(r RunRecord) { h.recs <- r },
		OnSnapshot: func(r SnapshotRecord) { snaps <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSettings(h.set)
	h.m = m
	ctx := context.Background()

	rec := m.takeSnapshot(ctx, snapRequest{series: "db", event: EventManual, apps: []string{"db"}, reuse: true})
	if rec.Status != StatusOK || rec.Snapshots != 2 || rec.Added <= 0 {
		t.Fatalf("snapshot: %+v", rec)
	}
	<-snaps
	if fi, err := os.Stat(snapshotKey(base)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", fi, err)
	}
	if pts := m.SnapshotPoints("db"); len(pts) != 1 || pts[0].Run != rec.Run || !pts[0].Apps[0].Dump {
		t.Fatalf("points %+v", pts)
	}

	a, _ := h.set.Access("t1", settings.AccessOptions{Write: true})
	if _, err := m.Init(ctx, a); err != nil {
		t.Fatalf("init t1: %v", err)
	}
	m.cfg.Now = time.Now
	h.clock = &clock{t: time.Now()}
	job := h.runNow(h.job("a", "t1", 24*60, "db"))
	if job.Status != StatusOK || job.Apps[0].Method != MethodReused {
		t.Fatalf("job after the snapshot: %+v", job)
	}

	m.maintain(ctx, SnapshotTarget)
	if mr := <-snaps; mr.Kind != KindMaintain || mr.Status != StatusOK {
		t.Fatalf("maintenance: %+v", mr)
	}
	list, err := c.BackupContainers()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		t.Errorf("left behind: %s (%s)", b.Name, b.Role)
	}
}
