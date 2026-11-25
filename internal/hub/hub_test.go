package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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

	listResult  *api.Result
	imageResult appmgr.ImageSetResult
	rollback    appmgr.RollbackResult
	redeploys   []appmgr.RedeployPayload

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

func (f *fakeApp) Create(_ api.Caller, config any) *api.Result {
	f.record("create")
	return ok("created")
}
func (f *fakeApp) GetBuildStats(id any) *api.Result {
	f.record("buildstats:" + str(id))
	return ok("stats")
}
func (f *fakeApp) Delete(_ api.Caller, id any, purge bool) *api.Result {
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
func (f *fakeApp) SetNetworkMode(id any, mode string) *api.Result {
	f.record("networkmode:" + str(id) + ":" + mode)
	return ok("ok")
}
func (f *fakeApp) SetIsolated(id any, isolated bool) *api.Result {
	state := "off"
	if isolated {
		state = "on"
	}
	f.record("isolate:" + str(id) + ":" + state)
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
	f.mu.Lock()
	f.redeploys = append(f.redeploys, payload)
	f.mu.Unlock()
	return ok("ok")
}
func (f *fakeApp) SetImage(payload appmgr.ImageSetPayload) appmgr.ImageSetResult {
	f.record("image:" + payload.Container + ":" + payload.Image + ":" + str(payload.Pull) + ":" + str(payload.RequireSnapshot))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageResult
}
func (f *fakeApp) Rollback(payload appmgr.RollbackPayload) appmgr.RollbackResult {
	f.record("rollback:" + payload.Container + ":" + payload.BuildID + ":" + str(payload.RequireSnapshot))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rollback
}
func (f *fakeApp) Restart(id any) *api.Result { f.record("restart:" + str(id)); return ok("ok") }
func (f *fakeApp) Start(id any) *api.Result   { f.record("start:" + str(id)); return ok("ok") }
func (f *fakeApp) Stop(id any) *api.Result    { f.record("stop:" + str(id)); return ok("ok") }

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

func (fakeDomain) Add(_ api.Caller, domainArg, appID any) api.Result {
	return api.Res(true, "domain "+str(domainArg)+" added to "+str(appID))
}
func (fakeDomain) Delete(_ api.Caller, domainArg any, skipSync bool) api.Result {
	return api.Res(true, "domain "+str(domainArg)+" deleted")
}
func (fakeDomain) List(appIDArg any) api.Result {
	return api.Res(true, json.RawMessage(`[]`))
}

// operator marks a call made with the operator's reach, which is the only one
// the hub may use: it acts for the server's owner, never for an app.
func operator(caller api.Caller) string {
	if caller == api.Operator {
		return "(operator) "
	}
	return "(NOT operator) "
}

type fakeMTLS struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeMTLS) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}
func (f *fakeMTLS) Issue(caller api.Caller, domainArg, optsArg any) api.Result {
	f.record("issue " + operator(caller) + str(domainArg) + " " + str(pmap(optsArg)["csr"]))
	return api.Res(true, "issued", map[string]any{"fingerprint": "aa"})
}
func (f *fakeMTLS) Import(caller api.Caller, optsArg any) api.Result {
	f.record("import " + operator(caller) + str(pmap(optsArg)["cert"]))
	return api.Res(true, "imported", map[string]any{"fingerprint": "aa"})
}
func (f *fakeMTLS) List(caller api.Caller, domainArg any) api.Result {
	f.record("list " + operator(caller) + str(domainArg))
	return api.Res(true, json.RawMessage(`[]`))
}
func (f *fakeMTLS) Revoke(caller api.Caller, fingerprintArg any) api.Result {
	f.record("revoke " + operator(caller) + str(fingerprintArg))
	return api.Res(true, "revoked")
}
func (f *fakeMTLS) Scope(fingerprintArg, hostsArg any) api.Result {
	f.record("scope " + str(fingerprintArg))
	return api.Res(true, "scoped")
}
func (f *fakeMTLS) Set(domainArg, modeArg, optsArg any) api.Result {
	f.record("set " + str(domainArg) + " " + str(modeArg) + " " + str(pmap(optsArg)["caPem"]))
	return api.Res(true, "set")
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

type fakeMail struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeMail) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeMail) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMail) Create(email, password, retype any) api.Result {
	f.record("create:" + str(email) + ":" + str(password) + ":" + str(retype))
	return api.Res(true, "created")
}
func (f *fakeMail) Delete(email any) api.Result {
	f.record("delete:" + str(email))
	return api.Res(true, "deleted")
}
func (f *fakeMail) List(domain any) api.Result {
	f.record("list:" + str(domain))
	return api.Res(true, "accounts")
}
func (f *fakeMail) ListAll() api.Result {
	f.record("listall")
	return api.Res(true, []any{map[string]any{"domain": "x.test", "email": "a@x.test"}})
}
func (f *fakeMail) Password(email, password, retype any) api.Result {
	f.record("password:" + str(email) + ":" + str(password) + ":" + str(retype))
	return api.Res(true, "changed")
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
	mail   *fakeMail
	cont   *fakeContainer
	mtls   *fakeMTLS
	backup *fakeBackup
	dials  chan string
	noApp  bool
	noMail bool
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
		mail:  &fakeMail{},
		cont:  &fakeContainer{},
		mtls:  &fakeMTLS{},
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
		MTLS:      fx.mtls,
		Proxy:     fx.proxy,
		Container: fx.cont,
		SysInfo: func() jscanon.Obj {
			return jscanon.Obj{{K: "arch", V: "arm64"}, {K: "version", V: "1.10.1"}}
		},
		SysStats: func() jscanon.Obj {
			return jscanon.Obj{
				{K: "cpu", V: 42},
				{K: "disk", V: jscanon.Obj{{K: "used", V: 1}, {K: "total", V: 2}}},
				{K: "memory", V: jscanon.Obj{{K: "used", V: 3}, {K: "total", V: 4}}},
				{K: "network", V: jscanon.Obj{{K: "download", V: 5}, {K: "upload", V: 6}}},
			}
		},
	}
	if !fx.noApp {
		deps.App = fx.app
	}
	if !fx.noMail {
		deps.Mail = fx.mail
	}
	if fx.backup != nil {
		deps.Backup = fx.backup
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
// task pushes (returning their types in order). A missing service errors its
// push out, so that one never arrives.
func (fx *hubFixture) connect() []string {
	fx.t.Helper()
	fx.h.Start()
	fx.h.Check()
	expected := 6
	if fx.noApp {
		expected--
	}
	if fx.noMail {
		expected--
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

	want := []string{"system.info", "app.list", "dns.list", "domain.list", "mail.list", "domain.mtls.cert.list"}
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

func TestUnknownCommandAnswersFailure(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	fx.cloud.send(t, jscanon.Obj{
		{K: "id", V: "req-9"},
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.image.switch", "payload": map[string]any{}}},
		{K: "timestamp", V: time.Now().Unix()},
	})

	resp := fx.cloud.next(t)
	if resp["type"] != "command.response" {
		t.Fatalf("expected command.response, got %v", resp["type"])
	}
	data, _ := resp["data"].(map[string]any)
	if data["id"] != "req-9" || data["success"] != false || data["message"] != "unknown action" {
		t.Fatalf("bad response data: %v", data)
	}
	if _, ok := data["data"]; ok {
		t.Fatalf("unknown action carried data: %v", data)
	}

	// Without a request id there is nobody to answer.
	fx.cloud.send(t, jscanon.Obj{
		{K: "type", V: "command"},
		{K: "data", V: map[string]any{"action": "app.image.switch", "payload": map[string]any{}}},
		{K: "timestamp", V: time.Now().Unix()},
	})
	fx.cloud.expectNone(t, 150*time.Millisecond)
}

func TestImageSetRepliesFlat(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	ask := func(reqID string, result appmgr.ImageSetResult) map[string]any {
		t.Helper()
		fx.app.mu.Lock()
		fx.app.imageResult = result
		fx.app.mu.Unlock()
		fx.cloud.send(t, jscanon.Obj{
			{K: "id", V: reqID},
			{K: "type", V: "command"},
			{K: "data", V: map[string]any{"action": "app.image.set", "payload": map[string]any{
				"id": "res-1", "container": "db", "image": "postgres:17.6-alpine", "pull": false,
			}}},
			{K: "timestamp", V: time.Now().Unix()},
		})
		for {
			resp := fx.cloud.next(t)
			if resp["type"] == "command.response" {
				data, _ := resp["data"].(map[string]any)
				return data
			}
		}
	}

	data := ask("req-1", appmgr.ImageSetResult{Reason: "unhealthy", RolledBack: true, Message: "Rolled back."})
	if data["id"] != "req-1" || data["success"] != false || data["reason"] != "unhealthy" ||
		data["rolledBack"] != true || data["message"] != "Rolled back." {
		t.Fatalf("failure reply = %v", data)
	}
	if _, ok := data["data"]; ok {
		t.Fatalf("a flat reply must not nest data: %v", data)
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	data = ask("req-2", appmgr.ImageSetResult{
		Success: true, Changed: true, Previous: "postgres:17.5-alpine", Image: "postgres:17.6-alpine",
		ImageID: digest, ImageDigest: digest,
	})
	want := map[string]any{
		"id": "req-2", "success": true, "changed": true, "previous": "postgres:17.5-alpine",
		"image": "postgres:17.6-alpine", "imageId": digest, "imageDigest": digest,
	}
	if len(data) != len(want) {
		t.Fatalf("success reply = %v", data)
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("%s = %v, want %v", k, data[k], v)
		}
	}

	data = ask("req-3", appmgr.ImageSetResult{Success: true})
	if len(data) != 3 || data["success"] != true || data["changed"] != false {
		t.Fatalf("unchanged reply = %v", data)
	}

	data = ask("req-4", appmgr.ImageSetResult{Reason: "snapshot", Message: "No snapshot."})
	if data["reason"] != "snapshot" || data["rolledBack"] != false {
		t.Fatalf("snapshot refusal = %v", data)
	}
	if _, ok := data["snapshot"]; ok {
		t.Fatalf("a refusal without a snapshot names one: %v", data)
	}
	data = ask("req-5", appmgr.ImageSetResult{Reason: "unhealthy", RolledBack: true, Message: "Rolled back.", Snapshot: "s20261124T030000Z-abc"})
	if data["snapshot"] != "s20261124T030000Z-abc" || data["reason"] != "unhealthy" {
		t.Fatalf("failure after the snapshot = %v", data)
	}
}

func TestRollbackRepliesFlat(t *testing.T) {
	fx := newHubFixture(t)
	fx.connect()
	fx.waitIdle()

	ask := func(reqID string, result appmgr.RollbackResult) map[string]any {
		t.Helper()
		fx.app.mu.Lock()
		fx.app.rollback = result
		fx.app.mu.Unlock()
		fx.cloud.send(t, jscanon.Obj{
			{K: "id", V: reqID},
			{K: "type", V: "command"},
			{K: "data", V: map[string]any{"action": "app.rollback", "payload": map[string]any{
				"id": "res-1", "container": "web", "buildId": "build_1785879774117_52c29d07",
			}}},
			{K: "timestamp", V: time.Now().Unix()},
		})
		for {
			resp := fx.cloud.next(t)
			if resp["type"] == "command.response" {
				data, _ := resp["data"].(map[string]any)
				return data
			}
		}
	}

	data := ask("req-1", appmgr.RollbackResult{Reason: "missing", Message: "Build is gone."})
	if data["id"] != "req-1" || data["success"] != false || data["reason"] != "missing" ||
		data["rolledBack"] != false || data["message"] != "Build is gone." {
		t.Fatalf("failure reply = %v", data)
	}
	if _, ok := data["data"]; ok {
		t.Fatalf("a flat reply must not nest data: %v", data)
	}
	fx.app.mu.Lock()
	calls := strings.Join(fx.app.calls, " ")
	fx.app.mu.Unlock()
	if !strings.Contains(calls, "rollback:web:build_1785879774117_52c29d07") {
		t.Fatalf("calls = %s", calls)
	}

	imageID := "sha256:" + strings.Repeat("a", 64)
	sha := strings.Repeat("b", 40)
	data = ask("req-2", appmgr.RollbackResult{
		Success: true, Changed: true, BuildID: "build_2", Previous: "build_3", CommitSha: sha, ImageID: imageID,
	})
	want := map[string]any{
		"id": "req-2", "success": true, "changed": true, "buildId": "build_2", "previous": "build_3",
		"commitSha": sha, "imageId": imageID,
	}
	if len(data) != len(want) {
		t.Fatalf("success reply = %v", data)
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("%s = %v, want %v", k, data[k], v)
		}
	}

	data = ask("req-3", appmgr.RollbackResult{Success: true})
	if len(data) != 3 || data["success"] != true || data["changed"] != false {
		t.Fatalf("unchanged reply = %v", data)
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
	run("app.network.mode", map[string]any{"name": "web", "mode": "host"})
	run("app.network.mode", map[string]any{"name": "web"}) // absent mode → bridge
	run("app.network.isolate", map[string]any{"name": "web", "isolated": true})
	run("app.network.isolate", map[string]any{"name": "web", "isolated": false})
	run("app.network.isolate", map[string]any{"name": "web"}) // absent → off
	run("app.image.set", map[string]any{"id": "res-1", "container": "db", "image": "postgres:17.6", "pull": true})
	run("app.image.set", map[string]any{"container": "db", "image": "postgres:17.6", "pull": "yes"}) // only true forces
	run("app.image.set", map[string]any{"container": "db", "image": "postgres:17.6", "requireSnapshot": true})
	run("app.rollback", map[string]any{"id": "res-1", "container": "web", "buildId": "build_1_a"})
	run("app.rollback", map[string]any{"container": "web", "buildId": "build_1_a", "requireSnapshot": "yes"}) // only true requires

	want := []string{
		"delete:web:true",
		"delete:web:false",
		"getenv:app-1",
		"getenv:web",
		"buildstats:c1",
		"delenv:web:A,B",
		"redeploy:web:dev",
		"restart:web",
		"networkmode:web:host",
		"networkmode:web:",
		"isolate:web:on",
		"isolate:web:off",
		"isolate:web:off",
		"image:db:postgres:17.6:true:false",
		"image:db:postgres:17.6:false:false",
		"image:db:postgres:17.6:false:true",
		"rollback:web:build_1_a:false",
		"rollback:web:build_1_a:false",
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

func TestMailCommands(t *testing.T) {
	fx := newHubFixture(t)
	run := func(action string, payload any) {
		t.Helper()
		if _, err := fx.h.commands[action].fn(payload); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}

	run("mail.create", map[string]any{"email": "a@x.test", "password": "p"})
	run("mail.delete", map[string]any{"email": "a@x.test"})
	run("mail.list", map[string]any{"domain": "x.test"})
	run("mail.list", map[string]any{}) // no domain
	run("mail.password", map[string]any{"email": "a@x.test", "password": "n"})

	want := []string{
		"create:a@x.test:p:p",
		"delete:a@x.test",
		"list:x.test",
		"listall",
		"password:a@x.test:n:n",
	}
	got := fx.mail.called()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %s, want %s", i, got[i], want[i])
		}
	}

	if fx.h.commands["mail.send"] != nil {
		t.Error("mail.send must not be reachable from the Hub")
	}
}

func TestMailUnavailable(t *testing.T) {
	fx := newHubFixture(t, func(f *hubFixture) { f.noMail = true })
	for _, action := range []string{"mail.create", "mail.delete", "mail.list", "mail.password"} {
		if _, err := fx.h.commands[action].fn(map[string]any{}); err == nil {
			t.Errorf("%s should fail without a Mail service", action)
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
		"app.network.set", "app.network.mode", "app.network.isolate",
		"app.port.set", "app.redeploy", "app.restart", "app.start",
		"app.stats", "app.stop",
		"app.volumes.set", "dns.add", "dns.delete", "dns.list", "domain.add",
		"domain.delete", "domain.list", "proxy.tunnel", "system.info", "system.stats",
		"app.logs.on", "app.logs.off", "app.build_logs.on", "app.build_logs.off",
		"terminal.open", "terminal.close", "mail.create", "mail.delete", "mail.list",
		"mail.password", "system.update", "domain.mtls.set", "domain.mtls.cert.issue",
		"domain.mtls.cert.scope", "domain.mtls.cert.revoke", "domain.mtls.cert.list",
		"domain.mtls.cert.import",
		"backup.target.list", "backup.target.prepare", "backup.target.confirm",
		"backup.target.cancel", "backup.target.update", "backup.target.delete",
		"backup.target.adopt", "backup.target.servers", "backup.key",
		"backup.job.list", "backup.job.create", "backup.job.update", "backup.job.delete",
		"backup.job.run", "backup.retired.list", "backup.retired.purge",
		"backup.coverage", "backup.status", "backup.history", "backup.point.list",
		"backup.restore.preview", "backup.restore", "backup.restore.cancel",
		"backup.restore.list",
		"snapshot.status", "snapshot.list", "snapshot.history", "snapshot.settings",
		"snapshot.settings.update", "snapshot.take", "snapshot.delete",
		"snapshot.restore.preview", "snapshot.restore",
		"app.image.set", "app.rollback",
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
		"domain.mtls.cert.list": 30 * time.Minute,
		"mail.list":             60 * time.Minute,
		"system.info":           60 * time.Minute, "system.stats": 60 * time.Second,
	}
	for name, want := range intervals {
		if got := fx.h.commands[name].interval; got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
}

func TestMTLSCommandsReachTheService(t *testing.T) {
	fx := newHubFixture(t)
	run := func(name string, payload map[string]any) {
		t.Helper()
		if _, err := fx.h.commands[name].fn(payload); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	run("domain.mtls.set", map[string]any{"domain": "x.com", "mode": "optional", "caPem": "PEM"})
	run("domain.mtls.cert.issue", map[string]any{"domain": "x.com", "csr": "CSR"})
	run("domain.mtls.cert.scope", map[string]any{"fingerprint": "aa", "hosts": []any{"x.com"}})
	run("domain.mtls.cert.revoke", map[string]any{"fingerprint": "aa"})
	run("domain.mtls.cert.list", map[string]any{"domain": "x.com"})
	run("domain.mtls.cert.import", map[string]any{"cert": "PEM", "hosts": []any{"x.com"}})

	want := []string{
		"set x.com optional PEM", "issue (operator) x.com CSR", "scope aa",
		"revoke (operator) aa", "list (operator) x.com", "import (operator) PEM",
	}
	// A new record reaches the dashboard's mirror right away, like an issue does.
	if got := fx.h.commands["domain.mtls.cert.import"].triggers; !reflect.DeepEqual(got, []string{"domain.mtls.cert.list"}) {
		t.Fatalf("import triggers = %v", got)
	}
	if got := fx.mtls.calls; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// A CA import carries the CA's private key, which must never cross the hub, and
// removing one destroys it, which is the operator's call on the server itself.
func TestHubCannotImportOrRemoveACA(t *testing.T) {
	fx := newHubFixture(t)
	for name := range fx.h.commands {
		if strings.Contains(name, "ca.import") || strings.Contains(name, "ca.remove") {
			t.Fatalf("the hub offers %s", name)
		}
	}
}

func TestSystemStats(t *testing.T) {
	fx := newHubFixture(t)
	res, err := fx.h.commands["system.stats"].fn(nil)
	if err != nil {
		t.Fatalf("system.stats: %v", err)
	}
	env, ok := res.(api.Result)
	if !ok || !env.Status {
		t.Fatalf("system.stats result: %+v", res)
	}
	stats, _ := env.Data.(jscanon.Obj)
	if len(stats) != 4 || stats[0].K != "cpu" || stats[3].K != "network" {
		t.Errorf("stats payload: %v", stats)
	}
}

func TestSystemStatsUnavailable(t *testing.T) {
	fx := newHubFixture(t)
	fx.h.deps.SysStats = nil
	if _, err := fx.h.commands["system.stats"].fn(nil); err == nil {
		t.Fatal("system.stats should fail when SysStats is nil")
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

// fakeBackup records which backup methods ran, with the caller where one is
// asked.
type fakeBackup struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeBackup) rec(call string, payload any) api.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, _ := payload.(map[string]any); m != nil && m["id"] != nil {
		call += " " + fmt.Sprint(m["id"])
	}
	f.calls = append(f.calls, call)
	return api.Res(true, nil, []any{})
}

func (f *fakeBackup) Targets() api.Result             { return f.rec("targets", nil) }
func (f *fakeBackup) PrepareTarget(p any) api.Result  { return f.rec("prepare", p) }
func (f *fakeBackup) ConfirmTarget(p any) api.Result  { return f.rec("confirm", p) }
func (f *fakeBackup) CancelTarget(p any) api.Result   { return f.rec("cancel", p) }
func (f *fakeBackup) UpdateTarget(p any) api.Result   { return f.rec("update", p) }
func (f *fakeBackup) DeleteTarget(p any) api.Result   { return f.rec("delete", p) }
func (f *fakeBackup) AdoptTarget(p any) api.Result    { return f.rec("adopt", p) }
func (f *fakeBackup) TargetServers(p any) api.Result  { return f.rec("servers", p) }
func (f *fakeBackup) Jobs() api.Result                { return f.rec("jobs", nil) }
func (f *fakeBackup) CreateJob(p any) api.Result      { return f.rec("job.create", p) }
func (f *fakeBackup) UpdateJob(p any) api.Result      { return f.rec("job.update", p) }
func (f *fakeBackup) DeleteJob(p any) api.Result      { return f.rec("job.delete", p) }
func (f *fakeBackup) RunJob(p any) api.Result         { return f.rec("job.run", p) }
func (f *fakeBackup) Retired() api.Result             { return f.rec("retired", nil) }
func (f *fakeBackup) PurgeRetired(p any) api.Result   { return f.rec("purge", p) }
func (f *fakeBackup) Coverage() api.Result            { return f.rec("coverage", nil) }
func (f *fakeBackup) Status() api.Result              { return f.rec("status", nil) }
func (f *fakeBackup) History(p any) api.Result        { return f.rec("history", p) }
func (f *fakeBackup) Points(p any) api.Result         { return f.rec("points", p) }
func (f *fakeBackup) PreviewRestore(p any) api.Result { return f.rec("preview", p) }
func (f *fakeBackup) StartRestore(p any) api.Result   { return f.rec("restore", p) }
func (f *fakeBackup) CancelRestore(p any) api.Result  { return f.rec("restore.cancel", p) }
func (f *fakeBackup) Restores() api.Result            { return f.rec("restores", nil) }
func (f *fakeBackup) SnapshotStatus() api.Result      { return f.rec("snapshot.status", nil) }
func (f *fakeBackup) SnapshotPoints(p any) api.Result { return f.rec("snapshot.list", p) }
func (f *fakeBackup) SnapshotHistory(p any) api.Result {
	return f.rec("snapshot.history", p)
}
func (f *fakeBackup) SnapshotSettings() api.Result { return f.rec("snapshot.settings", nil) }
func (f *fakeBackup) UpdateSnapshotSettings(p any) api.Result {
	return f.rec("snapshot.settings.update", p)
}
func (f *fakeBackup) TakeSnapshot(p any) api.Result   { return f.rec("snapshot.take", p) }
func (f *fakeBackup) DeleteSnapshot(p any) api.Result { return f.rec("snapshot.delete", p) }
func (f *fakeBackup) PreviewSnapshotRestore(p any) api.Result {
	return f.rec("snapshot.restore.preview", p)
}
func (f *fakeBackup) StartSnapshotRestore(p any) api.Result {
	return f.rec("snapshot.restore", p)
}
func (f *fakeBackup) Key(c api.Caller, p any) api.Result {
	if c != api.Operator {
		return api.Res(false, "denied")
	}
	return f.rec("key (operator)", p)
}

func TestBackupCommandsReachTheService(t *testing.T) {
	fb := &fakeBackup{}
	fx := newHubFixture(t, func(f *hubFixture) { f.backup = fb })
	for _, name := range []string{"backup.key", "backup.job.run", "backup.restore"} {
		res, err := fx.h.commands[name].fn(map[string]any{"id": "x1"})
		if err != nil || !res.(api.Result).Status {
			t.Fatalf("%s: %v %v", name, res, err)
		}
	}
	if want := []string{"key (operator) x1", "job.run x1", "restore x1"}; !reflect.DeepEqual(fb.calls, want) {
		t.Fatalf("calls = %v, want %v", fb.calls, want)
	}
	// Lists are tasks; a change re-sends the lists it touched.
	for name, want := range map[string]time.Duration{
		"backup.target.list": 30 * time.Minute, "backup.job.list": 30 * time.Minute,
		"backup.retired.list": 30 * time.Minute, "backup.coverage": 30 * time.Minute,
		"backup.status": 5 * time.Minute, "backup.history": 30 * time.Minute,
		"backup.restore.list": 30 * time.Minute,
	} {
		if got := fx.h.commands[name].interval; got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
	if got, _ := fx.h.Triggers("backup.job.create"); !reflect.DeepEqual(got, []string{"backup.job.list", "backup.coverage", "backup.status"}) {
		t.Errorf("job.create triggers = %v", got)
	}
	for _, name := range []string{"backup.target.prepare", "backup.key", "backup.point.list", "backup.restore.preview"} {
		if got, ok := fx.h.Triggers(name); !ok || len(got) != 0 {
			t.Errorf("%s changes nothing but triggers %v", name, got)
		}
	}
}

func TestSnapshotCommandsReachTheService(t *testing.T) {
	fb := &fakeBackup{}
	fx := newHubFixture(t, func(f *hubFixture) { f.backup = fb })
	for _, name := range []string{"snapshot.take", "snapshot.restore", "snapshot.settings.update"} {
		res, err := fx.h.commands[name].fn(map[string]any{"id": "x1"})
		if err != nil || !res.(api.Result).Status {
			t.Fatalf("%s: %v %v", name, res, err)
		}
	}
	if want := []string{"snapshot.take x1", "snapshot.restore x1", "snapshot.settings.update x1"}; !reflect.DeepEqual(fb.calls, want) {
		t.Fatalf("calls = %v, want %v", fb.calls, want)
	}
	for name, want := range map[string]time.Duration{
		"snapshot.status": 5 * time.Minute, "snapshot.list": 30 * time.Minute,
		"snapshot.history": 30 * time.Minute, "snapshot.settings": 30 * time.Minute,
	} {
		if got := fx.h.commands[name].interval; got != want {
			t.Errorf("%s interval = %v, want %v", name, got, want)
		}
	}
	if got, _ := fx.h.Triggers("snapshot.restore"); !reflect.DeepEqual(got, []string{"backup.restore.list", "snapshot.status"}) {
		t.Errorf("snapshot.restore triggers = %v", got)
	}
	if got, _ := fx.h.Triggers("snapshot.delete"); !reflect.DeepEqual(got, []string{"snapshot.list", "snapshot.status"}) {
		t.Errorf("snapshot.delete triggers = %v", got)
	}
}

// ODAC's own state is restored on a freshly installed server, from its CLI;
// the hub has no door to it.
func TestHubCannotStageAServerRestore(t *testing.T) {
	fx := newHubFixture(t, func(f *hubFixture) { f.backup = &fakeBackup{} })
	for name := range fx.h.commands {
		if strings.HasPrefix(name, "backup.server.") {
			t.Fatalf("the hub offers %s", name)
		}
	}
}

// Without backups the commands keep their place but no list is sent.
func TestBackupListsAreNotTasksWithoutBackups(t *testing.T) {
	fx := newHubFixture(t)
	for _, name := range fx.h.order {
		if (strings.HasPrefix(name, "backup.") || strings.HasPrefix(name, "snapshot.")) && fx.h.commands[name].hasInterval {
			t.Errorf("%s is a task on a server without backups", name)
		}
	}
	if _, err := fx.h.commands["backup.job.list"].fn(nil); err == nil {
		t.Fatal("backup.job.list answered without backups")
	}
}

// TestRedeployForwardsRuntimePresence pins the one distinction the hub must
// not flatten: an absent runtime member keeps the stored override, a null
// one clears it.
func TestRedeployForwardsRuntimePresence(t *testing.T) {
	fx := newHubFixture(t)
	fn := fx.h.commands["app.redeploy"].fn
	for _, p := range []map[string]any{
		{"container": "web", "commitSha": "abc1234"},
		{"container": "web", "runtime": nil},
		{"container": "web", "runtime": map[string]any{"version": "22"}},
	} {
		if _, err := fn(p); err != nil {
			t.Fatal(err)
		}
	}
	fx.app.mu.Lock()
	got := fx.app.redeploys
	fx.app.mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("redeploys = %d", len(got))
	}
	if got[0].HasRuntime || got[0].CommitSha != "abc1234" {
		t.Errorf("absent runtime = %+v", got[0])
	}
	if !got[1].HasRuntime || got[1].Runtime != nil {
		t.Errorf("null runtime = %+v", got[1])
	}
	if v, _ := got[2].Runtime.(map[string]any); !got[2].HasRuntime || v["version"] != "22" {
		t.Errorf("versioned runtime = %+v", got[2])
	}
}
