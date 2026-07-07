package system

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// recorder logs every Start/Stop/Check call into a shared ordered trace.
type trace struct {
	mu     sync.Mutex
	events []string
}

func (tr *trace) add(e string) {
	tr.mu.Lock()
	tr.events = append(tr.events, e)
	tr.mu.Unlock()
}

func (tr *trace) snapshot() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.events...)
}

func (tr *trace) count(e string) int {
	n := 0
	for _, got := range tr.snapshot() {
		if got == e {
			n++
		}
	}
	return n
}

func (tr *trace) waitFor(t *testing.T, e string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for tr.count(e) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("event %q never happened; trace: %v", e, tr.snapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type fakeService struct {
	name string
	tr   *trace
}

func (f *fakeService) Start() { f.tr.add(f.name + ".start") }
func (f *fakeService) Stop()  { f.tr.add(f.name + ".stop") }
func (f *fakeService) Check() { f.tr.add(f.name + ".check") }

func newTestSystem(t *testing.T, tr *trace) (*System, Services) {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := Services{
		App:   &fakeService{"app", tr},
		SSL:   &fakeService{"ssl", tr},
		Proxy: &fakeService{"proxy", tr},
		DNS:   &fakeService{"dns", tr},
		Mail:  &fakeService{"mail", tr},
		Api:   &fakeService{"api", tr},
		Hub:   &fakeService{"hub", tr},
	}
	s := New(cfg, svc, NewStartupGate(t.TempDir()))
	s.startupDelay = 10 * time.Millisecond
	s.tickDelay = 20 * time.Millisecond
	s.tickEvery = 5 * time.Millisecond
	return s, svc
}

func indexOf(events []string, e string) int {
	for i, got := range events {
		if got == e {
			return i
		}
	}
	return -1
}

func TestInitStartOrderAndTick(t *testing.T) {
	tr := &trace{}
	s, _ := newTestSystem(t, tr)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(false)

	// Immediate wave (normal startup → gate is ready synchronously).
	ev := tr.snapshot()
	for i, want := range []string{"proxy.start", "dns.start", "hub.start"} {
		if indexOf(ev, want) != i {
			t.Fatalf("immediate start wave = %v, want proxy,dns,hub first", ev)
		}
	}
	if indexOf(ev, "mail.start") != -1 || indexOf(ev, "api.start") != -1 {
		t.Fatalf("mail/api started synchronously: %v", ev)
	}

	// Delayed wave: mail then api.
	tr.waitFor(t, "api.start")
	ev = tr.snapshot()
	if mi, ai := indexOf(ev, "mail.start"), indexOf(ev, "api.start"); mi == -1 || ai != mi+1 {
		t.Fatalf("mail/api order wrong: %v", ev)
	}

	// Tick fires repeatedly with exact per-round order app,ssl,proxy,mail,hub.
	tr.waitFor(t, "hub.check")
	deadline := time.Now().Add(2 * time.Second)
	for tr.count("hub.check") < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	ev = tr.snapshot()
	first := indexOf(ev, "app.check")
	round := ev[first : first+5]
	want := []string{"app.check", "ssl.check", "proxy.check", "mail.check", "hub.check"}
	for i := range want {
		if round[i] != want[i] {
			t.Fatalf("tick round = %v, want %v", round, want)
		}
	}
	if indexOf(ev, "dns.check") != -1 {
		t.Fatalf("dns.check must never run (not in Node's tick): %v", ev)
	}
}

func TestStopSemantics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exceptWeb bool
		webStops  bool
	}{
		{"full stop", false, true},
		{"update overlap keeps web", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &trace{}
			s, _ := newTestSystem(t, tr)
			if err := s.Init(); err != nil {
				t.Fatal(err)
			}
			tr.waitFor(t, "app.check")

			s.Stop(tc.exceptWeb)
			ev := tr.snapshot()

			mi, ai, hi := indexOf(ev, "mail.stop"), indexOf(ev, "api.stop"), indexOf(ev, "hub.stop")
			if mi == -1 || ai != mi+1 || hi != ai+1 {
				t.Fatalf("stop order mail,api,hub violated: %v", ev)
			}
			gotWeb := indexOf(ev, "dns.stop") != -1 || indexOf(ev, "proxy.stop") != -1
			if gotWeb != tc.webStops {
				t.Fatalf("web stop = %v, want %v (exceptWeb=%v): %v", gotWeb, tc.webStops, tc.exceptWeb, ev)
			}
			if tc.webStops {
				di, pi := indexOf(ev, "dns.stop"), indexOf(ev, "proxy.stop")
				if di == -1 || pi != di+1 {
					t.Fatalf("web stop order dns,proxy violated: %v", ev)
				}
			}

			// Tick must be silent after Stop.
			checks := tr.count("app.check")
			time.Sleep(30 * time.Millisecond)
			if tr.count("app.check") != checks {
				t.Fatal("tick still running after Stop")
			}
		})
	}
}

func TestStopCancelsPendingStartupTimers(t *testing.T) {
	tr := &trace{}
	s, _ := newTestSystem(t, tr)
	s.startupDelay = 30 * time.Millisecond
	s.tickDelay = 30 * time.Millisecond
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.Stop(false) // stop before the delayed wave fires

	time.Sleep(60 * time.Millisecond)
	if tr.count("mail.start") != 0 || tr.count("api.start") != 0 {
		t.Fatalf("delayed startup fired after Stop: %v", tr.snapshot())
	}
	if tr.count("app.check") != 0 {
		t.Fatalf("tick started after Stop: %v", tr.snapshot())
	}
}

