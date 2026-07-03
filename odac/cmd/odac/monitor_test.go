package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"odac/internal/apiproto"
)

// testMonitor builds a monitor with fixed size, no real docker, and a fixed
// clock, on top of the standard testApp fixture.
func testMonitor(t *testing.T, mode string) (*monitor, *app) {
	t.Helper()
	a, out, _ := testApp(t, "127.0.0.1:0")
	m := newMonitor(a, mode, out)
	m.size = func() (int, int) { return 83, 24 } // width becomes 80
	m.docker = func(args ...string) (string, string, error) {
		return "", "", errors.New("docker disabled in tests")
	}
	m.now = func() time.Time { return time.Unix(1767225600, 0) }
	m.width, m.height = 80, 24
	return m, a
}

func TestMonCol1(t *testing.T) {
	tests := []struct{ width, want int }{
		{80, 20},  // floor(80/12*3)
		{81, 20},  // floor(20.25)
		{300, 50}, // capped
		{201, 50}, // floor(50.25) == cap boundary
		{199, 49}, // floor(49.75)
	}
	for _, tt := range tests {
		if got := monCol1(tt.width); got != tt.want {
			t.Errorf("monCol1(%d) = %d, want %d", tt.width, got, tt.want)
		}
	}
}

func TestMcolor(t *testing.T) {
	if got := mcolor("x", "blue", "white", "bold"); got != "\x1b[1m\x1b[47m\x1b[34mx\x1b[0m\x1b[0m\x1b[0m" {
		t.Errorf("fg+bg+bold nesting = %q", got)
	}
	if got := mcolor("x", "cyan"); got != "x" {
		t.Errorf("unknown color must be a no-op, got %q", got)
	}
	if got := mcolor("x", ""); got != "x" {
		t.Errorf("empty color must be a no-op, got %q", got)
	}
}

func TestMicon(t *testing.T) {
	if got := micon("running", true); !strings.Contains(got, "\x1b[47m") {
		t.Errorf("selected icon must have white background: %q", got)
	}
	if got := micon("running", false); strings.Contains(got, "\x1b[47m") {
		t.Errorf("unselected icon must not have background: %q", got)
	}
	if got := micon("", false); got != "   " {
		t.Errorf("unknown status = %q, want three spaces", got)
	}
}

func TestMspacing(t *testing.T) {
	if got := mspacing("ab", 5, ""); got != "ab   " {
		t.Errorf("left pad = %q", got)
	}
	if got := mspacing("ab", 5, "right"); got != "   ab" {
		t.Errorf("right pad = %q", got)
	}
	colored := mcolor("ab", "red")
	if got := mspacing(colored, 5, ""); got != colored+"   " {
		t.Errorf("ANSI-aware pad = %q", got)
	}
	if got := mspacing("abcdef", 3, ""); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := mspacing("abcdef", 3, "right"); got != "abcdef" {
		t.Errorf("negative right pad must clamp: %q", got)
	}
}

func TestMsafeLog(t *testing.T) {
	// plain padding
	if got := msafeLog("ab", 4); got != "ab  " {
		t.Errorf("pad = %q", got)
	}
	// empty
	if got := msafeLog("", 3); got != "   " {
		t.Errorf("empty = %q", got)
	}
	// truncation appends reset and keeps escapes intact
	in := mcolor("hello", "red") + " world"
	got := msafeLog(in, 7)
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("truncated line must end with reset: %q", got)
	}
	if !strings.Contains(got, "\x1b[31mhello\x1b[0m") {
		t.Errorf("escapes must survive truncation: %q", got)
	}
	if n := visibleLen(got); n != 7 {
		t.Errorf("visible length = %d, want 7 (%q)", n, got)
	}
	// tabs become two spaces, \r dropped
	if got := msafeLog("a\tb\r", 6); got != "a  b  " {
		t.Errorf("tab/cr handling = %q", got)
	}
}

