package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"odac/internal/config"
	"odac/internal/docker"
)

// fakeExec is the Terminal returned by Container.createTerminalSession
// (jest's createTerminalStub).
type fakeExec struct {
	mu      sync.Mutex
	closed  bool
	reasons []string
	writes  [][]byte
	resizes [][2]int
}

func (f *fakeExec) Write(data []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	return true
}

func (f *fakeExec) Resize(cols, rows int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	return true
}

func (f *fakeExec) Close(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.reasons = append(f.reasons, reason)
}

func (f *fakeExec) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeExec) closeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reasons...)
}

// termFrame is one frame received by the fake cloud terminal endpoint.
type termFrame struct {
	binary bool
	data   []byte
}

// termEndpoint is one accepted per-session socket on the cloud side.
type termEndpoint struct {
	path   string
	header http.Header
	conn   *websocket.Conn
	frames chan termFrame
}

// termCloud serves /ws/terminal/<id> and records sessions.
type termCloud struct {
	srv   *httptest.Server
	conns chan *termEndpoint
}

func newTermCloud(t *testing.T) *termCloud {
	tc := &termCloud{conns: make(chan *termEndpoint, 8)}
	tc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/ws/terminal/") {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ep := &termEndpoint{path: r.URL.Path, header: r.Header.Clone(), conn: conn, frames: make(chan termFrame, 64)}
		tc.conns <- ep
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				close(ep.frames)
				return
			}
			ep.frames <- termFrame{binary: typ == websocket.MessageBinary, data: data}
		}
	}))
	t.Cleanup(tc.srv.Close)
	return tc
}

func (tc *termCloud) accept(t *testing.T) *termEndpoint {
	t.Helper()
	select {
	case ep := <-tc.conns:
		return ep
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal socket dialed")
		return nil
	}
}

func (ep *termEndpoint) next(t *testing.T) termFrame {
	t.Helper()
	select {
	case fr, ok := <-ep.frames:
		if !ok {
			t.Fatal("terminal socket closed while awaiting a frame")
		}
		return fr
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal frame received")
		return termFrame{}
	}
}

func (ep *termEndpoint) send(t *testing.T, binary bool, data []byte) {
	t.Helper()
	typ := websocket.MessageText
	if binary {
		typ = websocket.MessageBinary
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ep.conn.Write(ctx, typ, data); err != nil {
		t.Fatalf("cloud terminal write: %v", err)
	}
}

// termFixture wires a terminalManager against the fakes.
type termFixture struct {
	t     *testing.T
	mgr   *terminalManager
	cloud *termCloud
	cont  *fakeContainer
	exec  *fakeExec
	cfg   *config.Store
	dials int
	token string
}

func newTermFixture(t *testing.T, terminalCfg map[string]any) *termFixture {
	t.Helper()
	fx := &termFixture{t: t, cloud: newTermCloud(t), cont: &fakeContainer{}, token: "tok"}
	fx.exec = &fakeExec{}
	fx.cont.terminal = fx.exec

	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx.cfg = cfg
	hubCfg := map[string]any{"token": "tok", "secret": "sec"}
	if terminalCfg != nil {
		hubCfg["terminal"] = terminalCfg
	}
	cfg.Set("hub", hubCfg)

	wsURL := "ws" + strings.TrimPrefix(fx.cloud.srv.URL, "http") + "/ws"
	fx.mgr = newTerminalManager(wsURL, func() string { return fx.token }, cfg, fx.cont)
	realDial := fx.mgr.dial
	fx.mgr.dial = func(url, token, ticket string) (*websocket.Conn, error) {
		fx.dials++
		return realDial(url, token, ticket)
	}
	t.Cleanup(fx.mgr.closeAll)
	return fx
}

func termPayload(overrides map[string]any) map[string]any {
	p := map[string]any{
		"app": "web", "sessionId": "sess-abcdef12", "ticket": "ticket-abcdef12",
		"cols": float64(100), "rows": float64(30),
	}
	for k, v := range overrides {
		p[k] = v
	}
	return p
}

func openSession(t *testing.T, fx *termFixture) (map[string]any, *termEndpoint) {
	t.Helper()
	res, err := fx.mgr.open(termPayload(nil))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["success"] != true {
		t.Fatalf("open failed: %v", m)
	}
	return m, fx.cloud.accept(t)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

// ---- guards ----

func TestTerminalEnabledByDefault(t *testing.T) {
	fx := newTermFixture(t, map[string]any{}) // empty config still opens
	res, _ := openSession(t, fx)
	if res["message"] != "Terminal session opened" {
		t.Fatalf("open = %v", res)
	}
	if len(fx.cont.termCalls) != 1 {
		t.Fatal("exec not created")
	}
}

func TestTerminalDisabled(t *testing.T) {
	fx := newTermFixture(t, map[string]any{"enabled": false})
	res, _ := fx.mgr.open(termPayload(nil))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Terminal access is disabled" {
		t.Fatalf("open = %v", m)
	}
	if len(fx.cont.termCalls) != 0 {
		t.Fatal("exec created while disabled")
	}
}

func TestTerminalRejectsMalformedIDs(t *testing.T) {
	for _, override := range []map[string]any{
		{"sessionId": "short"},
		{"sessionId": "../../etc"},
		{"ticket": "nope"},
		{"sessionId": nil},
	} {
		fx := newTermFixture(t, nil)
		res, _ := fx.mgr.open(termPayload(override))
		m := res.(map[string]any)
		if m["success"] != false || m["message"] != "Invalid session id or ticket" {
			t.Fatalf("%v: open = %v", override, m)
		}
		if fx.dials != 0 || len(fx.cont.termCalls) != 0 {
			t.Fatalf("%v: dialed or exec'd before validation", override)
		}
	}
}

func TestTerminalMissingApp(t *testing.T) {
	fx := newTermFixture(t, nil)
	res, _ := fx.mgr.open(termPayload(map[string]any{"app": nil}))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Missing app" {
		t.Fatalf("open = %v", m)
	}
}

