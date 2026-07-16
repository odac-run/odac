package watchdog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"odac/internal/config"
)

func TestStreamIncrementalAppend(t *testing.T) {
	file := filepath.Join(t.TempDir(), "test.log")
	st := stream{flushed: -1}

	st.buf += "line one\n"
	st.dirty = true
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}

	st.buf += "line two\n"
	st.dirty = true
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "line one\nline two\n" {
		t.Errorf("file content = %q", raw)
	}
	if string(raw) != st.buf {
		t.Errorf("file diverged from buffer")
	}
}

func TestStreamCleanFlushIsNoop(t *testing.T) {
	file := filepath.Join(t.TempDir(), "test.log")
	st := stream{flushed: -1}
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("flush of clean stream must not create the file")
	}
}

func TestStreamTrimsPastMaxLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "test.log")
	st := stream{flushed: -1}

	st.buf = strings.Repeat("x\n", maxLines+500)
	st.dirty = true
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	gotLines := len(strings.Split(string(raw), "\n"))
	if gotLines != trimLines {
		t.Errorf("trimmed file has %d lines, want %d", gotLines, trimLines)
	}
	if string(raw) != st.buf {
		t.Error("buffer and file diverged after trim rewrite")
	}

	// Appending after a trim must remain incremental.
	st.buf += "tail\n"
	st.dirty = true
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(file)
	if !strings.HasSuffix(string(raw), "x\ntail\n") {
		t.Errorf("post-trim append broken, tail = %q", string(raw[len(raw)-20:]))
	}
}

func TestStreamFlushFailureKeepsData(t *testing.T) {
	// A directory path makes the write fail.
	dir := t.TempDir()
	st := stream{flushed: -1}
	st.buf = "data\n"
	st.dirty = true

	if err := st.flush(dir); err == nil {
		t.Fatal("expected write error")
	}
	if !st.dirty || st.flushed != -1 {
		t.Errorf("failed rewrite must stay dirty with flushed=-1, got dirty=%v flushed=%d", st.dirty, st.flushed)
	}

	// Retry against a real file succeeds with the same content.
	file := filepath.Join(dir, "ok.log")
	if err := st.flush(file); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "data\n" {
		t.Errorf("retried flush wrote %q", raw)
	}
}

func TestRegisterCrashBudget(t *testing.T) {
	w := &Watchdog{}
	now := time.Now()

	// Crashes 1..99 within the window allow restarts; the 100th does not.
	for i := 1; i < maxRestartsInWindow; i++ {
		if !w.registerCrash(now) {
			t.Fatalf("crash %d unexpectedly exhausted the budget", i)
		}
	}
	if w.registerCrash(now) {
		t.Error("crash 100 within the window must exhaust the budget")
	}

	// After a calm period the window resets.
	if !w.registerCrash(now.Add(restartWindow + time.Second)) {
		t.Error("budget must reset after the restart window elapses")
	}
}

func TestIsoNowShape(t *testing.T) {
	got := isoNow()
	// JS Date.toISOString() shape: 2026-07-02T10:12:13.456Z
	if len(got) != 24 || got[10] != 'T' || got[23] != 'Z' || got[19] != '.' {
		t.Errorf("isoNow() = %q, not ISO-8601 ms UTC", got)
	}
}

// The startup reap must not run in update mode: the pids in the shared
// config belong to the LIVE old instance during a zero-downtime update
// (shared volume + host pid namespace), not to stale leftovers. Reaping
// them kills the handshake peer — found live in the 3.8 staging rehearsal.
func TestStartupChecksSkipsReapInUpdateMode(t *testing.T) {
	newWatchdog := func(t *testing.T) (*Watchdog, *[]int) {
		t.Helper()
		cfg, err := config.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Set("server", map[string]any{"watchdog": 11111, "pid": 22222})
		cfg.Set("apps", []any{map[string]any{"pid": 33333}})
		var reaped []int
		w := New(cfg, []string{"true"})
		w.reap = func(pid int) { reaped = append(reaped, pid) }
		return w, &reaped
	}

	t.Setenv("ODAC_UPDATE_MODE", "true")
	w, reaped := newWatchdog(t)
	w.startupChecks()
	if len(*reaped) != 0 {
		t.Fatalf("update mode reaped %v, want none", *reaped)
	}

	os.Unsetenv("ODAC_UPDATE_MODE")
	w, reaped = newWatchdog(t)
	w.startupChecks()
	if len(*reaped) != 3 {
		t.Fatalf("normal mode reaped %v, want [11111 22222 33333]", *reaped)
	}
}
