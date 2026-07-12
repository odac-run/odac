package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/appmgr"
	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/docker"
	"odac/internal/jscanon"
	"odac/internal/logx"
)

func TestMain(m *testing.M) {
	logx.Stdout = io.Discard
	logx.Stderr = io.Discard
	os.Exit(m.Run())
}

const (
	testToken  = "test-token"
	testSecret = "test-secret"
)

// fakeCloud is a real WebSocket endpoint standing in for hub.odac.run.
type fakeCloud struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	headers []http.Header
	conns   []*websocket.Conn

	msgs chan []byte
}

func newFakeCloud(t *testing.T) *fakeCloud {
	fc := &fakeCloud{t: t, msgs: make(chan []byte, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.headers = append(fc.headers, r.Header.Clone())
		fc.mu.Unlock()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fc.mu.Lock()
		fc.conns = append(fc.conns, conn)
		fc.mu.Unlock()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			fc.msgs <- data
		}
	})
	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)
	return fc
}

// send pushes a signed cloud→agent message over the latest connection.
func (fc *fakeCloud) send(t *testing.T, fields jscanon.Obj) {
	t.Helper()
	raw := signMessage(t, fields)
	fc.sendRaw(t, raw)
}

func (fc *fakeCloud) sendRaw(t *testing.T, raw []byte) {
	t.Helper()
	fc.mu.Lock()
	if len(fc.conns) == 0 {
		fc.mu.Unlock()
		t.Fatal("no agent connection")
	}
	conn := fc.conns[len(fc.conns)-1]
	fc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("cloud write: %v", err)
	}
}

// signMessage assembles {…fields, signature} with a valid signature over
// the id/type/data/timestamp subset.
func signMessage(t *testing.T, fields jscanon.Obj) []byte {
	t.Helper()
	var id, data any
	var msgType string
	var timestamp int64
	for _, f := range fields {
		switch f.K {
		case "id":
			id = f.V
		case "type":
			msgType = f.V.(string)
		case "data":
			data = f.V
		case "timestamp":
			timestamp = f.V.(int64)
		}
	}
	sig, err := sign(id, msgType, data, timestamp, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	msg := append(append(jscanon.Obj{}, fields...), jscanon.Field{K: "signature", V: sig})
	raw, err := jscanon.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// next returns the next agent→cloud message parsed, verifying its signature.
func (fc *fakeCloud) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case raw := <-fc.msgs:
		if ok, reason := verifyWire(raw, testSecret, time.Now()); !ok {
			t.Fatalf("agent message failed verification (%s): %s", reason, raw)
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("agent message not JSON: %v\n%s", err, raw)
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no agent message received")
		return nil
	}
}

func (fc *fakeCloud) expectNone(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case raw := <-fc.msgs:
		t.Fatalf("unexpected agent message: %s", raw)
	case <-time.After(wait):
	}
}

// ---- service fakes ----

type fakeApp struct {
	mu    sync.Mutex
	calls []string

	listResult *api.Result

	logCB    func(applog.Entry)
	logUnsub func()
	unsubbed int
}

func (f *fakeApp) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeApp) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func ok(msg string) *api.Result { r := api.Res(true, msg); return &r }

func (f *fakeApp) Create(config any) *api.Result { f.record("create"); return ok("created") }
func (f *fakeApp) GetBuildStats(id any) *api.Result {
	f.record("buildstats:" + str(id))
	return ok("stats")
}
func (f *fakeApp) Delete(id any, purge bool) *api.Result {
	f.record("delete:" + str(id) + ":" + str(purge))
	return ok("deleted")
}
func (f *fakeApp) GetEnv(id any) *api.Result { f.record("getenv:" + str(id)); return ok("env") }
func (f *fakeApp) DeleteEnv(id any, keys []string) *api.Result {
	f.record("delenv:" + str(id) + ":" + strings.Join(keys, ","))
	return ok("ok")
}
func (f *fakeApp) LinkEnv(id any, target string) *api.Result {
	f.record("linkenv:" + str(id) + ":" + target)
	return ok("ok")
}
func (f *fakeApp) SetEnv(id any, env map[string]any) *api.Result {
	f.record("setenv:" + str(id))
	return ok("ok")
}
func (f *fakeApp) UnlinkEnv(id any, target string) *api.Result {
	f.record("unlinkenv:" + str(id) + ":" + target)
	return ok("ok")
}