func TestTerminalSessionCap(t *testing.T) {
	fx := newTermFixture(t, map[string]any{"enabled": true, "maxSessions": float64(1)})
	openSession(t, fx)

	res, _ := fx.mgr.open(termPayload(map[string]any{"sessionId": "sess-99999999"}))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Too many terminal sessions (max 1)" {
		t.Fatalf("second open = %v", m)
	}
	if fx.mgr.count() != 1 {
		t.Fatalf("count = %d", fx.mgr.count())
	}
}

func TestTerminalDuplicateSession(t *testing.T) {
	fx := newTermFixture(t, nil)
	openSession(t, fx)
	res, _ := fx.mgr.open(termPayload(nil))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Session already open" {
		t.Fatalf("duplicate open = %v", m)
	}
}

func TestTerminalRequiresToken(t *testing.T) {
	fx := newTermFixture(t, nil)
	fx.token = ""
	res, _ := fx.mgr.open(termPayload(nil))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Not authenticated" {
		t.Fatalf("open = %v", m)
	}
}

func TestTerminalRejectedAppFreesSlot(t *testing.T) {
	fx := newTermFixture(t, nil)
	fx.cont.terminalErr = errors.New("Unknown app: odac")

	res, _ := fx.mgr.open(termPayload(map[string]any{"app": "odac"}))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Unknown app: odac" {
		t.Fatalf("open = %v", m)
	}
	if fx.mgr.count() != 0 {
		t.Fatal("slot not freed")
	}
	// The exec is validated before the Hub is dialed: no socket opened.
	if fx.dials != 0 {
		t.Fatal("dialed despite exec failure")
	}
}

func TestTerminalDialFailureReapsExec(t *testing.T) {
	fx := newTermFixture(t, nil)
	fx.mgr.dial = func(url, token, ticket string) (*websocket.Conn, error) {
		return nil, errors.New("handshake failed")
	}

	res, _ := fx.mgr.open(termPayload(nil))
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "handshake failed" {
		t.Fatalf("open = %v", m)
	}
	if got := fx.exec.closeCalls(); len(got) != 1 || got[0] != "error" {
		t.Fatalf("exec close calls = %v", got)
	}
	if fx.mgr.count() != 0 {
		t.Fatal("slot not freed")
	}
}

// ---- success ----