func TestRestartAfterStop(t *testing.T) {
	// The updater rollback path re-runs Init on a half-stopped instance.
	tr := &trace{}
	s, _ := newTestSystem(t, tr)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	tr.waitFor(t, "app.check")
	s.Stop(true)

	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(false)
	if tr.count("proxy.start") != 2 {
		t.Fatalf("proxy not restarted on second Init: %v", tr.snapshot())
	}
	checks := tr.count("app.check")
	tr.waitFor(t, "hub.check")
	deadline := time.Now().Add(2 * time.Second)
	for tr.count("app.check") <= checks {
		if time.Now().After(deadline) {
			t.Fatal("tick did not resume after re-Init")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNilServicesSkipped(t *testing.T) {
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, Services{}, NewStartupGate(t.TempDir()))
	s.startupDelay = 5 * time.Millisecond
	s.tickDelay = 5 * time.Millisecond
	s.tickEvery = 5 * time.Millisecond
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond) // ticks run against all-nil slots
	s.Stop(false)                     // must not panic either
}

func TestRecordServerInfo(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, Services{}, NewStartupGate(t.TempDir()))
	before := time.Now().UnixMilli()
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(false)

	srv := cfg.Map("server")
	if srv["pid"] != os.Getpid() {
		t.Errorf("server.pid = %v, want %d", srv["pid"], os.Getpid())
	}
	started, _ := srv["started"].(int64)
	if started < before || started > time.Now().UnixMilli() {
		t.Errorf("server.started = %v, not a current ms epoch", srv["started"])
	}
	if srv["os"] == "windows" || srv["arch"] == "amd64" {
		t.Errorf("server.os/arch use Go vocabulary: %v/%v", srv["os"], srv["arch"])
	}

	// Dirty flag set → SaveDirty persists it, and started serializes as a
	// plain integer (contract 0.6: no exponent/decimal).
	if err := cfg.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fmt.Sprintf(`"started": %d`, started)) {
		t.Errorf("server.json started not a plain integer:\n%s", raw)
	}
}

func TestNodeVocabulary(t *testing.T) {
	cases := map[[2]string][2]string{
		{"linux", "arm64"}:   {"linux", "arm64"},
		{"linux", "amd64"}:   {"linux", "x64"},
		{"windows", "amd64"}: {"win32", "x64"},
		{"darwin", "arm64"}:  {"darwin", "arm64"},
		{"linux", "386"}:     {"linux", "ia32"},
	}
	for in, want := range cases {
		if got := nodeOS(in[0]); got != want[0] {
			t.Errorf("nodeOS(%s) = %s, want %s", in[0], got, want[0])
		}
		if got := nodeArch(in[1]); got != want[1] {
			t.Errorf("nodeArch(%s) = %s, want %s", in[1], got, want[1])
		}
	}
}

func TestStartupGateSwitchLogs(t *testing.T) {
	out := &bytes.Buffer{}
	oldOut := logx.Stdout
	logx.Stdout = out
	t.Cleanup(func() { logx.Stdout = oldOut })

	t.Setenv("ODAC_LOG_NAME", ".odac-update")
	g := NewStartupGate(t.TempDir())
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "ODAC_CMD:SWITCH_LOGS\n"; got != want {
		t.Errorf("stdout = %q, want bare marker %q", got, want)
	}
}

func TestStartupGateNoSwitchLogsOnNormalName(t *testing.T) {
	out := &bytes.Buffer{}
	oldOut := logx.Stdout
	logx.Stdout = out
	t.Cleanup(func() { logx.Stdout = oldOut })

	t.Setenv("ODAC_LOG_NAME", ".odac")
	if err := NewStartupGate(t.TempDir()).Init(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SWITCH_LOGS") {
		t.Errorf("unexpected SWITCH_LOGS on normal log name: %q", out.String())
	}
}

func TestStartupGateWarnsOnExistingSocket(t *testing.T) {
	errBuf := &bytes.Buffer{}
	oldErr := logx.Stderr
	logx.Stderr = errBuf
	t.Cleanup(func() { logx.Stderr = oldErr })
	t.Setenv("ODAC_LOG_NAME", "")

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "run", "update.sock")
	if err := os.WriteFile(sock, nil, 0o666); err != nil {
		t.Fatal(err)
	}

	ready := false
	g := NewStartupGate(base)
	g.OnReady(func() { ready = true })
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Error("gate did not become ready despite socket (3.1 stub must continue as normal startup)")
	}
	if !strings.Contains(errBuf.String(), "task 3.7") {
		t.Errorf("expected 3.7 warning on stderr, got: %q", errBuf.String())
	}
	if _, err := os.Stat(sock); err != nil {
		t.Error("stub unlinked the socket — it must leave it in place")
	}
}

func TestOnReadyAfterReadyFiresImmediately(t *testing.T) {
	g := NewStartupGate(t.TempDir())
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	fired := false
	g.OnReady(func() { fired = true })
	if !fired {
		t.Error("OnReady after ready did not fire immediately")
	}
}
