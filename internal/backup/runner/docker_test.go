package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/docker"
)

// The tests below run against a real Docker daemon: helper containers
// built from a throwaway image holding this tree's odac-backup. They are
// off unless ODAC_DOCKER_TESTS=1, because they need a daemon, pull alpine
// and take about a minute.
const dockerTestsEnv = "ODAC_DOCKER_TESTS"

const testServerID = "0123456789abcdef0123456789abcdef"

type dockerEnv struct {
	client *docker.Client
	image  string
	app    string
}

func setupDocker(t *testing.T) *dockerEnv {
	t.Helper()
	if os.Getenv(dockerTestsEnv) != "1" {
		t.Skipf("set %s=1 to run against a Docker daemon", dockerTestsEnv)
	}
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
	dockerfile := "FROM alpine:3.22\nCOPY odac-backup /app/bin/odac-backup\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("odac-backup-test:%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", tag).Run() })

	// The "database app" whose network namespace the dump joins.
	app := fmt.Sprintf("odac-backup-test-db-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", app, "alpine:3.22", "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("start app: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", app).Run() })
	return &dockerEnv{client: c, image: tag, app: app}
}

func (e *dockerEnv) runner(t *testing.T) *Runner {
	t.Helper()
	helper, dump := DefaultLimits(2<<30, 2)
	r, err := New(Config{
		Docker:     FromClient(e.client),
		Binary:     "/app/bin/odac-backup",
		Image:      e.image,
		Limits:     helper,
		DumpLimits: dump,
		DumpRate:   DefaultDumpRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type repoDirs struct{ cache, target, src string }

func newRepoDirs(t *testing.T) repoDirs {
	t.Helper()
	// macOS's TMPDIR is a symlink Docker Desktop resolves; mount the target.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := repoDirs{cache: filepath.Join(base, "cache"), target: filepath.Join(base, "target"), src: filepath.Join(base, "src")}
	for _, p := range []string{d.cache, d.target, filepath.Join(d.src, "sub")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		body := bytes.Repeat([]byte(strconv.Itoa(i)), 50000+i)
		if err := os.WriteFile(filepath.Join(d.src, "sub", fmt.Sprintf("f%d", i)), body, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// The helper reads app data owned by any user.
	if err := os.WriteFile(filepath.Join(d.src, "private"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d repoDirs) request(op string) *proto.Request {
	return &proto.Request{
		Op: op,
		Repo: proto.Repo{
			Storage:  proto.Storage{Filesystem: &proto.FilesystemStorage{Path: "/srv"}},
			Password: "correct horse battery staple",
			ServerID: testServerID,
			CacheDir: d.cache,
		},
	}
}

func seq(n int) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.Bytes()
}

func TestDockerBackupAndRestore(t *testing.T) {
	env := setupDocker(t)
	r := env.runner(t)
	d := newRepoDirs(t)
	target := &Target{Root: d.target}
	ctx := context.Background()

	if res, err := r.Run(ctx, Invocation{Request: d.request(proto.OpInit), Target: target}); err != nil || !res.Created {
		t.Fatalf("init: %+v, %v", res, err)
	}

	req := d.request(proto.OpBackup)
	req.Backup = &proto.Backup{Run: "r1", Trigger: "manual", Items: []proto.Item{
		{Source: proto.Source{Job: "j", App: "db", Name: "dump"}, Consistency: "consistent", Stream: "dump.sql"},
		{Source: proto.Source{Job: "j", App: "web", Name: "data"}, Consistency: "live", Dir: d.src},
	}}
	var snaps []proto.Snapshot
	res, err := r.Run(ctx, Invocation{
		Request: req,
		Target:  target,
		Dump:    &docker.DumpSpec{Image: "alpine:3.22", App: env.app, Cmd: []string{"seq", "1", "200000"}},
		Handler: proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }},
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if res.Count != 2 || len(snaps) != 2 || snaps[1].Files != 21 {
		t.Fatalf("backup: %+v, snapshots %+v", res, snaps)
	}
	if want := int64(len(seq(200000))); snaps[0].Bytes != want {
		t.Fatalf("dump stored %d bytes, want %d", snaps[0].Bytes, want)
	}

	restore := d.request(proto.OpRestore)
	restore.Restore = &proto.Restore{ID: snaps[0].ID}
	var out bytes.Buffer
	if _, err := r.Run(ctx, Invocation{Request: restore, Target: target, Handler: proto.Handler{Output: &out}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !bytes.Equal(out.Bytes(), seq(200000)) {
		t.Fatalf("restored %d bytes that differ from the dump", out.Len())
	}

	// A dump that fails stores nothing and says why.
	req.Backup.Run = "r2"
	snaps = nil
	_, err = r.Run(ctx, Invocation{
		Request: req,
		Target:  target,
		Dump:    &docker.DumpSpec{Image: "alpine:3.22", App: env.app, Cmd: []string{"sh", "-c", "seq 1 100000; echo 'pg_dump: lost connection' >&2; exit 3"}},
		Handler: proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }},
	})
	if err == nil || !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "lost connection") {
		t.Fatalf("failed dump: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("a failed dump stored %+v", snaps)
	}
	list := d.request(proto.OpList)
	list.List = &proto.List{Run: "r2"}
	if res, err := r.Run(ctx, Invocation{Request: list, Target: target}); err != nil || res.Count != 0 {
		t.Fatalf("list after a failed dump: %+v, %v", res, err)
	}

	assertNoBackupContainers(t, env.client)
}

// An update hands a run to the next server, which reads its result from
// the helper's log.
func TestDockerReleaseAndAdopt(t *testing.T) {
	env := setupDocker(t)
	d := newRepoDirs(t)
	target := &Target{Root: d.target}
	ctx := context.Background()
	old := env.runner(t)
	if _, err := old.Run(ctx, Invocation{Request: d.request(proto.OpInit), Target: target}); err != nil {
		t.Fatal(err)
	}

	req := d.request(proto.OpBackup)
	req.Backup = &proto.Backup{Run: "r1", Trigger: "manual", Items: []proto.Item{
		{Source: proto.Source{Job: "j", App: "db", Name: "dump"}, Consistency: "consistent", Stream: "dump.sql"},
		{Source: proto.Source{Job: "j", App: "web", Name: "data"}, Consistency: "live", Dir: d.src},
	}}
	var once sync.Once
	_, err := old.Run(ctx, Invocation{
		Request: req,
		Target:  target,
		Labels:  map[string]string{"odac.backup.job": "j"},
		Dump:    &docker.DumpSpec{Image: "alpine:3.22", App: env.app, Cmd: []string{"seq", "1", "50000"}},
		// The stream is stored; the directory is still to come.
		Handler: proto.Handler{Snapshot: func(proto.Snapshot) { once.Do(old.Release) }},
	})
	if !errors.Is(err, ErrReleased) {
		t.Fatalf("Run = %v, want ErrReleased", err)
	}

	orphans, err := env.runner(t).Adopt()
	if err != nil {
		t.Fatal(err)
	}
	var mine []*Orphan
	for _, o := range orphans {
		if o.Labels()[LabelRun] == "r1" && o.Labels()["odac.backup.job"] == "j" {
			mine = append(mine, o)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("adopted %d helpers for the run", len(mine))
	}
	var snaps []proto.Snapshot
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := mine[0].Follow(fctx, proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }})
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if res.Count != 2 || len(snaps) != 2 || snaps[0].Run != "r1" {
		t.Fatalf("followed %+v, snapshots %+v", res, snaps)
	}
	assertNoBackupContainers(t, env.client)
}

func assertNoBackupContainers(t *testing.T, c *docker.Client) {
	t.Helper()
	list, err := c.BackupContainers()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		t.Errorf("left behind: %s (%s, %v)", b.Name, b.Role, b.Labels)
	}
}