func TestConvertProxyLog(t *testing.T) {
	raw := "2026/07/03 10:00:01.123456 [INFO] started\n" +
		"2026/07/03 10:00:02 [ERROR] boom\n" +
		"no timestamp line"
	got := strings.Split(convertProxyLog(raw), "\n")
	want := []string{
		"[LOG][2026-07-03T10:00:01][proxy] [INFO] started",
		"[ERR][2026-07-03T10:00:02][proxy] [ERROR] boom",
		"[LOG][proxy] no timestamp line",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("convertProxyLog =\n%v\nwant\n%v", got, want)
	}
}

func TestFilterModuleLines(t *testing.T) {
	log := "[LOG][2026-07-03T10:00:00.000Z] [api] listening\n" +
		"[ERR][2026-07-03T10:00:01.000Z] [dns] lookup failed\n" +
		"[LOG][2026-07-03T10:00:02.000Z] [mail] queued\n" +
		"unrelated line\n"

	all := filterModuleLines(log, []string{"api", "dns", "mail"}, 100)
	if len(all) != 3 {
		t.Fatalf("want 3 module lines, got %d: %v", len(all), all)
	}
	if !strings.Contains(all[0], "[api]") || !strings.Contains(all[0], "listening") {
		t.Errorf("api line = %q", all[0])
	}
	if !strings.Contains(all[1], "\x1b[31m") {
		t.Errorf("[ERR] date must be red: %q", all[1])
	}
	if !strings.Contains(all[0], "\x1b[32m") {
		t.Errorf("[LOG] date must be green: %q", all[0])
	}

	only := filterModuleLines(log, []string{"dns"}, 100)
	if len(only) != 1 || !strings.Contains(only[0], "[dns]") {
		t.Errorf("dns filter = %v", only)
	}

	tail := filterModuleLines(log, []string{"api", "dns", "mail"}, 2)
	if len(tail) != 2 || !strings.Contains(tail[1], "[mail]") {
		t.Errorf("tail slice = %v", tail)
	}
}

func TestFormatModuleLineProxyDate(t *testing.T) {
	line := "[ERR][2026-07-03T10:00:02][proxy] [ERROR] boom"
	got := formatModuleLine(line, "proxy")
	if !strings.Contains(got, "[2026-07-03 10:00:02]") {
		t.Errorf("19-char proxy date must format cleanly: %q", got)
	}
	if !strings.Contains(got, "[ERROR] boom") {
		t.Errorf("message lost: %q", got)
	}
}

