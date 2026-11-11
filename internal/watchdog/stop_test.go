package watchdog

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"odac/internal/config"
)

// TestHelperServer stands in for odac-server when the stop tests re-run this
// binary as the watchdog's child.
func TestHelperServer(t *testing.T) {
	mode := os.Getenv("ODAC_WATCHDOG_HELPER")
	if mode == "" {
		return
	}
	dir := os.Getenv("ODAC_WATCHDOG_HELPER_DIR")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644)
	if mode == "ignore" {
		for {
			<-sig
		}
	}
	<-sig
	os.WriteFile(filepath.Join(dir, "graceful"), nil, 0o644)
	os.Exit(143)
}

func newHelperWatchdog(t *testing.T, mode string) (*Watchdog, string) {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("ODAC_WATCHDOG_HELPER", mode)
	t.Setenv("ODAC_WATCHDOG_HELPER_DIR", dir)
	w := New(cfg, []string{os.Args[0], "-test.run=^TestHelperServer$"})
	w.reap = func(int) {}
	return w, dir
}

func runInBackground(w *Watchdog) <-chan int {
	codes := make(chan int, 1)
	go func() {
		code, _ := w.runServerOnce()
		codes <- code
	}()
	return codes
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestStopChildLetsTheServerShutDown(t *testing.T) {
	w, dir := newHelperWatchdog(t, "trap")
	codes := runInBackground(w)
	waitForFile(t, filepath.Join(dir, "ready"))

	start := time.Now()
	w.stopChild()
	if took := time.Since(start); took >= w.stopGrace {
		t.Fatalf("stopChild took %v, want the child's own exit", took)
	}
	if _, err := os.Stat(filepath.Join(dir, "graceful")); err != nil {
		t.Fatal("child was killed before it could shut down")
	}
	if code := <-codes; code != 143 {
		t.Fatalf("exit code %d, want 143", code)
	}
}

func TestStopChildKillsAfterGrace(t *testing.T) {
	w, dir := newHelperWatchdog(t, "ignore")
	w.stopGrace = 200 * time.Millisecond
	codes := runInBackground(w)
	waitForFile(t, filepath.Join(dir, "ready"))

	w.stopChild()
	select {
	case code := <-codes:
		if code != -1 {
			t.Fatalf("exit code %d, want -1 (killed)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child survived the grace period")
	}
}

func TestStoppingWatchdogStartsNoServer(t *testing.T) {
	w, dir := newHelperWatchdog(t, "trap")
	w.stopChild()
	if _, err := w.runServerOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
		t.Fatal("a stopping watchdog started the server")
	}
}