func (f *fakeApp) List(detailed bool) *api.Result {
	f.record("list")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listResult != nil {
		return f.listResult
	}
	return ok("apps")
}

func (f *fakeApp) SetNetworks(id any, networks []any, payloadOK bool) *api.Result {
	f.record("networks:" + str(id))
	return ok("ok")
}
func (f *fakeApp) SetPorts(id any, ports []any, payloadOK bool) *api.Result {
	f.record("ports:" + str(id))
	return ok("ok")
}
func (f *fakeApp) SetVolumes(id any, volumes []any, payloadOK bool) *api.Result {
	f.record("volumes:" + str(id))
	return ok("ok")
}
func (f *fakeApp) Redeploy(payload appmgr.RedeployPayload) *api.Result {
	f.record("redeploy:" + payload.Container + ":" + payload.Branch)
	return ok("ok")
}
func (f *fakeApp) Restart(id any) *api.Result { f.record("restart:" + str(id)); return ok("ok") }

func (f *fakeApp) SubscribeToLogs(appName string, cb func(applog.Entry)) func() {
	f.record("sub:" + appName)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logUnsub == nil {
		return nil
	}
	f.logCB = cb
	return func() {
		f.mu.Lock()
		f.unsubbed++
		f.mu.Unlock()
		f.logUnsub()
	}
}

type fakeDNS struct {
	mu      sync.Mutex
	records []map[string]any
	deletes []map[string]any
}

func (f *fakeDNS) Record(args ...map[string]any) {
	f.mu.Lock()
	f.records = append(f.records, args...)
	f.mu.Unlock()
}

func (f *fakeDNS) Delete(args ...map[string]any) {
	f.mu.Lock()
	f.deletes = append(f.deletes, args...)
	f.mu.Unlock()
}

func (f *fakeDNS) List(domainArg any) api.Result {
	return api.Res(true, json.RawMessage(`{"zones":1}`))
}

type fakeDomain struct{}

func (fakeDomain) Add(domainArg, appID any) api.Result {
	return api.Res(true, "domain "+str(domainArg)+" added to "+str(appID))
}
func (fakeDomain) Delete(domainArg any, skipSync bool) api.Result {
	return api.Res(true, "domain "+str(domainArg)+" deleted")
}
func (fakeDomain) List(appIDArg any) api.Result {
	return api.Res(true, json.RawMessage(`[]`))
}

type fakeProxy struct {
	mu      sync.Mutex
	tunnels [][]dataplane.Tunnel
}

func (f *fakeProxy) SetTunnels(tunnels []dataplane.Tunnel) int {
	f.mu.Lock()
	f.tunnels = append(f.tunnels, tunnels)
	f.mu.Unlock()
	return len(tunnels)
}

type fakeContainer struct {
	mu           sync.Mutex
	stats        map[string]*docker.Stats
	buildCB      func(applog.Entry)
	buildUnsub   func()
	lastBuildLog string

	terminal    TerminalExec
	terminalErr error
	termCalls   []docker.TerminalOptions
}

func (f *fakeContainer) GetStats(name string, nowMs int64) *docker.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats[name]
}

func (f *fakeContainer) SubscribeToBuildLogs(appName string, cb func(applog.Entry)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buildUnsub == nil {
		return nil
	}
	f.buildCB = cb
	return f.buildUnsub
}

func (f *fakeContainer) GetLastBuildLog(appName string) string { return f.lastBuildLog }

func (f *fakeContainer) CreateTerminalSession(appName string, opts docker.TerminalOptions) (TerminalExec, error) {
	f.mu.Lock()
	f.termCalls = append(f.termCalls, opts)
	f.mu.Unlock()
	if f.terminalErr != nil {
		return nil, f.terminalErr
	}
	return f.terminal, nil
}

// ---- fixture ----

