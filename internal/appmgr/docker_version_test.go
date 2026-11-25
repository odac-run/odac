package appmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/applog"
	"odac/internal/backup"
	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/config"
	"odac/internal/docker"
)

// The tests below run against a real Docker daemon and cover Phase G:
//   - Postgres and MySQL patch tag switches with real version snapshots
//   - Health gate forced failure (immediate exit + failing HEALTHCHECK) and rollback
//   - Mid-switch agent crash recovery (imageChange marker + hold tag + Check)
//   - Blue-Green rollback of a git app (both failing and succeeding paths)
//   - Bidirectional restore vs switch contention and in-place image_mismatch refusal
//
// They are off unless ODAC_DOCKER_TESTS=1. Pull required base images first:
//
//	docker pull postgres:16.15-alpine postgres:16-alpine mysql:8.4.11 mysql:8.4 alpine:3.22 busybox:1.37
//	ODAC_DOCKER_TESTS=1 go test -v -timeout 15m ./internal/appmgr/ -run TestDocker
const dockerVersionTestsEnv = "ODAC_DOCKER_TESTS"

const dockerTestServerID = "fedcba9876543210fedcba9876543210"

// dockerProxySpy counts proxy sync and purge calls triggered by Manager.
type dockerProxySpy struct {
	mu     sync.Mutex
	syncs  int
	purges int
}

func (p *dockerProxySpy) SyncConfig() {
	p.mu.Lock()
	p.syncs++
	p.mu.Unlock()
}

func (p *dockerProxySpy) PurgeCacheForApp(any) {
	p.mu.Lock()
	p.purges++
	p.mu.Unlock()
}

func (p *dockerProxySpy) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.syncs, p.purges
}

// dockerSnapshotHooks adapts *backup.Manager to appmgr.Snapshotter, matching
// cmd/odac-server/backup.go.
type dockerSnapshotHooks struct{ m *backup.Manager }

func (s dockerSnapshotHooks) BeforeDeploy(app, event string) (DeploySnapshot, string) {
	p, note := s.m.BeforeDeploy(app, event)
	if p == nil {
		return nil, note
	}
	return p, ""
}

func (s dockerSnapshotHooks) BeforeDelete(app string) string { return s.m.BeforeDelete(app) }

// dockerAppControl adapts *Manager to backup.AppControl, matching
// cmd/odac-server/backup.go.
type dockerAppControl struct{ m *Manager }

