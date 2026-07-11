package applog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLogger(t *testing.T) *Logger {
	t.Helper()
	l := New(t.TempDir(), "myapp")
	if err := l.Init(); err != nil {
		t.Fatal(err)
	}
	return l
}

// --- Build streams ---

func TestBuildStreamSummary(t *testing.T) {
	l := newTestLogger(t)
	b, err := l.NewBuildStream("build_1000_abc", map[string]any{"image": "img", "strategy": "git-app"})
	if err != nil {
		t.Fatal(err)
	}

	b.StartPhase("git_clone")
	b.Write([]byte("Cloning...\n"))
	b.EndPhase("git_clone", true)
	b.StartPhase("compile")
	b.Write([]byte("compiling\n"))
	b.EndPhase("compile", true)
	if err := b.Finalize(true); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(l.buildsDir, "build_1000_abc.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum buildSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.ID != "build_1000_abc" || sum.Status != "success" {
		t.Errorf("summary id/status = %q/%q", sum.ID, sum.Status)
	}
	if len(sum.Phases) != 2 || sum.Phases[0].Name != "git_clone" || sum.Phases[0].Status != "success" {
		t.Errorf("phases = %+v", sum.Phases)
	}
	if sum.Metadata["strategy"] != "git-app" {
		t.Errorf("metadata = %v", sum.Metadata)
	}

	// Raw bytes must pass through to the .log file.
	logRaw, _ := os.ReadFile(b.Path())
	if string(logRaw) != "Cloning...\ncompiling\n" {
		t.Errorf("log file = %q", logRaw)
	}
}

func TestBuildStreamErrorWarningHeuristics(t *testing.T) {
	l := newTestLogger(t)
	b, _ := l.NewBuildStream("build_1", nil)
	b.StartPhase("compile")

	var entries []Entry
	unsub := b.Subscribe(func(e Entry) { entries = append(entries, e) })
	defer unsub()

	b.Write([]byte("Error: something broke\n"))        // error
	b.Write([]byte("error in node_modules/x\n"))       // excluded: node_modules
	b.Write([]byte("Warning: deprecated\n"))           // warning
	b.Write([]byte("npm warn old package\n"))          // hmm: no "warning" substring, plain line
	b.Write([]byte("npm WARN warning: legacy peer\n")) // excluded: npm warn
	b.Write([]byte("   \n"))                           // empty, skipped
	b.Write([]byte("|\n/\n-\n\\\n"))                   // spinner lines, skipped
	b.Write([]byte("progress\rreal line\n"))           // \r stripped
	b.Finalize(false)

	raw, _ := os.ReadFile(filepath.Join(l.buildsDir, "build_1.json"))
	var sum buildSummary
	json.Unmarshal(raw, &sum)
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1", sum.Errors)
	}
	if sum.Warnings != 1 {
		t.Errorf("warnings = %d, want 1", sum.Warnings)
	}
	if sum.Status != "failed" {
		t.Errorf("status = %q", sum.Status)
	}
	if sum.Phases[0].Errors != 1 || sum.Phases[0].Warnings != 1 {
		t.Errorf("phase counters = %d/%d, want 1/1", sum.Phases[0].Errors, sum.Phases[0].Warnings)
	}
	if sum.Phases[0].Status != "failed" { // auto-closed by Finalize(false)
		t.Errorf("phase status = %q", sum.Phases[0].Status)
	}

	// The error line must broadcast as type "err"; spinner/empty lines are absent.
	var sawErr bool
	for _, e := range entries {
		if e.T == "err" && strings.Contains(e.D, "something broke") {
			sawErr = true
		}
		if strings.TrimSpace(e.D) == "|" {
			t.Error("spinner line reached subscribers")
		}
	}
	if !sawErr {
		t.Error("error line not broadcast as err")
	}
}

func TestBuildStreamPartialLinesAndFlush(t *testing.T) {
	l := newTestLogger(t)
	b, _ := l.NewBuildStream("build_2", nil)

	var lines []string
	b.Subscribe(func(e Entry) { lines = append(lines, e.D) })

	b.Write([]byte("half"))
	b.Write([]byte(" line\nnext "))
	if err := b.Finalize(true); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "half line") {
		t.Errorf("split line not assembled: %q", joined)
	}
	if !strings.Contains(joined, "next") {
		t.Errorf("leftover not flushed on finalize: %q", joined)
	}
}