type hubFixture struct {
	t     *testing.T
	h     *Hub
	cloud *fakeCloud
	cfg   *config.Store

	app    *fakeApp
	dns    *fakeDNS
	proxy  *fakeProxy
	cont   *fakeContainer
	dials  chan string
	noApp  bool
	noAuth bool
}

func newHubFixture(t *testing.T, opts ...func(*hubFixture)) *hubFixture {
	t.Helper()
	fx := &hubFixture{
		t:     t,
		cloud: newFakeCloud(t),
		app:   &fakeApp{},
		dns:   &fakeDNS{},
		proxy: &fakeProxy{},
		cont:  &fakeContainer{},
		dials: make(chan string, 16),
	}
	for _, opt := range opts {
		opt(fx)
	}

	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx.cfg = cfg
	if !fx.noAuth {
		cfg.Set("hub", map[string]any{"token": testToken, "secret": testSecret})
	}

	deps := Deps{
		DNS:       fx.dns,
		Domain:    fakeDomain{},
		Proxy:     fx.proxy,
		Container: fx.cont,
		SysInfo: func() jscanon.Obj {
			return jscanon.Obj{{K: "arch", V: "arm64"}, {K: "version", V: "1.10.1"}}
		},
	}
	if !fx.noApp {
		deps.App = fx.app
	}

	fx.h = New(cfg, fx.cloud.srv.URL, deps)
	// Instrument the dial seam to count attempts while keeping the real
	// dialer.
	realDial := fx.h.ws.dial
	fx.h.ws.dial = func(url, token string) (*websocket.Conn, error) {
		fx.dials <- token
		return realDial(url, token)
	}
	t.Cleanup(func() {
		fx.h.Stop()
		fx.h.bg.Wait()
	})
	return fx
}

// connect starts the Hub, drives one Check to dial, and drains the initial
// task pushes (returning their types in order). With no App service the
// app.list push errors out and never sends, so only three arrive.
func (fx *hubFixture) connect() []string {
	fx.t.Helper()
	fx.h.Start()
	fx.h.Check()
	expected := 4
	if fx.noApp {
		expected = 3
	}
	var types []string
	for i := 0; i < expected; i++ {
		msg := fx.cloud.next(fx.t)
		types = append(types, str(msg["type"]))
	}
	// The initial triggers reset only their own timers; park every task's
	// lastRun so a later Check doesn't fire zero-lastRun tasks mid-test
	// (jest did the same in beforeEach).
	fx.h.mu.Lock()
	for _, c := range fx.h.commands {
		if c.interval > 0 {
			c.lastRun = fx.h.now()
		}
	}
	fx.h.mu.Unlock()
	return types
}

func (fx *hubFixture) waitIdle() {
	fx.t.Helper()
	fx.h.bg.Wait()
}

// ---- tests ----