func TestLoadModuleLogsFromDisk(t *testing.T) {
	m, a := testMonitor(t, "debug")
	logsDir := filepath.Join(a.cfg.BaseDir(), "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	odacLog := "[LOG][2026-07-03T10:00:00.000Z] [api] up\n" +
		"[LOG][2026-07-03T10:00:01.000Z] [dns] answering\n"
	if err := os.WriteFile(filepath.Join(logsDir, ".odac.log"), []byte(odacLog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "proxy.log"),
		[]byte("2026/07/03 10:00:02 [INFO] routed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.loadModuleLogs()
	if len(m.logsContent) != 3 {
		t.Fatalf("want odac+proxy merge (3 lines), got %d: %v", len(m.logsContent), m.logsContent)
	}
	if !strings.Contains(m.logsContent[2], "[proxy]") {
		t.Errorf("proxy line missing: %v", m.logsContent)
	}

	// watching only dns (index 4) drops api and proxy lines
	m.watch = []int{4}
	m.loadModuleLogs()
	if len(m.logsContent) != 1 || !strings.Contains(m.logsContent[0], "[dns]") {
		t.Errorf("dns watch = %v", m.logsContent)
	}
}

func TestParseDockerLogs(t *testing.T) {
	stdout := "2026-07-03T10:00:02.000000000Z second line\n" +
		"2026-07-03T10:00:01.000000000Z first line\n"
	stderr := "2026-07-03T10:00:03.000000000Z error: kaput\n"
	got := parseDockerLogs(stdout, stderr, nil, 10)
	if len(got) != 3 {
		t.Fatalf("want 3 lines, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "first line") || !strings.Contains(got[2], "kaput") {
		t.Errorf("timestamp sort failed: %v", got)
	}
	if !strings.Contains(got[2], "\x1b[31m") {
		t.Errorf("'error' content must color the date red: %q", got[2])
	}
	if !strings.Contains(got[0], "\x1b[32m") {
		t.Errorf("normal content must color the date green: %q", got[0])
	}

	failed := parseDockerLogs("", "", errors.New("no such container"), 10)
	if len(failed) != 1 || !strings.Contains(failed[0], "Error fetching logs: no such container") {
		t.Errorf("error path = %v", failed)
	}

	tail := parseDockerLogs(stdout, stderr, nil, 1)
	if len(tail) != 1 || !strings.Contains(tail[0], "kaput") {
		t.Errorf("tail slice = %v", tail)
	}
}

func TestParseStats(t *testing.T) {
	stdout := "blog|0.35%|15.25MiB / 2GiB\nqueue|12.00%|1.5GiB / 2GiB\n"
	stats, maxCPU, maxMem := parseStats(stdout)
	if got := stats["blog"]; got != (monStat{cpu: "0%", mem: "15MB"}) {
		t.Errorf("blog = %+v", got)
	}
	if got := stats["queue"]; got != (monStat{cpu: "12%", mem: "1GB"}) {
		t.Errorf("queue = %+v", got)
	}
	if maxCPU != 3 || maxMem != 4 {
		t.Errorf("max lens = %d/%d, want 3/4", maxCPU, maxMem)
	}
}

func TestParseStatuses(t *testing.T) {
	stdout := "blog|running\nqueue|exited\nbad|dead\nspin|restarting\nnap|paused\n"
	got := parseStatuses(stdout)
	want := map[string]string{
		"blog": "running", "queue": "stopped", "bad": "errored",
		"spin": "progress", "nap": "stopped",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

func TestRefreshAppsPartition(t *testing.T) {
	m, a := testMonitor(t, "monit")
	a.cfg.Set("apps", []any{
		map[string]any{"id": "3", "name": "zeta"},
		map[string]any{"id": "1", "name": "blog"},
		map[string]any{"id": "2", "name": "queue"},
	})
	a.cfg.Set("domains", map[string]any{
		"example.com": map[string]any{"appId": "1"},
		"z.com":       map[string]any{"appId": "3"},
	})

	m.refreshApps()
	names := make([]string, len(m.apps))
	for i, app := range m.apps {
		names[i] = app.name
	}
	// public (blog, zeta) sorted, then internal (queue)
	if !reflect.DeepEqual(names, []string{"blog", "zeta", "queue"}) {
		t.Errorf("apps = %v", names)
	}
	if m.publicCount != 2 {
		t.Errorf("publicCount = %d, want 2", m.publicCount)
	}
}

func TestDebugFrameLayout(t *testing.T) {
	m, _ := testMonitor(t, "debug")
	m.selected = 1
	m.watch = []int{2}
	m.logsContent = []string{"log line one"}

	frame := m.debugFrame()
	for _, want := range []string{"Modules", "Logs", "api", "updater", "[X] ", "log line one",
		"┌", "┬", "┐", "└", "┴", "┘", " ODAC", "Ctrl+C Exit"} {
		if !strings.Contains(frame, want) {
			t.Errorf("debug frame missing %q", want)
		}
	}
	// selected row (app, index 1) is highlighted blue-on-white
	if !strings.Contains(frame, "\x1b[47m\x1b[34m") {
		t.Errorf("selected module must render blue on white:\n%s", frame)
	}
	// every line inside the box has consistent visible width
	lines := strings.Split(frame, "\n")
	if n := visibleLen(lines[0]); n != m.width+3 {
		t.Errorf("top border visible width = %d, want %d", n, m.width+3)
	}
	for i := 1; i < m.height-2; i++ {
		if n := visibleLen(lines[i]); n != m.width+3 {
			t.Errorf("row %d visible width = %d, want %d: %q", i, n, m.width+3, lines[i])
		}
	}
}

func TestMonitFrameLayout(t *testing.T) {
	m, a := testMonitor(t, "monit")
	a.cfg.Set("apps", []any{
		map[string]any{"id": "1", "name": "blog"},
		map[string]any{"id": "2", "name": "queue"},
	})
	a.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "1"}})
	m.refreshApps()
	m.stats["blog"] = monStat{cpu: "0%", mem: "15MB"}
	m.maxCPULen, m.maxMemLen = 2, 4
	m.statuses["blog"] = "running"
	m.logsContent = []string{"container says hi"}

	frame := m.monitFrame()
	for _, want := range []string{"Public", "Internal", "blog", "queue",
		"[15MB| 0%]", "container says hi", "▶"} {
		if !strings.Contains(frame, want) {
			t.Errorf("monit frame missing %q", want)
		}
	}
	// row 0 → app 0 (blog), row 1 is the Internal header, row 2 → app 1
	if m.lineToApp[0] != 0 || m.lineToApp[2] != 1 {
		t.Errorf("lineToApp = %v", m.lineToApp)
	}
	if _, ok := m.lineToApp[1]; ok {
		t.Errorf("group header line must not map to an app: %v", m.lineToApp)
	}
	lines := strings.Split(frame, "\n")
	for i := 0; i < m.height-2; i++ {
		if n := visibleLen(lines[i]); n != m.width+3 {
			t.Errorf("row %d visible width = %d, want %d: %q", i, n, m.width+3, lines[i])
		}
	}
}

func TestMonitFrameTitleAllPublic(t *testing.T) {
	m, a := testMonitor(t, "monit")
	a.cfg.Set("apps", []any{map[string]any{"id": "1", "name": "blog"}})
	a.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "1"}})
	m.refreshApps()
	frame := m.monitFrame()
	if !strings.Contains(frame, "Apps") || strings.Contains(frame, "Internal") {
		t.Errorf("all-public frame must title 'Apps' with no group header:\n%s", frame)
	}
}