func TestBuildStreamLongLineForceSplit(t *testing.T) {
	l := newTestLogger(t)
	b, _ := l.NewBuildStream("build_3", nil)

	var warn string
	b.Subscribe(func(e Entry) {
		if strings.HasPrefix(e.D, "[Warning] Long line truncated:") {
			warn = e.D
		}
	})

	b.Write([]byte(strings.Repeat("x", maxLineLength+10)))
	if warn == "" {
		t.Fatal("expected long-line truncation warning")
	}
	if !strings.HasSuffix(warn, "...") {
		t.Errorf("warning = %q", warn[:80])
	}
	b.Finalize(true)
}

func TestBuildStreamClearsPreviousBuildBuffer(t *testing.T) {
	l := newTestLogger(t)
	b1, _ := l.NewBuildStream("build_4", nil)
	b1.Write([]byte("old build line\n"))
	b1.Finalize(true)

	_, _ = l.NewBuildStream("build_5", nil)
	var replay []string
	l.Subscribe(func(e Entry) { replay = append(replay, e.D) }, Build)

	for _, d := range replay {
		if strings.Contains(d, "old build line") {
			t.Error("previous build's lines replayed to new subscriber")
		}
	}
	if len(replay) != 1 || !strings.Contains(replay[0], "Build session build_5 initialized") {
		t.Errorf("replay = %v, want only the init line", replay)
	}
}

func TestBuildPruneKeepsTen(t *testing.T) {
	l := newTestLogger(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("build_%03d", i)
		b, _ := l.NewBuildStream(id, nil)
		b.Write([]byte("x\n"))
		b.Finalize(true)
		// Distinct mtimes, oldest first.
		ts := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(filepath.Join(l.buildsDir, id+".json"), ts, ts)
		os.Chtimes(filepath.Join(l.buildsDir, id+".log"), ts, ts)
	}
	l.pruneBuilds()

	names, _ := os.ReadDir(l.buildsDir)
	var jsons, logs int
	for _, de := range names {
		if strings.HasSuffix(de.Name(), ".json") {
			jsons++
		}
		if strings.HasSuffix(de.Name(), ".log") {
			logs++
		}
	}
	if jsons != 10 || logs != 10 {
		t.Errorf("after prune: %d json / %d log, want 10/10", jsons, logs)
	}
	if _, err := os.Stat(filepath.Join(l.buildsDir, "build_000.json")); !os.IsNotExist(err) {
		t.Error("oldest build should be pruned")
	}
	if _, err := os.Stat(filepath.Join(l.buildsDir, "build_012.json")); err != nil {
		t.Error("newest build should survive")
	}
}

func TestGetLastBuildSortsPhases(t *testing.T) {
	l := newTestLogger(t)
	sum := buildSummary{
		ID: "build_9", Timestamp: 1000, Status: "success",
		Phases: []*Phase{
			{Name: "running_late", Start: 50, Status: "running"},
			{Name: "second", Start: 10, End: 30, Status: "success"},
			{Name: "first", Start: 5, End: 20, Status: "success"},
			{Name: "running_early", Start: 40, Status: "running"},
		},
		Metadata: map[string]any{},
	}
	raw, _ := json.Marshal(sum)
	os.WriteFile(filepath.Join(l.buildsDir, "build_9.json"), raw, 0o644)

	info := l.GetLastBuild()
	if info == nil {
		t.Fatal("expected build info")
	}
	order := []string{}
	for _, p := range info.Phases {
		order = append(order, p.Name)
	}
	want := "first,second,running_early,running_late"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("phase order = %s, want %s", got, want)
	}
}

func TestGetLastBuildPicksNewestByFilename(t *testing.T) {
	l := newTestLogger(t)
	for _, id := range []string{"build_1000_a", "build_2000_b"} {
		raw, _ := json.Marshal(buildSummary{ID: id, Timestamp: 1, Status: "success"})
		os.WriteFile(filepath.Join(l.buildsDir, id+".json"), raw, 0o644)
	}
	if info := l.GetLastBuild(); info == nil || info.ID != "build_2000_b" {
		t.Errorf("last build = %+v, want build_2000_b", info)
	}
}

func TestGetDailySummary(t *testing.T) {
	l := newTestLogger(t)
	now := time.Now().UnixMilli()
	write := func(id string, ts int64, status string, dur float64) {
		raw, _ := json.Marshal(buildSummary{ID: id, Timestamp: ts, Status: status, Duration: dur})
		os.WriteFile(filepath.Join(l.buildsDir, id+".json"), raw, 0o644)
	}
	write("build_a", now-1000, "success", 10)
	write("build_b", now-2000, "failed", 5)
	write("build_old", now-25*60*60*1000, "success", 99) // >24h, excluded
	os.WriteFile(filepath.Join(l.buildsDir, "build_bad.json"), []byte("{corrupt"), 0o644)

	s := l.GetDailySummary()
	if s.Total != 2 || s.Success != 1 || s.Failed != 1 {
		t.Errorf("summary = %+v", s)
	}
	if s.AvgDuration != 7.5 {
		t.Errorf("avg = %v, want 7.5", s.AvgDuration)
	}
	if len(s.Builds) != 2 || s.Builds[0].ID != "build_a" {
		t.Errorf("builds order = %+v (want newest first)", s.Builds)
	}
}