func TestCheckWithoutConfigDoesNothing(t *testing.T) {
	fx := newHubFixture(t, func(f *hubFixture) { f.noAuth = true })
	fx.h.Start()
	fx.h.Check()
	select {
	case <-fx.dials:
		t.Fatal("dialed without a hub token")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCheckInactiveDoesNothing(t *testing.T) {
	fx := newHubFixture(t)
	fx.h.Check() // Start() never called
	select {
	case <-fx.dials:
		t.Fatal("dialed while inactive")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestConnectSendsInitialTasksAndBearer(t *testing.T) {
	fx := newHubFixture(t)
	types := fx.connect()

	want := []string{"system.info", "app.list", "dns.list", "domain.list"}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("initial pushes = %v, want %v", types, want)
		}
	}

	fx.cloud.mu.Lock()
	header := fx.cloud.headers[0].Get("Authorization")
	fx.cloud.mu.Unlock()
	if header != "Bearer "+testToken {
		t.Errorf("Authorization = %q", header)
	}
}

func TestSecondCheckDoesNotRedial(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	<-fx.dials // consume the first dial
	fx.h.Check()
	fx.cloud.expectNone(t, 150*time.Millisecond) // no task due, no messages
	select {
	case <-fx.dials:
		t.Fatal("re-dialed while connected")
	default:
	}
}

func TestTaskIntervals(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	// Not due: freshly reset lastRun.
	fx.h.mu.Lock()
	for _, c := range fx.h.commands {
		if c.interval > 0 {
			c.lastRun = fx.h.now()
		}
	}
	fx.h.mu.Unlock()
	fx.h.Check()
	fx.waitIdle()
	fx.cloud.expectNone(t, 150*time.Millisecond)

	// Due: app.stats interval elapsed.
	fx.h.mu.Lock()
	fx.h.commands["app.stats"].lastRun = fx.h.now().Add(-61 * time.Second)
	fx.h.mu.Unlock()
	fx.h.Check()
	msg := fx.cloud.next(t)
	if msg["type"] != "app.stats" {
		t.Fatalf("expected app.stats push, got %v", msg["type"])
	}
}

func TestCommandDispatchResponseAndTrigger(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "req-7"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{
			"action":  "dns.add",
			"payload": map[string]any{"name": "x.test", "type": "A", "value": "1.2.3.4"},
		}},
		{K: "timestamp", V: time.Now().Unix()},
	})

	resp := fx.cloud.next(t)
	if resp["type"] != "command.response" {
		t.Fatalf("expected command.response, got %v", resp["type"])
	}
	data, _ := resp["data"].(map[string]any)
	if data["id"] != "req-7" || data["success"] != true || data["message"] != "DNS record added" {
		t.Fatalf("bad response data: %v", data)
	}

	// dns.add triggers dns.list.
	next := fx.cloud.next(t)
	if next["type"] != "dns.list" {
		t.Fatalf("expected dns.list trigger, got %v", next["type"])
	}

	fx.dns.mu.Lock()
	records := fx.dns.records
	fx.dns.mu.Unlock()
	if len(records) != 1 || records[0]["name"] != "x.test" {
		t.Fatalf("DNS.Record calls = %v", records)
	}
}

func TestCommandWithoutRequestIDSendsNoResponse(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.restart", "payload": map[string]any{"container": "web"}}},
		{K: "timestamp", V: time.Now().Unix()},
	})

	// app.restart has triggers app.list + app.stats — those DO fire.
	first := fx.cloud.next(t)
	if first["type"] != "app.list" {
		t.Fatalf("expected app.list trigger, got %v", first["type"])
	}
	second := fx.cloud.next(t)
	if second["type"] != "app.stats" {
		t.Fatalf("expected app.stats trigger, got %v", second["type"])
	}
	dispatched := false
	for _, call := range fx.app.called() {
		if call == "restart:web" {
			dispatched = true
		}
	}
	if !dispatched {
		t.Fatalf("restart not dispatched: %v", fx.app.called())
	}
}

func TestUnknownCommandIgnored(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "req-9"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "bogus.action"}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	fx.cloud.expectNone(t, 200*time.Millisecond)
}

func TestCommandErrorAnswersFailure(t *testing.T) {
	fx := newHubFixture(t, func(f *hubFixture) { f.noApp = true })
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "req-3"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.create", "payload": map[string]any{}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	resp := fx.cloud.next(t)
	data, _ := resp["data"].(map[string]any)
	if data["success"] != false || data["message"] != "App service is not available" {
		t.Fatalf("bad error response: %v", data)
	}
}

func TestBadSignatureDropped(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	raw := signMessage(t, jscanon.Obj{
		{K: "id", V: "req-1"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "dns.add", "payload": map[string]any{}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	fx.cloud.sendRaw(t, tamper(raw))
	fx.cloud.expectNone(t, 200*time.Millisecond)

	fx.dns.mu.Lock()
	count := len(fx.dns.records)
	fx.dns.mu.Unlock()
	if count != 0 {
		t.Fatal("tampered command was executed")
	}
}

func TestDisconnectTokenInvalidClearsConfig(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "type", V: "disconnect"},
		{K: "reason", V: "token_invalid"},
		{K: "data", V: nil},
		{K: "timestamp", V: time.Now().Unix()},
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fx.cfg.Get("hub") == nil && !fx.h.ws.connected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hub config not cleared or socket alive: cfg=%v connected=%v",
		fx.cfg.Get("hub"), fx.h.ws.connected())
}

func TestDisconnectOtherReasonKeepsConfig(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "type", V: "disconnect"},
		{K: "reason", V: "maintenance"},
		{K: "data", V: nil},
		{K: "timestamp", V: time.Now().Unix()},
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !fx.h.ws.connected() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fx.h.ws.connected() {
		t.Fatal("socket still open after disconnect")
	}
	if fx.cfg.Get("hub") == nil {
		t.Fatal("hub config cleared for a non-credential reason")
	}
	// The reconnect window is armed: 5s + rand(0..15s) in the future.
	if fx.h.ws.shouldReconnect() {
		t.Fatal("reconnect window not armed after close")
	}
}