func TestHandleInputNavigation(t *testing.T) {
	m, _ := testMonitor(t, "debug")
	post := func(f func()) { f() }

	if m.handleInput([]byte{27, '[', 'B'}, post); m.selected != 1 {
		t.Errorf("down arrow: selected = %d", m.selected)
	}
	if m.handleInput([]byte{27, '[', 'A'}, post); m.selected != 0 {
		t.Errorf("up arrow: selected = %d", m.selected)
	}
	if m.handleInput([]byte{27, '[', 'A'}, post); m.selected != 0 {
		t.Errorf("up at top must clamp: selected = %d", m.selected)
	}
	// wheel down twice, batched in one chunk
	if m.handleInput([]byte{27, '[', 'M', 97, 33, 33, 27, '[', 'M', 97, 33, 33}, post); m.selected != 2 {
		t.Errorf("batched wheel down: selected = %d", m.selected)
	}
	// Enter toggles watch on and off
	m.handleInput([]byte{13}, post)
	if !reflect.DeepEqual(m.watch, []int{2}) {
		t.Errorf("watch after enter = %v", m.watch)
	}
	m.handleInput([]byte{13}, post)
	if len(m.watch) != 0 {
		t.Errorf("watch after second enter = %v", m.watch)
	}
	// Ctrl+C quits
	if !m.handleInput([]byte{3}, post) {
		t.Error("Ctrl+C must return quit")
	}
}

func TestMouseClickSelectsModule(t *testing.T) {
	m, _ := testMonitor(t, "debug")
	// click at x=3, y=4 → module index 2 (config): X10 coords are value+32
	m.handleInput([]byte{27, '[', 'M', 32, 32 + 3, 32 + 4}, func(f func()) { f() })
	if m.selected != 2 || !reflect.DeepEqual(m.watch, []int{2}) {
		t.Errorf("click: selected = %d, watch = %v", m.selected, m.watch)
	}
}

func TestMouseClickSelectsApp(t *testing.T) {
	m, a := testMonitor(t, "monit")
	a.cfg.Set("apps", []any{
		map[string]any{"id": "1", "name": "blog"},
		map[string]any{"id": "2", "name": "queue"},
	})
	m.refreshApps()
	m.monitFrame() // builds lineToApp
	// click row y=3 → rendered line 1 → app index 1
	m.handleInput([]byte{27, '[', 'M', 32, 32 + 3, 32 + 3}, func(f func()) { f() })
	if m.selected != 1 {
		t.Errorf("click: selected = %d, want 1", m.selected)
	}
}