func TestReadLastBuildLogNewestByMtime(t *testing.T) {
	l := newTestLogger(t)
	if got := l.ReadLastBuildLog(); got != "" {
		t.Errorf("no logs should read empty, got %q", got)
	}
	old := filepath.Join(l.buildsDir, "aaa.log")
	os.WriteFile(old, []byte("old"), 0o644)
	oldTime := time.Now().Add(-time.Hour)
	os.Chtimes(old, oldTime, oldTime)
	os.WriteFile(filepath.Join(l.buildsDir, "zzz.log"), []byte("newest"), 0o644)
	// Name sorts after but mtime decides — set it older than "aaa".
	if got := l.ReadLastBuildLog(); got != "newest" {
		t.Errorf("last build log = %q, want newest", got)
	}
}

// --- Runtime streams ---

func TestRuntimeStreamWritesDailyFile(t *testing.T) {
	l := newTestLogger(t)
	r, err := l.NewRuntimeStream()
	if err != nil {
		t.Fatal(err)
	}
	r.Write([]byte("hello "))
	r.Error([]byte("world\n"))
	r.End()
	r.End() // idempotent

	today := time.Now().UTC().Format("2006-01-02")
	raw, err := os.ReadFile(filepath.Join(l.runtimeDir, today+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello world\n" {
		t.Errorf("log = %q", raw)
	}

	// Writes after End are dropped, not crashes.
	r.Write([]byte("late"))
	raw, _ = os.ReadFile(filepath.Join(l.runtimeDir, today+".log"))
	if strings.Contains(string(raw), "late") {
		t.Error("write after End must be dropped")
	}
}

func TestRuntimeErrorHistogram(t *testing.T) {
	l := newTestLogger(t)
	fixed := time.Date(2026, 7, 11, 15, 4, 5, 0, time.Local)
	l.now = func() time.Time { return fixed }

	r, _ := l.NewRuntimeStream()
	r.Error([]byte("boom\n"))
	r.End()

	raw, err := os.ReadFile(filepath.Join(l.runtimeDir, "stats.json"))
	if err != nil {
		t.Fatal("stats.json not written on first error")
	}
	var stats runtimeStats
	json.Unmarshal(raw, &stats)
	if stats.Today[15] != 1 {
		t.Errorf("hour 15 flag = %d, want 1", stats.Today[15])
	}
	for h, v := range stats.Today {
		if h != 15 && v != 0 {
			t.Errorf("hour %d unexpectedly set", h)
		}
	}
}

func TestRuntimeStatsDayRoll(t *testing.T) {
	l := newTestLogger(t)
	day1 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return day1 }
	r, _ := l.NewRuntimeStream()
	r.Error([]byte("x\n"))
	r.End()

	day2 := day1.Add(24 * time.Hour)
	l.now = func() time.Time { return day2 }
	_, err := l.NewRuntimeStream()
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(filepath.Join(l.runtimeDir, "stats.json"))
	var stats runtimeStats
	json.Unmarshal(raw, &stats)
	if stats.Date != "2026-07-11" {
		t.Errorf("date = %q", stats.Date)
	}
	if stats.Yesterday[12] != 1 {
		t.Error("yesterday should carry day1's error flag")
	}
	for _, v := range stats.Today {
		if v != 0 {
			t.Error("today should be a clean slate")
		}
	}
}

func TestRuntimeSizeRotation(t *testing.T) {
	l := newTestLogger(t)
	l.maxRuntimeBytes = 64
	l.rotateCheckEvery = 1 // stat on every write

	r, _ := l.NewRuntimeStream()
	chunk := []byte(strings.Repeat("a", 40) + "\n")
	r.Write(chunk) // 41 bytes, under cap
	r.Write(chunk) // 82 > 64 → rotate on this check
	r.Write([]byte("fresh\n"))
	r.End()

	backup, err := os.ReadFile(r.Path() + ".1")
	if err != nil {
		t.Fatal("expected .1 backup after size rotation")
	}
	if len(backup) != 82 {
		t.Errorf("backup size = %d, want 82", len(backup))
	}
	fresh, err := os.ReadFile(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != "fresh\n" {
		t.Errorf("fresh file = %q", fresh)
	}
}

func TestRuntimeSubscribersSurviveEnd(t *testing.T) {
	l := newTestLogger(t)
	r1, _ := l.NewRuntimeStream()

	var got []string
	l.Subscribe(func(e Entry) { got = append(got, e.T+":"+e.D) }, Runtime)

	r1.Write([]byte("one"))
	r1.End()

	r2, _ := l.NewRuntimeStream()
	r2.Error([]byte("two"))
	r2.End()

	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "out:one") || !strings.Contains(joined, "err:two") {
		t.Errorf("subscriber missed entries across restart: %v", got)
	}
}