func TestConfigure(t *testing.T) {
	fx := newHubFixture(t)

	fx.h.handleConfigure(map[string]any{"intervals": map[string]any{
		"app.stats": float64(120),
		"dns.add":   float64(5), // not a task: ignored
		"app.list":  nil,        // falsy: disabled
	}})

	fx.h.mu.Lock()
	stats := fx.h.commands["app.stats"].interval
	list := fx.h.commands["app.list"].interval
	listConfigurable := fx.h.commands["app.list"].hasInterval
	dnsAdd := fx.h.commands["dns.add"].interval
	fx.h.mu.Unlock()

	if stats != 120*time.Second {
		t.Errorf("app.stats interval = %v", stats)
	}
	if list != 0 || !listConfigurable {
		t.Errorf("app.list interval = %v configurable=%v", list, listConfigurable)
	}
	if dnsAdd != 0 {
		t.Errorf("dns.add gained an interval: %v", dnsAdd)
	}

	// Invalid payloads are logged and ignored.
	if _, err := fx.h.handleConfigure(nil); err != nil {
		t.Errorf("configure(nil) errored: %v", err)
	}
	if _, err := fx.h.handleConfigure(map[string]any{"intervals": "nope"}); err != nil {
		t.Errorf("configure(bad) errored: %v", err)
	}
}

