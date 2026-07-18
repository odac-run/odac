package supervise

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/logx"
)

// shortTempDir avoids t.TempDir for socket dirs: unix socket paths must stay
// under ~104 bytes and t.TempDir embeds the full test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "odacsup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func copyTestBinary(t *testing.T, dst string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o755); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	sup    *Supervisor
	binDir string
	runDir string
	synced chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("GO_TEST_FAKE_MODULE", "1")

	f := &fixture{
		binDir: t.TempDir(),
		runDir: shortTempDir(t),
		synced: make(chan struct{}, 16),
	}
	copyTestBinary(t, filepath.Join(f.binDir, "odac-proxy"))

	f.sup = New(Options{
		Name:      "proxy",
		Binary:    "odac-proxy",
		SocketEnv: "ODAC_SOCKET_PATH",
		Display:   "Proxy",
		BinDir:    f.binDir,
		RunDir:    f.runDir,
		Log:       logx.New("Proxy"),
		OnSync:    func() { f.synced <- struct{}{} },
	})
	f.sup.SyncDelay = 30 * time.Millisecond
	f.sup.CleanupDelay = 30 * time.Millisecond
	t.Cleanup(f.sup.Stop)
	return f
}

// spawnOrphan starts a fake module directly (simulating a survivor of a
// previous orchestrator) and returns its PID once the socket is bound, plus
// an idempotent kill-and-reap func (a killed-but-unreaped zombie would still
// pass pidAlive on unix).
func (f *fixture) spawnOrphan(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command(filepath.Join(f.binDir, "odac-proxy"))
	cmd.Env = append(os.Environ(), "ODAC_SOCKET_PATH="+f.sup.SocketPath())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	reap := func() {
		once.Do(func() {
			cmd.Process.Kill()
			cmd.Wait()
		})
	}
	t.Cleanup(reap)
	waitFor(t, 5*time.Second, "orphan socket", func() bool {
		_, err := os.Stat(f.sup.SocketPath())
		return err == nil
	})
	return cmd.Process.Pid, reap
}