func TestTerminalDialsPerSessionEndpoint(t *testing.T) {
	fx := newTermFixture(t, nil)
	res, ep := openSession(t, fx)

	data, _ := res["data"].(map[string]any)
	if data["sessionId"] != "sess-abcdef12" || data["app"] != "web" {
		t.Fatalf("open data = %v", data)
	}
	if ep.path != "/ws/terminal/sess-abcdef12" {
		t.Errorf("path = %s", ep.path)
	}
	if ep.header.Get("Authorization") != "Bearer tok" || ep.header.Get("X-Odac-Ticket") != "ticket-abcdef12" {
		t.Errorf("headers = %v", ep.header)
	}
}

func TestTerminalPassesSettingsToExec(t *testing.T) {
	fx := newTermFixture(t, map[string]any{
		"enabled": true, "idleTimeout": float64(1000), "maxLifetime": float64(2000),
		"allowPrivileged": true,
	})
	openSession(t, fx)

	opts := fx.cont.termCalls[0]
	if opts.Cols != 100 || opts.Rows != 30 {
		t.Errorf("size = %dx%d", opts.Cols, opts.Rows)
	}
	if *opts.IdleTimeout != time.Second || *opts.MaxLifetime != 2*time.Second {
		t.Errorf("limits = %v/%v", *opts.IdleTimeout, *opts.MaxLifetime)
	}
	if !opts.AllowPrivileged {
		t.Error("allowPrivileged not threaded through")
	}
}

func TestTerminalPrivilegedDefaultsOff(t *testing.T) {
	fx := newTermFixture(t, nil)
	openSession(t, fx)
	if fx.cont.termCalls[0].AllowPrivileged {
		t.Error("privileged allowed by default")
	}
}

// ---- framing ----

func TestTerminalFraming(t *testing.T) {
	fx := newTermFixture(t, nil)
	_, ep := openSession(t, fx)
	opts := fx.cont.termCalls[0]

	// pty output goes out as binary frames.
	opts.OnData([]byte("hello"))
	fr := ep.next(t)
	if !fr.binary || string(fr.data) != "hello" {
		t.Fatalf("frame = %+v", fr)
	}

	// binary frames are written straight to the pty.
	ep.send(t, true, []byte("ls\n"))
	waitFor(t, "pty write", func() bool {
		fx.exec.mu.Lock()
		defer fx.exec.mu.Unlock()
		return len(fx.exec.writes) == 1 && string(fx.exec.writes[0]) == "ls\n"
	})

	// a text resize frame resizes the pty.
	ep.send(t, false, []byte(`{"type":"resize","cols":120,"rows":40}`))
	waitFor(t, "resize", func() bool {
		fx.exec.mu.Lock()
		defer fx.exec.mu.Unlock()
		return len(fx.exec.resizes) == 1 && fx.exec.resizes[0] == [2]int{120, 40}
	})

	// a malformed control frame is ignored, not fatal.
	ep.send(t, false, []byte(`{not json`))
	time.Sleep(50 * time.Millisecond)
	if fx.mgr.count() != 1 {
		t.Fatal("malformed control frame killed the session")
	}

	// a text close frame ends the session.
	ep.send(t, false, []byte(`{"type":"close"}`))
	waitFor(t, "remote close", func() bool { return fx.mgr.count() == 0 })
	if got := fx.exec.closeCalls(); len(got) != 1 || got[0] != "remote" {
		t.Fatalf("close calls = %v", got)
	}
}

// ---- teardown ----

func TestTerminalExitTellsHubThenHangsUp(t *testing.T) {
	fx := newTermFixture(t, nil)
	_, ep := openSession(t, fx)
	opts := fx.cont.termCalls[0]

	fx.exec.Close("exited") // shell already gone; session must not re-close it
	opts.OnExit(docker.ExitInfo{Reason: "exited", ExitCode: 7})

	fr := ep.next(t)
	if fr.binary {
		t.Fatal("exit control went out as binary")
	}
	var control map[string]any
	if err := json.Unmarshal(fr.data, &control); err != nil {
		t.Fatal(err)
	}
	if control["type"] != "exit" || control["reason"] != "exited" || control["exitCode"] != float64(7) {
		t.Fatalf("exit control = %s", fr.data)
	}
	waitFor(t, "session removal", func() bool { return fx.mgr.count() == 0 })
}