func (a dockerAppControl) HoldApp(name string) (backup.AppHold, error) {
	h, err := a.m.HoldApp(name)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (a dockerAppControl) SetAppActive(name string, active bool) bool {
	return a.m.SetAppActive(name, active)
}

func (a dockerAppControl) AppDefinition(name string) (map[string]any, bool) {
	return a.m.AppDefinition(name)
}

func (a dockerAppControl) UniqueAppName(base string) string { return a.m.UniqueAppName(base) }
func (a dockerAppControl) AppsDir() string                  { return a.m.AppsDir() }

func (a dockerAppControl) CreateRestoredApp(def map[string]any, name string, renames map[string]string, group string) ([]string, error) {
	r, err := a.m.CreateRestoredApp(def, name, renames, group)
	if err != nil {
		return nil, err
	}
	return r.Dropped, nil
}

func (a dockerAppControl) DeleteRestoredApp(name string) error {
	return a.m.DeleteRestoredApp(name)
}

// dockerRoomyDisk measures snapshot repo size on disk while reporting ample
// free space so a nearly full developer disk does not skip snapshots.
type dockerRoomyDisk struct{}

func (dockerRoomyDisk) Space(string) (int64, int64, bool) { return 500 << 30, 1000 << 30, true }

func (dockerRoomyDisk) Size(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

type dockerTestEnv struct {
	t      *testing.T
	dcDir  string
	base   string
	client *docker.Client
	cfg    *config.Store
	m      *Manager
	proxy  *dockerProxySpy
	bmgr   *backup.Manager
	bset   *settings.Settings

	mu         sync.Mutex
	containers []string
	images     []string
}

func (e *dockerTestEnv) trackContainer(name string) {
	e.mu.Lock()
	e.containers = append(e.containers, name)
	e.mu.Unlock()
}

func (e *dockerTestEnv) trackImage(ref string) {
	e.mu.Lock()
	e.images = append(e.images, ref)
	e.mu.Unlock()
}

func dockerEnvVars(dcDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "DOCKER_CONFIG="+dcDir)
}

func runDocker(dcDir string, args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = dockerEnvVars(dcDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustDocker(t *testing.T, dcDir string, args ...string) string {
	t.Helper()
	out, err := runDocker(dcDir, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func newDockerTestEnv(t *testing.T, apps []any, domains map[string]any, withBackup bool, requiredImages ...string) *dockerTestEnv {
	t.Helper()
	if os.Getenv(dockerVersionTestsEnv) != "1" {
		t.Skipf("set %s=1 to run against a real Docker daemon", dockerVersionTestsEnv)
	}

	// Temporary credential-free DOCKER_CONFIG prevents Docker Desktop's
	// credential helper from blocking on a locked keychain.
	dcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dcDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dcDir)
	t.Setenv("ODAC_BACKEND_SOCKETS", "off")

	for _, img := range requiredImages {
		if _, err := runDocker(dcDir, "image", "inspect", img); err != nil {
			t.Fatalf("required image %s is not local; pull it first", img)
		}
	}

	// Record which ODAC networks did not exist before this test so Cleanup
	// removes only the ones we caused Docker to create.
	var netsToClean []string
	for _, netName := range []string{docker.DefaultNetwork, docker.BackupNetwork} {
		if _, err := runDocker(dcDir, "network", "inspect", netName); err != nil {
			netsToClean = append(netsToClean, netName)
		}
	}

	c, err := docker.Connect(docker.Options{})
	if err != nil || !c.Available() {
		t.Fatalf("Docker not available: %v", err)
	}

	// macOS TMPDIR is a symlink (/var -> /private/var) that Docker Desktop
	// resolves; mount the canonical path.
	rawBase := t.TempDir()
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	appsDir := filepath.Join(base, "apps")
	logsDir := filepath.Join(base, "logs")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mutate(func() {
		cfg.Set("app", map[string]any{"path": appsDir})
		cfg.Set("apps", apps)
		if domains != nil {
			cfg.Set("domains", domains)
		}
	})
	if err := cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	proxy := &dockerProxySpy{}
	m := New(cfg, logsDir, Deps{Docker: c, Proxy: proxy})
	m.pollInterval = 25 * time.Millisecond
	m.scanPortInterval = 25 * time.Millisecond
	m.scanProbeInterval = 25 * time.Millisecond
	m.probeTimeout = 500 * time.Millisecond
	m.imageStableFor = 4 * time.Second
	m.imageGateInterval = 250 * time.Millisecond
	m.deploySwitchDelay = 200 * time.Millisecond
	m.readyInterval = 100 * time.Millisecond
	m.renameInterval = 200 * time.Millisecond
	m.Init()

	e := &dockerTestEnv{
		t:      t,
		dcDir:  dcDir,
		base:   base,
		client: c,
		cfg:    cfg,
		m:      m,
		proxy:  proxy,
	}

	// LIFO cleanup registration order:
	// 1) Registered first (runs last): remove test images/tags and networks created by test.
	t.Cleanup(func() {
		e.mu.Lock()
		imgs := append([]string(nil), e.images...)
		e.mu.Unlock()
		for _, img := range imgs {
			runDocker(dcDir, "rmi", "-f", img)
		}
		for _, netName := range netsToClean {
			runDocker(dcDir, "network", "rm", netName)
		}
	})

	// 2) Remove root-owned files created by database containers inside base
	// before t.TempDir's own cleanup runs.
	t.Cleanup(func() {
		if _, err := runDocker(dcDir, "image", "inspect", "alpine:3.22"); err == nil {
			runDocker(dcDir, "run", "--rm", "-v", base+":/w", "alpine:3.22", "rm", "-rf", "/w/apps", "/w/backup", "/w/logs")
		}
	})

	// 3) Stop and remove all tracked containers, green companions, and hold tags,
	// then wait for Manager background goroutines.
	t.Cleanup(func() {
		// Mark every app deleted so background pollers exit promptly.
		e.cfg.Mutate(func() {
			for _, app := range e.m.apps {
				app["_deleted"] = true
			}
		})
		e.mu.Lock()
		names := append([]string(nil), e.containers...)
		e.mu.Unlock()
		for _, name := range names {
			e.m.endLogStream(name)
			runDocker(dcDir, "rm", "-f", "-v", name)
			// Sweep any green companion left by a test failure.
			if out, err := runDocker(dcDir, "ps", "-a", "--filter", "name=^/"+name+"-green-", "--format", "{{.Names}}"); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
					if g := strings.TrimSpace(line); g != "" {
						runDocker(dcDir, "rm", "-f", "-v", g)
					}
				}
			}
		}
		// Sweep any hold tags left for our app ids.
		for _, id := range []float64{1, 2, 3} {
			runDocker(dcDir, "rmi", "-f", holdTag(id))
		}
		e.m.bg.Wait()
	})

	if withBackup {
		e.wireBackups()
	}
	return e
}

func (e *dockerTestEnv) wireBackups() {
	e.t.Helper()
	helperTag := e.buildBackupHelperImage()

	helperLimits, dumpLimits := runner.DefaultLimits(2<<30, 2)
	dockerSide := runner.FromClient(e.client)
	rn, err := runner.New(runner.Config{
		Docker:     dockerSide,
		Binary:     "/app/bin/odac-backup",
		Image:      helperTag,
		Limits:     helperLimits,
		DumpLimits: dumpLimits,
		DumpRate:   runner.DefaultDumpRate,
		DumpIdle:   time.Minute,
	})
	if err != nil {
		e.t.Fatalf("runner.New: %v", err)
	}
	planner, err := consistency.NewPlanner(consistency.Config{
		Prober:          consistency.NewProber(dockerSide, dumpLimits),
		AvailableMemory: func() (int64, error) { return 4 << 30, nil },
	})
	if err != nil {
		e.t.Fatalf("consistency.NewPlanner: %v", err)
	}
	bmgr, err := backup.New(backup.Config{
		Apps:       e.m,
		States:     e.client,
		Planner:    planner,
		Loader:     planner,
		Control:    dockerAppControl{e.m},
		NewRunner:  func() (backup.Runner, error) { return backup.FromRunner(rn), nil },
		BaseDir:    e.base,
		ServerDirs: []string{},
		ServerID:   dockerTestServerID,
		Disk:       dockerRoomyDisk{},
	})
	if err != nil {
		e.t.Fatalf("backup.New: %v", err)
	}
	bset, err := settings.New(settings.Config{
		Store:      e.cfg,
		ServerID:   dockerTestServerID,
		ServerName: func() string { return "odac-vtest" },
		Init:       bmgr,
		Apps:       e.m,
	})
	if err != nil {
		e.t.Fatalf("settings.New: %v", err)
	}
	enabled := true
	if _, err := bset.SetSnapshots(settings.SnapshotsPatch{Enabled: &enabled}); err != nil {
		e.t.Fatalf("SetSnapshots: %v", err)
	}
	bmgr.SetSettings(bset)
	e.m.SetDeployWatcher(bmgr)
	e.m.SetSnapshotter(dockerSnapshotHooks{bmgr})
	bmgr.Start()
	e.bmgr = bmgr
	e.bset = bset

	// Registered last so it runs first in LIFO cleanup.
	e.t.Cleanup(func() {
		bmgr.Stop()
		list, err := e.client.BackupContainers()
		if err != nil {
			e.t.Errorf("BackupContainers: %v", err)
			return
		}
		for _, b := range list {
			e.t.Errorf("leaked backup container: %s (%s)", b.Name, b.Role)
			runDocker(e.dcDir, "rm", "-f", b.Name)
		}
	})

	// Wait until backup.Manager startup adoption/recovery finishes.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := bmgr.StartRestore(context.Background(), backup.RestoreRequest{})
		var se *settings.Error
		if errors.As(err, &se) && se.Code == settings.CodeInvalid {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("backup manager did not finish startup recovery: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *dockerTestEnv) buildBackupHelperImage() string {
	e.t.Helper()
	arch := runtime.GOARCH
	if out, err := runDocker(e.dcDir, "version", "--format", "{{.Server.Arch}}"); err == nil && strings.TrimSpace(out) != "" {
		arch = strings.TrimSpace(out)
	}
	dir := e.t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "odac-backup"), "odac/cmd/odac-backup")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		e.t.Fatalf("build odac-backup: %v\n%s", err, out)
	}
	dockerfile := "FROM alpine:3.22\nCOPY odac-backup /app/bin/odac-backup\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		e.t.Fatal(err)
	}
	tag := fmt.Sprintf("odac-vtest-helper:%d", time.Now().UnixNano())
	mustDocker(e.t, e.dcDir, "build", "-q", "-t", tag, dir)
	e.trackImage(tag)
	return tag
}