func TestSubscribeReplaysHistoryAndUnsubscribes(t *testing.T) {
	l := newTestLogger(t)
	r, _ := l.NewRuntimeStream()
	r.Write([]byte("early"))

	var got []string
	unsub := l.Subscribe(func(e Entry) { got = append(got, e.D) }, Runtime)
	if len(got) != 1 || got[0] != "early" {
		t.Errorf("history replay = %v", got)
	}

	unsub()
	r.Write([]byte("after"))
	if len(got) != 1 {
		t.Error("entry delivered after unsubscribe")
	}
	r.End()
}

func TestRuntimeBufferCap(t *testing.T) {
	l := newTestLogger(t)
	r, _ := l.NewRuntimeStream()
	for i := 0; i < runtimeBufferSize+20; i++ {
		r.Write([]byte(fmt.Sprintf("line%d", i)))
	}
	r.End()

	var got []string
	l.Subscribe(func(e Entry) { got = append(got, e.D) }, Runtime)
	if len(got) != runtimeBufferSize {
		t.Errorf("replayed %d entries, want %d", len(got), runtimeBufferSize)
	}
	if got[0] != "line20" {
		t.Errorf("oldest surviving entry = %q, want line20", got[0])
	}
}

func TestPruneRuntimeLogs(t *testing.T) {
	l := newTestLogger(t)
	oldFile := filepath.Join(l.runtimeDir, "2026-06-01.log")
	os.WriteFile(oldFile, []byte("x"), 0o644)
	stale := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(oldFile, stale, stale)
	freshFile := filepath.Join(l.runtimeDir, "2026-07-11.log")
	os.WriteFile(freshFile, []byte("x"), 0o644)
	keepJSON := filepath.Join(l.runtimeDir, "stats.json")
	os.WriteFile(keepJSON, []byte("{}"), 0o644)
	os.Chtimes(keepJSON, stale, stale) // non-.log files never pruned

	l.pruneRuntimeLogs()

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("8-day-old log should be pruned")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Error("fresh log should survive")
	}
	if _, err := os.Stat(keepJSON); err != nil {
		t.Error("stats.json should survive age pruning")
	}
}

// --- Health ---

func TestGetHealthSlidingWindow(t *testing.T) {
	l := newTestLogger(t)
	fixed := time.Date(2026, 7, 11, 10, 0, 0, 0, time.Local)
	l.now = func() time.Time { return fixed }
	today := fixed.UTC().Format("2006-01-02")

	yesterday := make([]int, 24)
	yesterday[23] = 1 // 23:00 yesterday — inside the window (hour+1=11 → slice [11:])
	yesterday[5] = 1  // 05:00 yesterday — outside
	todayArr := make([]int, 24)
	todayArr[9] = 1

	raw, _ := json.Marshal(runtimeStats{Date: today, Today: todayArr, Yesterday: yesterday})
	os.WriteFile(filepath.Join(l.runtimeDir, "stats.json"), raw, 0o644)

	logs := l.GetHealth()
	if len(logs) != 24 {
		t.Fatalf("window length = %d", len(logs))
	}
	// Window = yesterday[11..23] (13 entries) + today[0..10] (11 entries).
	if logs[12] != 1 { // yesterday[23]
		t.Errorf("yesterday 23:00 flag lost: %v", logs)
	}
	if logs[13+9] != 1 { // today[9]
		t.Errorf("today 09:00 flag lost: %v", logs)
	}
	sum := 0
	for _, v := range logs {
		sum += v
	}
	if sum != 2 {
		t.Errorf("window sum = %d, want 2 (05:00 must fall out)", sum)
	}
}

func TestGetHealthMissingStats(t *testing.T) {
	l := newTestLogger(t)
	logs := l.GetHealth()
	if len(logs) != 24 {
		t.Fatalf("len = %d", len(logs))
	}
	for _, v := range logs {
		if v != 0 {
			t.Fatal("expected all zeros")
		}
	}
}

func TestDestroyRemovesTree(t *testing.T) {
	l := newTestLogger(t)
	r, _ := l.NewRuntimeStream()
	r.Write([]byte("x"))
	r.End()
	if err := l.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.logsDir); !os.IsNotExist(err) {
		t.Error("logs dir should be removed")
	}
}