func TestRestartSelected(t *testing.T) {
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		if req.Action != "app.restart" || !reflect.DeepEqual(req.Data, []any{"blog"}) {
			conn.Write([]byte(`{"id":"r","result":false,"message":"bad request"}`))
			return
		}
		conn.Write([]byte(`{"id":"r","result":true,"message":"ok"}`))
	})
	m, a := testMonitor(t, "monit")
	a.client = &apiproto.Client{Addr: addr}
	a.cfg.Set("apps", []any{map[string]any{"id": "1", "name": "blog"}})
	m.refreshApps()

	applied := make(chan func(), 4)
	m.restartSelected(func(f func()) { applied <- f })

	if !strings.Contains(m.restarting["blog"], "Restarting blog...") {
		t.Errorf("pending marker = %q", m.restarting["blog"])
	}
	select {
	case f := <-applied:
		f()
	case <-time.After(3 * time.Second):
		t.Fatal("restart result never posted")
	}
	if !strings.Contains(m.restarting["blog"], "Successfully restarted blog") {
		t.Errorf("result marker = %q", m.restarting["blog"])
	}
	// the marker replaces the log pane while present
	m.logsContent = nil
	m.loadMonitLogs(func(f func()) { f() })
	if len(m.logsContent) != 1 || !strings.Contains(m.logsContent[0], "Successfully restarted") {
		t.Errorf("restart marker must fill the log pane: %v", m.logsContent)
	}
}

func TestLoadMonitLogsThrottle(t *testing.T) {
	m, a := testMonitor(t, "monit")
	a.cfg.Set("apps", []any{map[string]any{"id": "1", "name": "blog"}})
	m.refreshApps()

	calls := 0
	m.docker = func(args ...string) (string, string, error) {
		calls++
		if args[0] != "logs" || args[len(args)-1] != "blog" {
			t.Errorf("docker args = %v", args)
		}
		return "2026-07-03T10:00:00.000000000Z hi\n", "", nil
	}

	applied := make(chan func(), 4)
	post := func(f func()) { applied <- f }

	m.loadMonitLogs(post)
	select {
	case f := <-applied:
		f()
	case <-time.After(3 * time.Second):
		t.Fatal("docker logs never posted")
	}
	if calls != 1 || len(m.logsContent) != 1 {
		t.Fatalf("calls = %d, content = %v", calls, m.logsContent)
	}

	// same selection within 1s: throttled, no new exec
	m.loadMonitLogs(post)
	if calls != 1 {
		t.Errorf("throttle failed, calls = %d", calls)
	}

	// clock advances past 1s: fetches again
	m.now = func() time.Time { return time.Unix(1767225602, 0) }
	m.loadMonitLogs(post)
	select {
	case f := <-applied:
		f()
	case <-time.After(3 * time.Second):
		t.Fatal("second fetch never posted")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRenderOnlyFlushesChangedFrames(t *testing.T) {
	m, a := testMonitor(t, "debug")
	var buf strings.Builder
	m.out = &buf
	_ = a

	post := func(f func()) { f() }
	m.render(post)
	first := buf.Len()
	if first == 0 {
		t.Fatal("first render must flush")
	}
	if !strings.Contains(buf.String(), "\r\n") {
		t.Error("raw-mode output must use CRLF")
	}
	m.render(post)
	if buf.Len() != first {
		t.Error("identical frame must not re-flush")
	}
}

func TestMonitorRequiresTTY(t *testing.T) {
	a, _, errOut := testApp(t, "127.0.0.1:0")
	if code := a.monitor("monit"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "interactive terminal") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestMonitorRunQuitsOnCtrlC(t *testing.T) {
	m, _ := testMonitor(t, "debug")
	var buf strings.Builder
	m.out = &buf

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte{3}) // Ctrl+C
		w.Close()
	}()

	done := make(chan int, 1)
	go func() { done <- m.run(r) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run() did not quit on Ctrl+C")
	}
	if buf.Len() == 0 {
		t.Error("run() must render at least one frame")
	}
}

func TestDetailValueSmoke(t *testing.T) {
	// guard against monitor helpers shadowing ui.go behavior
	if got := detailValue(nil); got != "-" {
		t.Errorf("detailValue(nil) = %q", got)
	}
	if got := fmt.Sprint(micon("running", false)); !strings.Contains(got, "▶") {
		t.Errorf("micon = %q", got)
	}
}