func TestGetAppStats(t *testing.T) {
	fx := newHubFixture(t)
	listRes := api.Res(true, []any{
		map[string]any{"name": "web", "status": "running"},
		map[string]any{"name": "db", "status": "stopped"},
		map[string]any{"name": "ghost", "status": "running"}, // no stats → omitted
	})
	fx.app.listResult = &listRes
	webStats := &docker.Stats{CPUPercent: 12.5, Pids: 3}
	fx.cont.stats = map[string]*docker.Stats{"web": webStats}

	result, err := fx.h.getAppStats()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := toCanon(result)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jscanon.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"result":true,"message":null,"data":{"web":{"cpu_percent":12.5,` +
		`"memory":{"usage":0,"limit":0,"percent":0},"network":{"rx_bytes":0,"tx_bytes":0},` +
		`"pids":3,"timestamp":0}}}`
	if string(raw) != want {
		t.Errorf("app.stats payload:\n got %s\nwant %s", raw, want)
	}
}

func TestProxyTunnelCommand(t *testing.T) {
	fx := newHubFixture(t)
	fn := fx.h.commands["proxy.tunnel"].fn

	res, err := fn(map[string]any{"tunnels": []any{
		map[string]any{"domain": "a.test", "token": "t1", "container": "web"},
		map[string]any{"domain": "", "token": "t2", "container": "db"}, // falsy domain skipped
		"garbage",
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(api.Result)
	if !r.Status {
		t.Fatalf("proxy.tunnel failed: %v", r.Message)
	}
	fx.proxy.mu.Lock()
	got := fx.proxy.tunnels
	fx.proxy.mu.Unlock()
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != (dataplane.Tunnel{Domain: "a.test", Token: "t1", Container: "web"}) {
		t.Fatalf("SetTunnels calls = %v", got)
	}

	res, _ = fn(map[string]any{"tunnels": "not-a-list"})
	r = res.(api.Result)
	if r.Status || r.Message != "Invalid tunnels payload" {
		t.Fatalf("invalid payload result = %+v", r)
	}
}

func TestPayloadMappings(t *testing.T) {
	fx := newHubFixture(t)
	run := func(action string, payload any) {
		t.Helper()
		if _, err := fx.h.commands[action].fn(payload); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}

	run("app.delete", map[string]any{"id": "web"})                   // purge defaults true
	run("app.delete", map[string]any{"id": "web", "purge": false})   // explicit false
	run("app.env.get", map[string]any{"id": "app-1"})                // id fallback
	run("app.env.get", map[string]any{"name": "web", "id": "app-1"}) // name wins
	run("app.build_stats", map[string]any{"container": "c1"})        // name||container||id
	run("app.env.delete", map[string]any{"name": "web", "keys": []any{"A", "B"}})
	run("app.redeploy", map[string]any{"container": "web", "branch": "dev"})
	run("app.restart", map[string]any{"container": "web"})

	want := []string{
		"delete:web:true",
		"delete:web:false",
		"getenv:app-1",
		"getenv:web",
		"buildstats:c1",
		"delenv:web:A,B",
		"redeploy:web:dev",
		"restart:web",
	}
	got := fx.app.called()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestLogSubscriptionBatching(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	unsubCalled := make(chan struct{}, 1)
	fx.app.logUnsub = func() { unsubCalled <- struct{}{} }

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "sub-1"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.logs.on", "payload": map[string]any{"app": "web"}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	resp := fx.cloud.next(t)
	data, _ := resp["data"].(map[string]any)
	if data["success"] != true || data["message"] != "Subscribed to logs" {
		t.Fatalf("subscribe response: %v", data)
	}

	// 50 entries flush immediately as one batch.
	fx.app.mu.Lock()
	cb := fx.app.logCB
	fx.app.mu.Unlock()
	for i := 0; i < 50; i++ {
		cb(applog.Entry{T: "runtime", D: "line", TS: int64(i)})
	}
	batchMsg := fx.cloud.next(t)
	if batchMsg["type"] != "log.stream" {
		t.Fatalf("expected log.stream, got %v", batchMsg["type"])
	}
	payload, _ := batchMsg["data"].(map[string]any)
	batch, _ := payload["batch"].([]any)
	if payload["app"] != "web" || len(batch) != 50 {
		t.Fatalf("batch shape: app=%v len=%d", payload["app"], len(batch))
	}

	// A partial buffer flushes on the 500ms timer.
	cb(applog.Entry{T: "runtime", D: "tail", TS: 99})
	timed := fx.cloud.next(t)
	payload, _ = timed["data"].(map[string]any)
	if batch, _ := payload["batch"].([]any); len(batch) != 1 {
		t.Fatalf("timed batch len = %d", len(batch))
	}

	// Duplicate subscribe answers Already subscribed.
	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "sub-2"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.logs.on", "payload": map[string]any{"app": "web"}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	resp = fx.cloud.next(t)
	data, _ = resp["data"].(map[string]any)
	if data["message"] != "Already subscribed" {
		t.Fatalf("duplicate subscribe: %v", data)
	}

	// Unsubscribe flushes leftovers and releases.
	cb(applog.Entry{T: "runtime", D: "leftover", TS: 100})
	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "sub-3"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.logs.off", "payload": map[string]any{"app": "web"}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	leftover := fx.cloud.next(t)
	if leftover["type"] != "log.stream" {
		t.Fatalf("expected leftover flush, got %v", leftover["type"])
	}
	resp = fx.cloud.next(t) // the command.response ({result:true} fallback)
	data, _ = resp["data"].(map[string]any)
	if data["success"] != true {
		t.Fatalf("logs.off response: %v", data)
	}
	select {
	case <-unsubCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("unsubscribe never called")
	}
}

func TestLogSubscriptionUnavailable(t *testing.T) {
	fx := newHubFixture(t)
	// fx.app.logUnsub nil → SubscribeToLogs returns nil.
	res, err := fx.h.logsOn(map[string]any{"app": "web"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["success"] != false || m["message"] != "App not running or logs unavailable" {
		t.Fatalf("logsOn = %v", m)
	}
}

func TestBuildLogsReplay(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()
	fx.cont.lastBuildLog = "last build content"

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "bl-1"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.build_logs.on", "payload": map[string]any{"app": "web"}}},
		{K: "timestamp", V: time.Now().Unix()},
	})

	replay := fx.cloud.next(t)
	if replay["type"] != "build.log" {
		t.Fatalf("expected build.log, got %v", replay["type"])
	}
	payload, _ := replay["data"].(map[string]any)
	if payload["content"] != "last build content" || payload["finished"] != true {
		t.Fatalf("replay payload: %v", payload)
	}

	resp := fx.cloud.next(t)
	data, _ := resp["data"].(map[string]any)
	if data["message"] != "Sent last build log" {
		t.Fatalf("build_logs.on response: %v", data)
	}
}

func TestDisconnectCleansSubscriptions(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	released := make(chan struct{}, 1)
	fx.app.logUnsub = func() { released <- struct{}{} }
	if res, _ := fx.h.logsOn(map[string]any{"app": "web"}); res.(map[string]any)["success"] != true {
		t.Fatal("subscribe failed")
	}

	// The cloud drops the link; onDisconnect must clear subscriptions.
	fx.cloud.mu.Lock()
	fx.cloud.conns[0].CloseNow()
	fx.cloud.mu.Unlock()

	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription survived the disconnect")
	}
}

func TestStopDisconnects(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.h.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !fx.h.ws.connected() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fx.h.ws.connected() {
		t.Fatal("socket alive after Stop")
	}
	// Inactive: Check must not redial even after the reconnect window.
	for len(fx.dials) > 0 {
		<-fx.dials // drain the original connect
	}
	fx.h.ws.mu.Lock()
	fx.h.ws.nextReconnect = time.Time{}
	fx.h.ws.mu.Unlock()
	fx.h.Check()
	select {
	case <-fx.dials:
		t.Fatal("re-dialed after Stop")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestCommandTableOrder(t *testing.T) {
	fx := newHubFixture(t)
	want := []string{
		"configure", "app.create", "app.build_stats", "app.delete", "app.env.get",
		"app.env.delete", "app.env.link", "app.env.set", "app.env.unlink", "app.list",
		"app.network.set", "app.port.set", "app.redeploy", "app.restart", "app.stats",
		"app.volumes.set", "dns.add", "dns.delete", "dns.list", "domain.add",
		"domain.delete", "domain.list", "proxy.tunnel", "system.info",
		"app.logs.on", "app.logs.off", "app.build_logs.on", "app.build_logs.off",
		"terminal.open", "terminal.close", "system.update",
	}
	if len(fx.h.order) != len(want) {
		t.Fatalf("command table has %d entries, want %d\n%v", len(fx.h.order), len(want), fx.h.order)
	}
	for i, name := range want {
		if fx.h.order[i] != name {
			t.Errorf("order[%d] = %s, want %s", i, fx.h.order[i], name)
		}
		if fx.h.commands[name] == nil {
			t.Errorf("command %s missing", name)
		}
	}
	// Task intervals per the contract table.
	intervals := map[string]time.Duration{
		"app.list": 30 * time.Minute, "app.stats": 60 * time.Second,
		"dns.list": 60 * time.Minute, "domain.list": 30 * time.Minute,
		"system.info": 60 * time.Minute,
	}
	for name, want := range intervals {
		if got := fx.h.commands[name].interval; got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
}

func TestSystemUpdateStub(t *testing.T) {
	fx := newHubFixture(t)
	if _, err := fx.h.commands["system.update"].fn(nil); err == nil {
		t.Fatal("system.update should fail until task 3.7")
	}
}

func TestNormalizeResultShapes(t *testing.T) {
	// nil → {result:true} fallback → success true, nothing else.
	r := normalizeResult(nil)
	if r.success != true || r.hasMessage || r.hasData {
		t.Errorf("nil: %+v", r)
	}

	// Api.result envelope: success from .result, null message included.
	res := api.Res(true, map[string]any{"a": 1})
	r = normalizeResult(res)
	if r.success != true || !r.hasMessage || r.message != nil || !r.hasData {
		t.Errorf("api.Result: %+v", r)
	}

	// Plain map with explicit success.
	r = normalizeResult(map[string]any{"success": false, "message": "nope"})
	if r.success != false || r.message != "nope" || r.hasData {
		t.Errorf("map: %+v", r)
	}
}
