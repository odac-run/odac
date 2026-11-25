package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
)

// dockerControl holds real containers the way the app manager holds apps:
// stopped while held, started for a load.
type dockerControl struct {
	*fakeControl
	containers map[string]string
}

func (c *dockerControl) HoldApp(name string) (AppHold, error) {
	h, err := c.fakeControl.HoldApp(name)
	if err != nil {
		return nil, err
	}
	d := &dockerHold{fakeHold: h.(*fakeHold), container: c.containers[name]}
	d.Stop()
	return d, nil
}

type dockerHold struct {
	*fakeHold
	container string
}

func (h *dockerHold) Start() error {
	h.fakeHold.Start()
	if out, err := exec.Command("docker", "start", h.container).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func (h *dockerHold) Stop() {
	h.fakeHold.Stop()
	exec.Command("docker", "stop", "-t", "20", h.container).Run()
}

func (h *dockerHold) Release(active bool) error {
	h.fakeHold.Release(active)
	if active {
		return h.Start()
	}
	h.Stop()
	return nil
}

// TestDockerRestorePostgres restores a real Postgres app in place: its
// data volume emptied for the image to initialize, the dump loaded with
// psql, its other volume swapped back, a safety backup first. Off unless
// ODAC_DOCKER_TESTS=1; it needs postgres:16-alpine locally.
func TestDockerRestorePostgres(t *testing.T) {
	if os.Getenv("ODAC_DOCKER_TESTS") != "1" {
		t.Skip("set ODAC_DOCKER_TESTS=1 to run against a Docker daemon")
	}
	if err := exec.Command("docker", "image", "inspect", "postgres:16-alpine").Run(); err != nil {
		t.Fatal("postgres:16-alpine is not local; pull it first")
	}
	c, tag := dockerHelperImage(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	pg, up := filepath.Join(base, "apps", "db", "pg"), filepath.Join(base, "apps", "db", "up")
	os.MkdirAll(pg, 0o755)
	os.MkdirAll(up, 0o755)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("v1"), 0o644)
	container := fmt.Sprintf("odac-restore-test-db-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", container, "-e", "POSTGRES_PASSWORD=pw-for-the-test",
		"-e", "PGDATA=/var/lib/postgresql/data/pgdata", "-v", pg+":/var/lib/postgresql/data", "-v", up+":/uploads", "postgres:16-alpine").CombinedOutput(); err != nil {
		t.Fatalf("start postgres: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "-v", container).Run()
		// The database's files belong to its user; a container removes them.
		exec.Command("docker", "run", "--rm", "-v", base+":/w", "alpine:3.22", "rm", "-rf", "/w/apps", "/w/t1", "/w/backup").Run()
	})
	psql := func(sql string) (string, error) {
		out, err := exec.Command("docker", "exec", container, "psql", "-h", "127.0.0.1", "-U", "postgres", "-tA", "-c", sql).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := psql("SELECT 1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("postgres did not start")
		}
		time.Sleep(time.Second)
	}
	if out, err := psql("CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)"); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}

	h := newHarness(t)
	h.base = base
	h.apps.list = []settings.App{{Name: "db", Stateful: true}}
	h.apps.apps = map[string]consistency.App{"db": {Name: "db", Container: container, Image: "postgres:16-alpine",
		Volumes: []consistency.Volume{{Host: pg, Container: "/var/lib/postgresql/data"}, {Host: up, Container: "/uploads"}}}}
	h.set.roots = map[string]string{"t1": filepath.Join(base, "t1")}
	os.MkdirAll(h.set.roots["t1"], 0o755)
	helper, dump := runner.DefaultLimits(2<<30, 2)
	rn, err := runner.New(runner.Config{Docker: runner.FromClient(c), Binary: "/app/bin/odac-backup", Image: tag, Limits: helper, DumpLimits: dump, DumpRate: runner.DefaultDumpRate, DumpIdle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	planner, err := consistency.NewPlanner(consistency.Config{
		Prober:          consistency.NewProber(runner.FromClient(c), dump),
		AvailableMemory: func() (int64, error) { return 4 << 30, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeControl{h: h, active: map[string]bool{"db": true}, busy: map[string]bool{}, defs: map[string]map[string]any{}}
	ctl := &dockerControl{fakeControl: fc, containers: map[string]string{"db": container}}
	var mu sync.Mutex
	var done []RestoreResult
	m, err := New(Config{
		Apps: h.apps, States: c, Planner: planner, Loader: planner, Control: ctl,
		NewRunner: func() (Runner, error) { return FromRunner(rn), nil },
		BaseDir:   base, ServerDirs: []string{},
		// The safety copy before the restore is a snapshot.
		ServerID:  testServerID,
		Disk:      roomyDisk{},
		OnRecord:  func(r RunRecord) { h.recs <- r },
		OnRestore: func(r RestoreResult) { mu.Lock(); done = append(done, r); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSettings(h.set)
	h.m = m
	ctx := context.Background()
	a, _ := h.set.Access("t1", settings.AccessOptions{Write: true})
	if _, err := m.Init(ctx, a); err != nil {
		t.Fatalf("init: %v", err)
	}
	h.clock = &clock{t: time.Now()}
	j := h.job("j", "t1", 24*60, "db")
	rec := h.runNow(j)
	if rec.Status != StatusOK || rec.Apps[0].Method != MethodDump {
		t.Fatalf("backup: %+v", rec)
	}
	h.set.mu.Lock()
	h.set.jobs = nil
	h.set.mu.Unlock()

	if out, err := psql("INSERT INTO t SELECT generate_series(1001, 3000)"); err != nil {
		t.Fatalf("change: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("v2"), 0o644)
	m.Start()
	defer m.Stop()
	waitRecovered(t, m)
	if _, err := m.StartRestore(ctx, RestoreRequest{Target: "t1", Run: rec.Run, Mode: RestoreInPlace}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		mu.Lock()
		n := len(done)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore did not finish")
		}
		time.Sleep(time.Second)
	}
	res := done[0]
	if res.Status != StatusOK || res.Safety == "" {
		t.Fatalf("restore: %+v", res)
	}
	if got, err := psql("SELECT count(*) FROM t"); err != nil || got != "1000" {
		t.Fatalf("restored count %q, %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(up, "a.txt")); string(b) != "v1" {
		t.Fatalf("uploads %q", b)
	}
	names, _ := entries(pg)
	for _, n := range names {
		if strings.HasPrefix(n, ".odac-restore") {
			t.Errorf("left %s in the data volume", n)
		}
	}
	list, err := c.BackupContainers()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		t.Errorf("left behind: %s (%s)", b.Name, b.Role)
	}
}