func TestTerminalSocketDropReapsExec(t *testing.T) {
	fx := newTermFixture(t, nil)
	_, ep := openSession(t, fx)

	ep.conn.CloseNow()
	waitFor(t, "socket close", func() bool { return fx.mgr.count() == 0 })
	if got := fx.exec.closeCalls(); len(got) != 1 || got[0] != "socket" {
		t.Fatalf("close calls = %v", got)
	}
}

func TestTerminalOverflowCloses(t *testing.T) {
	old := maxBufferedBytes
	maxBufferedBytes = 8
	t.Cleanup(func() { maxBufferedBytes = old })

	fx := newTermFixture(t, nil)
	_, _ = openSession(t, fx)
	opts := fx.cont.termCalls[0]

	opts.OnData([]byte("way more than eight bytes"))
	waitFor(t, "overflow close", func() bool { return fx.mgr.count() == 0 })
	if got := fx.exec.closeCalls(); len(got) != 1 || got[0] != "overflow" {
		t.Fatalf("close calls = %v", got)
	}
}

func TestTerminalCloseCommand(t *testing.T) {
	fx := newTermFixture(t, nil)
	openSession(t, fx)

	res, _ := fx.mgr.close(map[string]any{"sessionId": "sess-abcdef12"})
	m := res.(map[string]any)
	if m["success"] != true || m["message"] != "Terminal session closed" {
		t.Fatalf("close = %v", m)
	}
	if got := fx.exec.closeCalls(); len(got) != 1 || got[0] != "command" {
		t.Fatalf("close calls = %v", got)
	}
	if fx.mgr.count() != 0 {
		t.Fatal("session not removed")
	}
}

func TestTerminalCloseUnknownSession(t *testing.T) {
	fx := newTermFixture(t, nil)
	res, _ := fx.mgr.close(map[string]any{"sessionId": "sess-00000000"})
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "Unknown session" {
		t.Fatalf("close = %v", m)
	}
}

func TestTerminalCloseAll(t *testing.T) {
	fx := newTermFixture(t, map[string]any{"enabled": true, "maxSessions": float64(5)})
	first := fx.exec
	openSession(t, fx)

	second := &fakeExec{}
	fx.cont.terminal = second
	res, err := fx.mgr.open(termPayload(map[string]any{"sessionId": "sess-22222222"}))
	if err != nil || res.(map[string]any)["success"] != true {
		t.Fatalf("second open = %v %v", res, err)
	}
	fx.cloud.accept(t)

	if fx.mgr.count() != 2 {
		t.Fatalf("count = %d", fx.mgr.count())
	}
	fx.mgr.closeAll()

	for _, exec := range []*fakeExec{first, second} {
		if got := exec.closeCalls(); len(got) != 1 || got[0] != "hub_disconnected" {
			t.Fatalf("close calls = %v", got)
		}
	}
	if fx.mgr.count() != 0 {
		t.Fatal("sessions not cleared")
	}
}

func TestTerminalDoubleCloseFiresOnce(t *testing.T) {
	fx := newTermFixture(t, nil)
	_, ep := openSession(t, fx)

	fx.mgr.close(map[string]any{"sessionId": "sess-abcdef12"})
	ep.conn.CloseNow() // the socket dropping afterwards must not re-close
	time.Sleep(100 * time.Millisecond)

	if got := fx.exec.closeCalls(); len(got) != 1 {
		t.Fatalf("close calls = %v", got)
	}
}

// TestTerminalManagerSettingsMerge pins the DEFAULTS ⊕ config.hub.terminal
// merge.
func TestTerminalManagerSettingsMerge(t *testing.T) {
	fx := newTermFixture(t, map[string]any{"maxSessions": float64(7)})
	s := fx.mgr.settings()
	if s["maxSessions"] != float64(7) {
		t.Errorf("maxSessions = %v", s["maxSessions"])
	}
	if s["enabled"] != true || s["allowPrivileged"] != false {
		t.Errorf("defaults not merged: %v", s)
	}
	if s["idleTimeout"] != float64(15*60*1000) || s["maxLifetime"] != float64(4*60*60*1000) {
		t.Errorf("timeout defaults: %v", s)
	}
}