func (f *fixture) writePidFile(t *testing.T, pid int) {
	t.Helper()
	if err := os.WriteFile(f.sup.PidFile(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pidFileContent(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func TestSpawnStartsDetachedProcess(t *testing.T) {
	f := newFixture(t)
	f.sup.Ensure()

	if !f.sup.Running() {
		t.Fatal("supervisor not running after Ensure")
	}
	pid := f.sup.Pid()
	if pid <= 0 {
		t.Fatalf("Pid() = %d", pid)
	}
	if got := pidFileContent(t, f.sup.PidFile()); got != strconv.Itoa(pid) {
		t.Errorf("pid file = %q, want %d", got, pid)
	}
	waitFor(t, 5*time.Second, "control socket", func() bool {
		_, err := os.Stat(f.sup.SocketPath())
		return err == nil
	})
	if !pidAlive(pid) {
		t.Error("spawned process not alive")
	}

	// Second Ensure is a no-op while the child lives.
	f.sup.Ensure()
	if f.sup.Pid() != pid {
		t.Errorf("Ensure respawned a live child: pid %d -> %d", pid, f.sup.Pid())
	}
}

func TestSpawnSchedulesSync(t *testing.T) {
	f := newFixture(t)
	f.sup.Ensure()
	select {
	case <-f.synced:
	case <-time.After(2 * time.Second):
		t.Fatal("OnSync never fired after spawn")
	}
}

func TestExitWatcherClearsAndRespawns(t *testing.T) {
	f := newFixture(t)
	f.sup.Ensure()
	pid := f.sup.Pid()

	terminate(pid)
	waitFor(t, 5*time.Second, "exit watcher", func() bool { return !f.sup.Running() })
	waitFor(t, 5*time.Second, "pid file removal", func() bool {
		return pidFileContent(t, f.sup.PidFile()) == ""
	})

	// The 1s check tick would call Ensure — a fresh child must spawn.
	f.sup.Ensure()
	if !f.sup.Running() || f.sup.Pid() == pid {
		t.Fatalf("respawn failed: running=%v pid %d -> %d", f.sup.Running(), pid, f.sup.Pid())
	}
}

func TestAdoptOrphan(t *testing.T) {
	f := newFixture(t)
	orphan, _ := f.spawnOrphan(t)
	f.writePidFile(t, orphan)

	f.sup.Ensure()

	if got := f.sup.Pid(); got != orphan {
		t.Fatalf("adopted pid = %d, want orphan %d", got, orphan)
	}
	if got := pidFileContent(t, f.sup.PidFile()); got != strconv.Itoa(orphan) {
		t.Errorf("pid file rewritten to %q during adoption", got)
	}
	select {
	case <-f.synced:
	case <-time.After(2 * time.Second):
		t.Fatal("OnSync never fired after adoption")
	}
}

// Deviation from Node (which never notices adopted orphans dying): a dead
// adopted process is detected on the next Ensure and replaced.
func TestAdoptedDeathRespawns(t *testing.T) {
	f := newFixture(t)
	orphan, reap := f.spawnOrphan(t)
	f.writePidFile(t, orphan)
	f.sup.Ensure()
	if f.sup.Pid() != orphan {
		t.Fatalf("adoption failed: pid %d, want %d", f.sup.Pid(), orphan)
	}

	reap()
	waitFor(t, 5*time.Second, "orphan death", func() bool { return !pidAlive(orphan) })

	f.sup.Ensure()
	if !f.sup.Running() || f.sup.Pid() == orphan {
		t.Fatalf("dead adopted orphan not replaced: running=%v pid=%d", f.sup.Running(), f.sup.Pid())
	}
}

func TestAdoptSkipsWhenSocketMissing(t *testing.T) {
	f := newFixture(t)
	// A live PID (our own) but no socket file: PID reuse or crashed module.
	f.writePidFile(t, os.Getpid())

	f.sup.Ensure()

	if pid := f.sup.Pid(); pid == os.Getpid() || pid == 0 {
		t.Fatalf("pid = %d, want a fresh spawn (self pid %d must not be adopted)", pid, os.Getpid())
	}
	if got := pidFileContent(t, f.sup.PidFile()); got != strconv.Itoa(f.sup.Pid()) {
		t.Errorf("pid file = %q, want fresh pid %d", got, f.sup.Pid())
	}
}

func TestAdoptRejectsCmdlineMismatch(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Skip("/proc not available on this platform")
	}
	f := newFixture(t)
	// Live PID (ours), socket file present, but cmdline is "supervise.test",
	// not "odac-proxy" — the triple check's last leg must reject it.
	f.writePidFile(t, os.Getpid())
	if err := os.WriteFile(f.sup.SocketPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	f.sup.Ensure()

	if pid := f.sup.Pid(); pid == os.Getpid() {
		t.Fatal("PID-reuse process was adopted despite cmdline mismatch")
	}
}

func TestUpdateModeForcesFreshSpawn(t *testing.T) {
	f := newFixture(t)
	orphan, _ := f.spawnOrphan(t)
	f.writePidFile(t, orphan)
	t.Setenv("ODAC_UPDATE_MODE", "true")

	f.sup.Ensure()

	pid := f.sup.Pid()
	if pid == orphan || pid == 0 {
		t.Fatalf("pid = %d, want fresh spawn distinct from orphan %d", pid, orphan)
	}
	// Update mode overwrites the PID file instead of failing with EEXIST.
	if got := pidFileContent(t, f.sup.PidFile()); got != strconv.Itoa(pid) {
		t.Errorf("pid file = %q, want %d", got, pid)
	}
}

func TestPrevInstanceFilesGCd(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ODAC_PREVIOUS_INSTANCE_ID", "oldinst")
	prevPid := filepath.Join(f.runDir, "proxy-oldinst.pid")
	prevSock := filepath.Join(f.runDir, "proxy-oldinst.sock")
	for _, p := range []string{prevPid, prevSock} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	f.sup.Ensure()

	waitFor(t, 5*time.Second, "previous-instance GC", func() bool {
		_, e1 := os.Stat(prevPid)
		_, e2 := os.Stat(prevSock)
		return os.IsNotExist(e1) && os.IsNotExist(e2)
	})
}

func TestStopTerminatesAndUnlinksSocket(t *testing.T) {
	f := newFixture(t)
	f.sup.Ensure()
	pid := f.sup.Pid()
	waitFor(t, 5*time.Second, "control socket", func() bool {
		_, err := os.Stat(f.sup.SocketPath())
		return err == nil
	})

	f.sup.Stop()

	if f.sup.Running() {
		t.Error("Running() true after Stop")
	}
	if _, err := os.Stat(f.sup.SocketPath()); !os.IsNotExist(err) {
		t.Errorf("socket file not unlinked by Stop (err=%v)", err)
	}
	waitFor(t, 5*time.Second, "process termination", func() bool { return !pidAlive(pid) })
	waitFor(t, 5*time.Second, "pid file removal by exit watcher", func() bool {
		return pidFileContent(t, f.sup.PidFile()) == ""
	})
}

func TestExitWatcherLeavesSuccessorPidFile(t *testing.T) {
	f := newFixture(t)
	f.sup.Ensure()
	first := f.sup.Pid()

	f.sup.Stop()
	// Immediately restart: the successor claims the PID file; the first
	// child's late exit watcher must not delete it.
	waitFor(t, 5*time.Second, "first pid file release", func() bool {
		return pidFileContent(t, f.sup.PidFile()) == ""
	})
	f.sup.Ensure()
	second := f.sup.Pid()
	if second == 0 || second == first {
		t.Fatalf("restart failed: pid %d -> %d", first, second)
	}

	time.Sleep(100 * time.Millisecond) // give any stray watcher a chance to misbehave
	if got := pidFileContent(t, f.sup.PidFile()); got != strconv.Itoa(second) {
		t.Errorf("successor pid file = %q, want %d", got, second)
	}
}

func TestClaimPidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy-default.pid")

	if err := claimPidFile(path, 111, false); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := claimPidFile(path, 222, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second claim err = %v, want fs.ErrExist", err)
	}
	if err := claimPidFile(path, 333, true); err != nil {
		t.Fatalf("update-mode claim: %v", err)
	}
	if got := pidFileContent(t, path); got != "333" {
		t.Errorf("pid file = %q, want 333", got)
	}
}

func TestClaimRaceKillsRedundantChild(t *testing.T) {
	f := newFixture(t)
	f.sup.claimPid = func(string, int, bool) error { return fs.ErrExist }

	f.sup.Ensure()

	if f.sup.Running() {
		t.Error("redundant child kept after PID-file race")
	}
	if got := pidFileContent(t, f.sup.PidFile()); got != "" {
		t.Errorf("pid file unexpectedly written: %q", got)
	}
}

func TestBinaryMissingIsLoggedNotFatal(t *testing.T) {
	f := newFixture(t)
	os.Remove(filepath.Join(f.binDir, "odac-proxy"))

	f.sup.Ensure()

	if f.sup.Running() {
		t.Error("Running() true without a binary")
	}
}

func TestInstanceIDNamesRunFiles(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ODAC_INSTANCE_ID", "abc42")
	if got := filepath.Base(f.sup.PidFile()); got != "proxy-abc42.pid" {
		t.Errorf("pid file name = %q", got)
	}
	if got := filepath.Base(f.sup.SocketPath()); got != "proxy-abc42.sock" {
		t.Errorf("socket name = %q", got)
	}
}