func (e *dockerTestEnv) buildTestImage(tag, dockerfile string) string {
	e.t.Helper()
	dir := e.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		e.t.Fatal(err)
	}
	mustDocker(e.t, e.dcDir, "build", "-q", "-t", tag, dir)
	e.trackImage(tag)
	meta, err := e.client.ImageMeta(tag)
	if err != nil || meta.ID == "" {
		e.t.Fatalf("ImageMeta(%s): %+v, %v", tag, meta, err)
	}
	return meta.ID
}

func (e *dockerTestEnv) containerID(name string) string {
	e.t.Helper()
	return strings.TrimSpace(mustDocker(e.t, e.dcDir, "inspect", "--format", "{{.Id}}", name))
}

func (e *dockerTestEnv) appRecord(name string) map[string]any {
	e.t.Helper()
	var cp map[string]any
	e.cfg.View(func() {
		if app := e.m.getLocked(name); app != nil {
			raw, _ := json.Marshal(app)
			_ = json.Unmarshal(raw, &cp)
		}
	})
	if cp == nil {
		e.t.Fatalf("app record %q not found", name)
	}
	return cp
}

func waitUntil(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func uniqueAppName(prefix string) string {
	return fmt.Sprintf("odac-vtest-%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// Scenario 1: Postgres snapshot-backed patch tag switch.
func TestDockerSetImagePostgresWithSnapshot(t *testing.T) {
	const (
		oldImage = "postgres:16.15-alpine"
		newImage = "postgres:16-alpine"
		pgPass   = "pg-phase-g-pass"
	)
	name := uniqueAppName("pg")
	e := newDockerTestEnv(t, nil, nil, true, oldImage, newImage, "alpine:3.22")
	pgDir := filepath.Join(e.base, "apps", name, "pgdata")
	if err := os.MkdirAll(pgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":     float64(1),
			"name":   name,
			"type":   "container",
			"image":  oldImage,
			"active": true,
			"env": map[string]any{
				"manual": map[string]any{
					"POSTGRES_USER":     "app",
					"POSTGRES_PASSWORD": pgPass,
					"PGDATA":            "/var/lib/postgresql/data/pgdata",
				},
			},
			"volumes": []any{
				map[string]any{"host": pgDir, "container": "/var/lib/postgresql/data"},
			},
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	e.trackContainer(name)

	if err := e.m.run(float64(1), nil); err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	psql := func(sql string) (string, error) {
		out, err := runDocker(e.dcDir, "exec", "-e", "PGPASSWORD="+pgPass, name,
			"psql", "-h", "127.0.0.1", "-U", "app", "-tA", "-c", sql)
		return strings.TrimSpace(out), err
	}
	waitUntil(t, "postgres ready", 2*time.Minute, func() bool {
		_, err := psql("SELECT 1")
		return err == nil
	})
	if out, err := psql("CREATE TABLE items (id int primary key, val text); INSERT INTO items VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma');"); err != nil {
		t.Fatalf("seed postgres: %v (%s)", err, out)
	}

	res := e.m.SetImage(ImageSetPayload{
		Container:       name,
		Image:           newImage,
		Pull:            false,
		RequireSnapshot: true,
	})
	if !res.Success || !res.Changed {
		t.Fatalf("SetImage failed: %+v", res)
	}
	if res.Previous != oldImage || res.Image != newImage {
		t.Fatalf("unexpected images: %+v", res)
	}
	newMeta, err := e.client.ImageMeta(newImage)
	if err != nil {
		t.Fatalf("ImageMeta(%s): %v", newImage, err)
	}
	if res.ImageID != newMeta.ID {
		t.Fatalf("ImageID = %q, want %q", res.ImageID, newMeta.ID)
	}
	if !strings.HasPrefix(res.ImageDigest, "sha256:") {
		t.Fatalf("ImageDigest not populated: %q", res.ImageDigest)
	}
	if res.Snapshot == "" {
		t.Fatalf("expected version snapshot run id, got empty in %+v", res)
	}

	// Snapshot must be stored with event=version and dump=true, and stay out of deploy marks.
	pts := e.bmgr.SnapshotPoints(name)
	foundPoint := false
	for _, pt := range pts {
		if pt.Run == res.Snapshot {
			foundPoint = true
			if pt.Event != backup.EventVersion {
				t.Fatalf("snapshot point event = %q, want %q", pt.Event, backup.EventVersion)
			}
			if len(pt.Apps) != 1 || pt.Apps[0].App != name || !pt.Apps[0].Dump {
				t.Fatalf("snapshot point apps = %+v", pt.Apps)
			}
		}
	}
	if !foundPoint {
		t.Fatalf("snapshot run %q not found in SnapshotPoints: %+v", res.Snapshot, pts)
	}
	for _, d := range e.bmgr.SnapshotStatus().Deploys {
		if d.App == name {
			t.Fatalf("version snapshot must not appear in SnapshotStatus.Deploys: %+v", d)
		}
	}

	// Verify record updated, marker cleared, hold tag gone, container on new image, data intact.
	rec := e.appRecord(name)
	if rec["image"] != newImage {
		t.Fatalf("record image = %v, want %s", rec["image"], newImage)
	}
	if rec[imageChangeKey] != nil {
		t.Fatalf("imageChange marker leaked: %v", rec[imageChangeKey])
	}
	if _, err := e.client.ImageMeta(holdTag(1)); err == nil {
		t.Fatalf("hold tag %s was not removed after commit", holdTag(1))
	}
	st, err := e.client.InspectState(name)
	if err != nil || !st.Running || st.ImageID != newMeta.ID {
		t.Fatalf("container state = %+v, err=%v, want running on %s", st, err, newMeta.ID)
	}
	waitUntil(t, "upgraded postgres ready", 2*time.Minute, func() bool {
		_, err := psql("SELECT 1")
		return err == nil
	})
	got, err := psql("SELECT count(*) || '|' || string_agg(val, ',' ORDER BY id) FROM items")
	if err != nil || got != "3|alpha,beta,gamma" {
		t.Fatalf("seeded postgres data after switch = %q (err=%v), want 3|alpha,beta,gamma", got, err)
	}
}

// Scenario 2: MySQL snapshot-backed patch tag switch.
func TestDockerSetImageMySQLWithSnapshot(t *testing.T) {
	const (
		oldImage = "mysql:8.4.11"
		newImage = "mysql:8.4"
		myPass   = "my-phase-g-pass-123"
	)
	name := uniqueAppName("my")
	e := newDockerTestEnv(t, nil, nil, true, oldImage, newImage, "alpine:3.22")
	myDir := filepath.Join(e.base, "apps", name, "mysql")
	if err := os.MkdirAll(myDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":     float64(1),
			"name":   name,
			"type":   "container",
			"image":  oldImage,
			"active": true,
			"env": map[string]any{
				"manual": map[string]any{
					"MYSQL_ROOT_PASSWORD": myPass,
				},
			},
			"volumes": []any{
				map[string]any{"host": myDir, "container": "/var/lib/mysql"},
			},
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	e.trackContainer(name)

	if err := e.m.run(float64(1), nil); err != nil {
		t.Fatalf("start mysql: %v", err)
	}
	mysqlExec := func(sql string) (string, error) {
		cmd := exec.Command("docker", "exec", "-i", name, "sh", "-c", `mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -h127.0.0.1 -N -B`)
		cmd.Env = dockerEnvVars(e.dcDir)
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	waitUntil(t, "mysql ready", 3*time.Minute, func() bool {
		_, err := mysqlExec("SELECT 1;")
		return err == nil
	})
	if out, err := mysqlExec("CREATE DATABASE shop; CREATE TABLE shop.items (id int primary key, val varchar(64)) ENGINE=InnoDB; INSERT INTO shop.items VALUES (1, 'one'), (2, 'two'), (3, 'three');"); err != nil {
		t.Fatalf("seed mysql: %v (%s)", err, out)
	}

	res := e.m.SetImage(ImageSetPayload{
		Container:       name,
		Image:           newImage,
		Pull:            false,
		RequireSnapshot: true,
	})
	if !res.Success || !res.Changed {
		t.Fatalf("SetImage mysql failed: %+v", res)
	}
	if res.Previous != oldImage || res.Image != newImage {
		t.Fatalf("unexpected images: %+v", res)
	}
	if !strings.HasPrefix(res.ImageDigest, "sha256:") {
		t.Fatalf("ImageDigest not populated: %q", res.ImageDigest)
	}
	if res.Snapshot == "" {
		t.Fatalf("expected version snapshot run id, got empty in %+v", res)
	}

	pts := e.bmgr.SnapshotPoints(name)
	foundPoint := false
	for _, pt := range pts {
		if pt.Run == res.Snapshot && pt.Event == backup.EventVersion && len(pt.Apps) == 1 && pt.Apps[0].Dump {
			foundPoint = true
		}
	}
	if !foundPoint {
		t.Fatalf("mysql version snapshot %q not stored as dump point: %+v", res.Snapshot, pts)
	}

	newMeta, err := e.client.ImageMeta(newImage)
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.client.InspectState(name)
	if err != nil || !st.Running || st.ImageID != newMeta.ID {
		t.Fatalf("mysql container state = %+v, err=%v, want running on %s", st, err, newMeta.ID)
	}
	rec := e.appRecord(name)
	if rec["image"] != newImage || rec[imageChangeKey] != nil {
		t.Fatalf("mysql record after switch = %v", rec)
	}
	waitUntil(t, "upgraded mysql ready", 3*time.Minute, func() bool {
		_, err := mysqlExec("SELECT 1;")
		return err == nil
	})
	got, err := mysqlExec("SELECT group_concat(val ORDER BY id) FROM shop.items;")
	if err != nil || got != "one,two,three" {
		t.Fatalf("seeded mysql data after switch = %q (err=%v), want one,two,three", got, err)
	}
}

// Scenario 3: Health gate forced failure (immediate exit and failing HEALTHCHECK)
// triggers automatic rollback to previous image.
func TestDockerSetImageHealthGateRollback(t *testing.T) {
	name := uniqueAppName("gate")
	repo := fmt.Sprintf("odac-vtest-gate-%d", time.Now().UnixNano()%1_000_000_000)
	goodRef := repo + ":v1"
	badExitRef := repo + ":bad-exit"
	badHCRef := repo + ":bad-hc"

	e := newDockerTestEnv(t, nil, nil, false, "alpine:3.22")
	goodID := e.buildTestImage(goodRef, "FROM alpine:3.22\nLABEL v=1\nCMD [\"sleep\", \"600\"]\n")
	_ = e.buildTestImage(badExitRef, "FROM alpine:3.22\nLABEL v=bad-exit\nCMD [\"sh\", \"-c\", \"exit 1\"]\n")
	_ = e.buildTestImage(badHCRef, "FROM alpine:3.22\nLABEL v=bad-hc\nHEALTHCHECK --interval=1s --timeout=1s --start-period=0s --retries=1 CMD exit 1\nCMD [\"sleep\", \"600\"]\n")

	if meta, err := e.client.ImageMeta(badHCRef); err != nil || !meta.Healthcheck {
		t.Fatalf("badHCRef healthcheck meta = %+v, err=%v", meta, err)
	}

	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":     float64(1),
			"name":   name,
			"type":   "container",
			"image":  goodRef,
			"active": true,
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	e.trackContainer(name)

	if err := e.m.run(float64(1), nil); err != nil {
		t.Fatalf("start initial container: %v", err)
	}
	waitUntil(t, "initial container running", 30*time.Second, func() bool {
		st, err := e.client.InspectState(name)
		return err == nil && st.Running && st.ImageID == goodID
	})

	cases := []struct {
		name      string
		targetRef string
	}{
		{name: "ImmediateExit", targetRef: badExitRef},
		{name: "FailingHealthcheck", targetRef: badHCRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := e.m.SetImage(ImageSetPayload{Container: name, Image: tc.targetRef, Pull: false})
			if res.Success || !res.RolledBack {
				t.Fatalf("SetImage(%s) = %+v, want success=false rolledBack=true", tc.targetRef, res)
			}
			if res.Reason != ImageUnhealthy && res.Reason != ImageStartFailed {
				t.Fatalf("reason = %q, want %q or %q", res.Reason, ImageUnhealthy, ImageStartFailed)
			}
			st, err := e.client.InspectState(name)
			if err != nil || !st.Running || st.ImageID != goodID {
				t.Fatalf("after rollback container state = %+v (err=%v), want running on %s", st, err, goodID)
			}
			rec := e.appRecord(name)
			if rec["image"] != goodRef {
				t.Fatalf("record image = %v, want %s", rec["image"], goodRef)
			}
			if rec[imageChangeKey] != nil {
				t.Fatalf("imageChange marker leaked after rollback: %v", rec[imageChangeKey])
			}
			if _, err := e.client.ImageMeta(holdTag(1)); err == nil {
				t.Fatalf("hold tag %s leaked after rollback", holdTag(1))
			}
		})
	}
}

// Scenario 4: Mid-switch agent kill and crash recovery on a fresh Manager.
func TestDockerCrashRecovery(t *testing.T) {
	name := uniqueAppName("crash")
	repo := fmt.Sprintf("odac-vtest-crash-%d", time.Now().UnixNano()%1_000_000_000)
	rollingRef := repo + ":rolling"
	v2Ref := repo + ":v2"

	e := newDockerTestEnv(t, nil, nil, false, "alpine:3.22")
	v1ID := e.buildTestImage(rollingRef, "FROM alpine:3.22\nLABEL v=1\nCMD [\"sleep\", \"600\"]\n")
	v2ID := e.buildTestImage(v2Ref, "FROM alpine:3.22\nLABEL v=2\nCMD [\"sleep\", \"600\"]\n")

	// Simulate an interrupted rolling-tag switch:
	// - hold tag keeps v1 alive
	// - rollingRef was moved to v2 during the pull
	// - container is currently running v2
	// - persisted config names rollingRef and carries the imageChange marker
	hold := holdTag(1)
	if err := e.client.TagImage(v1ID, hold); err != nil {
		t.Fatalf("TagImage hold: %v", err)
	}
	e.trackImage(hold)
	if err := e.client.TagImage(v2ID, rollingRef); err != nil {
		t.Fatalf("TagImage rolling -> v2: %v", err)
	}

	e.trackContainer(name)
	mustDocker(t, e.dcDir, "run", "-d", "--name", name, "--network", "bridge", v2ID)
	waitUntil(t, "interrupted v2 container running", 30*time.Second, func() bool {
		st, err := e.client.InspectState(name)
		return err == nil && st.Running && st.ImageID == v2ID
	})

	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":     float64(1),
			"name":   name,
			"type":   "container",
			"image":  rollingRef,
			"active": true,
			imageChangeKey: map[string]any{
				"image":      rollingRef,
				"previous":   rollingRef,
				"previousId": v1ID,
				"hold":       hold,
			},
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	// Simulate a fresh agent process restarting from the persisted config on disk.
	freshCfg, err := config.Open(e.base)
	if err != nil {
		t.Fatalf("reopen config: %v", err)
	}
	fresh := New(freshCfg, filepath.Join(e.base, "logs"), Deps{Docker: e.client, Proxy: &dockerProxySpy{}})
	fresh.pollInterval = 25 * time.Millisecond
	fresh.scanPortInterval = 25 * time.Millisecond
	fresh.scanProbeInterval = 25 * time.Millisecond
	fresh.Init()
	t.Cleanup(func() {
		freshCfg.Mutate(func() {
			for _, app := range fresh.apps {
				app["_deleted"] = true
			}
		})
		fresh.endLogStream(name)
		fresh.bg.Wait()
	})

	// Before recovery runs, HoldApp must refuse an app carrying an unrecovered marker.
	if _, err := fresh.HoldApp(name); !errors.Is(err, ErrImageChangePending) {
		t.Fatalf("HoldApp before recovery = %v, want ErrImageChangePending", err)
	}

	// First Check pulse detects the orphaned marker, restores the tag, removes
	// the interrupted container, and clears the marker and hold tag.
	fresh.Check()
	fresh.bg.Wait()

	var markerAfter any
	freshCfg.View(func() {
		if app := fresh.getLocked(name); app != nil {
			markerAfter = app[imageChangeKey]
		}
	})
	if markerAfter != nil {
		t.Fatalf("imageChange marker not cleared by recovery: %v", markerAfter)
	}
	meta, err := e.client.ImageMeta(rollingRef)
	if err != nil || meta.ID != v1ID {
		t.Fatalf("rolling tag after recovery = %+v (err=%v), want %s", meta, err, v1ID)
	}
	if _, err := e.client.ImageMeta(hold); err == nil {
		t.Fatalf("hold tag %s still exists after recovery", hold)
	}
	if st, err := e.client.InspectState(name); err != nil || st.Exists {
		t.Fatalf("interrupted v2 container was not removed: %+v (err=%v)", st, err)
	}

	// Next Check pulse respawns the app on its restored previous image.
	fresh.Check()
	fresh.bg.Wait()
	waitUntil(t, "recovered v1 container running", 30*time.Second, func() bool {
		st, err := e.client.InspectState(name)
		return err == nil && st.Running && st.ImageID == v1ID
	})
}

// Scenario 5: Blue-Green rollback of a git app (both failing and succeeding).
func TestDockerBlueGreenRollback(t *testing.T) {
	name := uniqueAppName("bg")
	repo := buildRepo(name)
	buildV1 := "build_1700000001000_aaaa1111"
	buildV2 := "build_1700000002000_bbbb2222"
	buildBad := "build_1700000003000_cccc3333"
	commitV1 := strings.Repeat("a", 40)
	commitV2 := strings.Repeat("b", 40)

	e := newDockerTestEnv(t, nil, map[string]any{
		name + ".example.test": map[string]any{"appId": name},
	}, false, "alpine:3.22", "busybox:1.37")

	// Start a lightweight sidecar on odac-network so L7 HTTP probes reach the
	// container's bridge IP identically on macOS Docker Desktop and Linux.
	if err := e.client.EnsureNetworks([]string{docker.DefaultNetwork}); err != nil {
		t.Fatalf("EnsureNetworks: %v", err)
	}
	prober := uniqueAppName("prober")
	e.trackContainer(prober)
	mustDocker(t, e.dcDir, "run", "-d", "--name", prober, "--network", docker.DefaultNetwork, "alpine:3.22", "sleep", "600")
	e.m.httpProbe = func(ip string, port int, timeout time.Duration, method string) bool {
		sec := max(1, int(timeout/time.Second))
		args := []string{"exec", prober, "wget", "-q", "-T", strconv.Itoa(sec), "-O", "/dev/null"}
		if method == "HEAD" {
			args = append(args, "--spider")
		}
		args = append(args, fmt.Sprintf("http://%s:%d/", ip, port))
		_, err := runDocker(e.dcDir, args...)
		return err == nil
	}
	httpBody := func(ip string, port int) (string, error) {
		out, err := runDocker(e.dcDir, "exec", prober, "wget", "-q", "-T", "3", "-O", "-", fmt.Sprintf("http://%s:%d/", ip, port))
		return strings.TrimSpace(out), err
	}

	v1Ref := buildRef(name, buildV1)
	v2Ref := buildRef(name, buildV2)
	badRef := buildRef(name, buildBad)
	v1ID := e.buildTestImage(v1Ref, "FROM busybox:1.37\nRUN mkdir -p /www && echo build-v1 > /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"3000\", \"-h\", \"/www\"]\n")
	v2ID := e.buildTestImage(v2Ref, "FROM busybox:1.37\nRUN mkdir -p /www && echo build-v2 > /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"3000\", \"-h\", \"/www\"]\n")
	// Bad build accepts TCP on 3000 (passes port check) and closes immediately
	// without speaking HTTP, tripping the L7 HTTP readiness gate cleanly.
	_ = e.buildTestImage(badRef, "FROM busybox:1.37\nCMD [\"sh\", \"-c\", \"while true; do nc -l -p 3000 -e /bin/true; done\"]\n")

	if err := e.client.TagImage(v2ID, repo); err != nil {
		t.Fatalf("TagImage %s: %v", repo, err)
	}
	e.trackImage(repo)

	// Persist buildV1's applog summary so Rollback restores commitSha.
	logger := applog.New(e.m.logsRoot, name)
	if err := logger.Init(); err != nil {
		t.Fatal(err)
	}
	b1, err := logger.NewBuildStream(buildV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	b1.SetMetadata("commitSha", commitV1)
	b1.SetMetadata("runtime", map[string]any{"strategy": "static", "image": "busybox:1.37"})
	b1.Finalize(true)

	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":        float64(1),
			"name":      name,
			"type":      "git",
			"image":     repo,
			"active":    true,
			buildIDKey:  buildV2,
			"commitSha": commitV2,
			"ports":     []any{map[string]any{"host": "proxy", "container": float64(3000)}},
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	e.trackContainer(name)

	if !e.m.zddEligible(name, 1) {
		t.Fatal("git app with domain must be Blue-Green eligible")
	}
	if err := e.m.run(float64(1), nil); err != nil {
		t.Fatalf("start git app: %v", err)
	}
	waitUntil(t, "v2 serving HTTP", 30*time.Second, func() bool {
		ip, err := e.client.GetIP(name)
		if err != nil || ip == "" {
			return false
		}
		body, err := httpBody(ip, 3000)
		return err == nil && body == "build-v2"
	})
	origCID := e.containerID(name)

	// 1) Failing Blue-Green rollback must leave the old container untouched.
	failRes := e.m.Rollback(RollbackPayload{Container: name, BuildID: buildBad})
	if failRes.Success || !failRes.RolledBack || failRes.Reason != ImageUnhealthy {
		t.Fatalf("failing Blue-Green rollback = %+v, want success=false rolledBack=true reason=%s", failRes, ImageUnhealthy)
	}
	if cid := e.containerID(name); cid != origCID {
		t.Fatalf("failing Blue-Green rollback replaced the live container: cid=%s want=%s", cid, origCID)
	}
	if meta, err := e.client.ImageMeta(repo); err != nil || meta.ID != v2ID {
		t.Fatalf("%s tag after failed rollback = %+v (err=%v), want %s", repo, meta, err, v2ID)
	}
	if ip, err := e.client.GetIP(name); err != nil {
		t.Fatalf("GetIP after failed rollback: %v", err)
	} else if body, err := httpBody(ip, 3000); err != nil || body != "build-v2" {
		t.Fatalf("live container body after failed rollback = %q (err=%v), want build-v2", body, err)
	}
	if out, err := runDocker(e.dcDir, "ps", "-a", "--filter", "name=^/"+name+"-green-", "--format", "{{.Names}}"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("leaked green container after failed rollback: %q (err=%v)", out, err)
	}

	// 2) Succeeding Blue-Green rollback switches cleanly to buildV1.
	syncsBefore, purgesBefore := e.proxy.counts()
	okRes := e.m.Rollback(RollbackPayload{Container: name, BuildID: buildV1})
	if !okRes.Success || !okRes.Changed {
		t.Fatalf("Blue-Green rollback to %s failed: %+v", buildV1, okRes)
	}
	if okRes.BuildID != buildV1 || okRes.Previous != buildV2 || okRes.CommitSha != commitV1 || okRes.ImageID != v1ID {
		t.Fatalf("Blue-Green rollback result = %+v", okRes)
	}
	newCID := e.containerID(name)
	if newCID == origCID {
		t.Fatalf("expected a new container after Blue-Green rollback, cid stayed %s", newCID)
	}
	st, err := e.client.InspectState(name)
	if err != nil || !st.Running || st.ImageID != v1ID {
		t.Fatalf("container after rollback = %+v (err=%v), want running on %s", st, err, v1ID)
	}
	if ip, err := e.client.GetIP(name); err != nil {
		t.Fatalf("GetIP after rollback: %v", err)
	} else if body, err := httpBody(ip, 3000); err != nil || body != "build-v1" {
		t.Fatalf("HTTP body after rollback = %q (err=%v), want build-v1", body, err)
	}
	syncsAfter, purgesAfter := e.proxy.counts()
	if syncsAfter <= syncsBefore || purgesAfter <= purgesBefore {
		t.Fatalf("expected Blue-Green proxy sync and purge: syncs %d->%d, purges %d->%d", syncsBefore, syncsAfter, purgesBefore, purgesAfter)
	}
	rec := e.appRecord(name)
	if rec[buildIDKey] != buildV1 || rec["commitSha"] != commitV1 || rec[imageChangeKey] != nil || rec["activeContainerId"] != nil {
		t.Fatalf("record after Blue-Green rollback = %v", rec)
	}
	if out, err := runDocker(e.dcDir, "ps", "-a", "--filter", "name=^/"+name+"-green-", "--format", "{{.Names}}"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("leaked green container after rollback: %q (err=%v)", out, err)
	}
}

// Scenario 6: Bidirectional restore vs switch contention and in-place
// image_mismatch refusal on a real snapshot repository.
func TestDockerRestoreContentionAndImageMismatch(t *testing.T) {
	const (
		pg16Old = "postgres:16.15-alpine"
		pg16New = "postgres:16-alpine"
		pg17    = "postgres:17-alpine"
		pgPass  = "pg-contention-pass"
	)
	name := uniqueAppName("cm")
	e := newDockerTestEnv(t, nil, nil, true, pg16Old, pg16New, "alpine:3.22")
	pgDir := filepath.Join(e.base, "apps", name, "pgdata")
	if err := os.MkdirAll(pgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.Mutate(func() {
		e.m.apps = []map[string]any{{
			"id":     float64(1),
			"name":   name,
			"type":   "container",
			"image":  pg16Old,
			"active": true,
			"env": map[string]any{
				"manual": map[string]any{
					"POSTGRES_USER":     "app",
					"POSTGRES_PASSWORD": pgPass,
					"PGDATA":            "/var/lib/postgresql/data/pgdata",
				},
			},
			"volumes": []any{
				map[string]any{"host": pgDir, "container": "/var/lib/postgresql/data"},
			},
		}}
		e.m.saveAppsLocked()
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	e.trackContainer(name)

	if err := e.m.run(float64(1), nil); err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	psql := func(sql string) (string, error) {
		out, err := runDocker(e.dcDir, "exec", "-e", "PGPASSWORD="+pgPass, name,
			"psql", "-h", "127.0.0.1", "-U", "app", "-tA", "-c", sql)
		return strings.TrimSpace(out), err
	}
	waitUntil(t, "postgres ready", 2*time.Minute, func() bool {
		_, err := psql("SELECT 1")
		return err == nil
	})
	if out, err := psql("CREATE TABLE notes (id int primary key, body text); INSERT INTO notes VALUES (1, 'pg16-seed');"); err != nil {
		t.Fatalf("seed postgres: %v (%s)", err, out)
	}

	// Part A1: HoldApp active -> SetImage must answer busy (never invalid).
	hold, err := e.m.HoldApp(name)
	if err != nil {
		t.Fatalf("HoldApp: %v", err)
	}
	busyRes := e.m.SetImage(ImageSetPayload{Container: name, Image: pg16New, Pull: false})
	if busyRes.Success || busyRes.Reason != ImageBusy || busyRes.RolledBack {
		_ = hold.Release(true)
		t.Fatalf("SetImage while HoldApp active = %+v, want reason=%s", busyRes, ImageBusy)
	}
	if err := hold.Release(true); err != nil {
		t.Fatalf("hold.Release(true): %v", err)
	}
	waitUntil(t, "postgres ready after release", 2*time.Minute, func() bool {
		_, err := psql("SELECT 1")
		return err == nil
	})

	// Part A2: SetImage processing lock active -> HoldApp must return ErrAppBusy.
	// Run a real SetImage in a goroutine (takes a version snapshot + 4s health gate)
	// and call HoldApp while SetImage holds the processing lock.
	setDone := make(chan ImageSetResult, 1)
	go func() {
		setDone <- e.m.SetImage(ImageSetPayload{
			Container:       name,
			Image:           pg16New,
			Pull:            false,
			RequireSnapshot: true,
		})
	}()
	waitUntil(t, "SetImage acquired processing lock", 15*time.Second, func() bool {
		return e.m.isProcessing(1)
	})
	if _, err := e.m.HoldApp(name); !errors.Is(err, ErrAppBusy) {
		<-setDone
		t.Fatalf("HoldApp while SetImage running = %v, want ErrAppBusy", err)
	}
	setRes := <-setDone
	if !setRes.Success || !setRes.Changed || setRes.Snapshot == "" {
		t.Fatalf("SetImage during contention test = %+v", setRes)
	}
	snapRun := setRes.Snapshot

	// Part B1: Same-major preview (point=postgres:16.15-alpine, current=postgres:16-alpine)
	// reports both images and is NOT blocked.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	samePreview, err := e.bmgr.PreviewSnapshotRestore(ctx, backup.SnapshotRestoreRequest{
		Run:  snapRun,
		Mode: backup.RestoreInPlace,
	})
	if err != nil {
		t.Fatalf("PreviewSnapshotRestore (same major): %v", err)
	}
	if len(samePreview.Apps) != 1 || samePreview.Apps[0].Image == nil {
		t.Fatalf("same-major preview apps = %+v", samePreview.Apps)
	}
	if samePreview.Apps[0].Image.Point != pg16Old || samePreview.Apps[0].Image.Current != pg16New || samePreview.Apps[0].Blocked != "" {
		t.Fatalf("same-major preview = %+v", samePreview.Apps[0])
	}

	// Part B2: Move the app record across a major boundary (postgres:17-alpine).
	// In-place PreviewSnapshotRestore must report image: {point, current} and
	// blocked: image_mismatch; StartSnapshotRestore must refuse with CodeImageMismatch.
	e.cfg.Mutate(func() {
		if app := e.m.getLocked(name); app != nil {
			app["image"] = pg17
			e.m.saveAppsLocked()
		}
	})
	if err := e.cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	preview, err := e.bmgr.PreviewSnapshotRestore(ctx, backup.SnapshotRestoreRequest{
		Run:  snapRun,
		Mode: backup.RestoreInPlace,
	})
	if err != nil {
		t.Fatalf("PreviewSnapshotRestore (cross major): %v", err)
	}
	if len(preview.Apps) != 1 {
		t.Fatalf("preview apps = %+v, want 1", preview.Apps)
	}
	pa := preview.Apps[0]
	if pa.Image == nil || pa.Image.Point != pg16Old || pa.Image.Current != pg17 {
		t.Fatalf("preview image = %+v, want point=%s current=%s", pa.Image, pg16Old, pg17)
	}
	if pa.Blocked != backup.BlockedImageMismatch {
		t.Fatalf("preview blocked = %q, want %q", pa.Blocked, backup.BlockedImageMismatch)
	}

	_, err = e.bmgr.StartSnapshotRestore(ctx, backup.SnapshotRestoreRequest{
		Run:  snapRun,
		Mode: backup.RestoreInPlace,
	})
	var se *settings.Error
	if !errors.As(err, &se) || se.Code != settings.CodeImageMismatch {
		t.Fatalf("StartSnapshotRestore error = %v, want *settings.Error with code %q", err, settings.CodeImageMismatch)
	}
	// Refusal must leave the live container untouched.
	if st, err := e.client.InspectState(name); err != nil || !st.Running {
		t.Fatalf("container should stay running after refused restore: %+v (err=%v)", st, err)
	}
}
