// Package hub is the Go port of server/src/Hub.js (+ Hub/WebSocket.js in
// ws.go/signer.go and Hub/Terminal.js in terminal.go): the cloud agent —
// signed WebSocket link, inbound command dispatch, periodic task pushes,
// log-stream forwarding, terminal sessions and the HTTP API (auth/app).
// Protocol contract: docs/migration/contracts/hub-protocol.md.
//
// Deviations from Node (deliberate):
//   - onConnect's four initial task pushes run sequentially on one tracked
//     goroutine (Node fires four un-awaited async calls whose sends
//     interleave nondeterministically).
//   - Task/command work runs on goroutines tracked by a WaitGroup so tests
//     can drain them; Node relied on the event loop settling.
//   - Where JS object insertion order leaks into outbound payloads built
//     from Go maps (task envelopes carrying decoded config), keys are
//     emitted sorted — the Hub re-verifies our bytes as-sent, so any fixed
//     order is valid (see internal/jscanon).
//   - Unverifiable messages are logged with the splice reason; Node logs
//     the same two lines from MessageSigner + Hub.
package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/appmgr"
	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/docker"
	"odac/internal/jscanon"
	"odac/internal/lang"
	"odac/internal/logx"
)

// DefaultURL is the production Hub endpoint; ODAC_HUB_URL overrides.
const DefaultURL = "https://hub.odac.run"

// AppService is the slice of appmgr.Manager the command table calls.
type AppService interface {
	Create(config any) *api.Result
	GetBuildStats(id any) *api.Result
	Delete(id any, purge bool) *api.Result
	GetEnv(id any) *api.Result
	DeleteEnv(id any, keys []string) *api.Result
	LinkEnv(id any, target string) *api.Result
	SetEnv(id any, env map[string]any) *api.Result
	UnlinkEnv(id any, target string) *api.Result
	List(detailed bool) *api.Result
	SetNetworks(id any, networks []any, payloadOK bool) *api.Result
	SetPorts(id any, portsPayload []any, payloadOK bool) *api.Result
	SetVolumes(id any, volumes []any, payloadOK bool) *api.Result
	Redeploy(payload appmgr.RedeployPayload) *api.Result
	Restart(id any) *api.Result
	SubscribeToLogs(appName string, cb func(applog.Entry)) func()
}

// DNSService is the slice of dataplane.DNS the command table calls.
type DNSService interface {
	Record(args ...map[string]any)
	Delete(args ...map[string]any)
	List(domainArg any) api.Result
}

// DomainService is the slice of domains.Domain the command table calls.
type DomainService interface {
	Add(domainArg, appID any) api.Result
	Delete(domainArg any, skipSync bool) api.Result
	List(appIDArg any) api.Result
}

// ProxyService is the slice of dataplane.Proxy the command table calls.
type ProxyService interface {
	SetTunnels(tunnels []dataplane.Tunnel) int
}

// TerminalExec is one container shell session (docker.Terminal's surface).
type TerminalExec interface {
	Write(data []byte) bool
	Resize(cols, rows int) bool
	Close(reason string)
	Closed() bool
}

// ContainerService is the slice of docker.Client the Hub calls.
type ContainerService interface {
	GetStats(name string, nowMs int64) *docker.Stats
	SubscribeToBuildLogs(appName string, cb func(applog.Entry)) func()
	GetLastBuildLog(appName string) string
	CreateTerminalSession(appName string, opts docker.TerminalOptions) (TerminalExec, error)
}

// Deps carries the Hub's collaborators. App/Container may be nil on a
// docker-less host (their commands answer failure); SysUpdate stays nil
// until task 3.7 lands the updater.
type Deps struct {
	App       AppService
	DNS       DNSService
	Domain    DomainService
	Proxy     ProxyService
	Container ContainerService
	SysInfo   func() jscanon.Obj
	SysUpdate func() (any, error)
}

// command mirrors one entry of Node's this.commands table.
type command struct {
	fn          func(payload any) (any, error)
	triggers    []string
	interval    time.Duration
	hasInterval bool // configure() eligibility survives interval=0
	lastRun     time.Time
}

// Hub mirrors the Hub.js singleton.
type Hub struct {
	cfg  *config.Store
	log  *logx.Logger
	deps Deps

	ws        *wsClient
	terminals *terminalManager

	baseURL string
	wsURL   string

	// Test seams.
	now      func() time.Time
	sleep    func(time.Duration)
	postJSON func(url string, body []byte, headers map[string]string) (*httpResponse, error)

	bg sync.WaitGroup

	mu       sync.Mutex
	active   bool
	commands map[string]*command
	order    []string
	logSubs  map[string]func()
}

// New wires a Hub against baseURL (pass DefaultURL or the ODAC_HUB_URL
// override; Node computes the same at module load).
func New(cfg *config.Store, baseURL string, deps Deps) *Hub {
	h := &Hub{
		cfg:      cfg,
		log:      logx.New("Hub"),
		deps:     deps,
		ws:       newWSClient(),
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		now:      time.Now,
		sleep:    time.Sleep,
		commands: map[string]*command{},
		logSubs:  map[string]func(){},
	}
	// HUB_URL.replace(/^http/, 'ws') + '/ws'
	h.wsURL = "ws" + strings.TrimPrefix(h.baseURL, "http") + "/ws"
	h.postJSON = defaultPostJSON

	h.terminals = newTerminalManager(h.wsURL, func() string {
		token, _ := h.hubConfig()
		return token
	}, cfg, deps.Container)

	h.buildCommands()

	h.ws.onConnect = func() {
		h.spawn(func() {
			for _, task := range []string{"system.info", "app.list", "dns.list", "domain.list"} {
				h.triggerSync(task)
			}
		})
	}
	h.ws.onMessage = h.handleMessage
	h.ws.onDisconnect = func() {
		h.log.Log("[Hub] Disconnected from Cloud. Cleaning up active log streams...")
		h.unsubscribeAllLogs()
		h.terminals.closeAll()
	}
	return h
}

func (h *Hub) spawn(fn func()) {
	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		fn()
	}()
}

// register appends one command preserving Node's table insertion order
// (check() iterates Object.entries in that order).
func (h *Hub) register(name string, c *command) {
	c.hasInterval = c.interval != 0
	h.commands[name] = c
	h.order = append(h.order, name)
}

// Start ports start().
func (h *Hub) Start() {
	h.mu.Lock()
	h.active = true
	h.mu.Unlock()
	h.log.Log("Hub Service started")
}

// Stop ports stop().
func (h *Hub) Stop() {
	h.mu.Lock()
	h.active = false
	h.mu.Unlock()
	h.terminals.closeAll()
	h.ws.disconnect()
	h.log.Log("Hub Service stopped")
}

// Check ports check(): rides the 1s system tick.
func (h *Hub) Check() {
	h.mu.Lock()
	active := h.active
	h.mu.Unlock()
	if !active {
		return
	}

	token, _ := h.hubConfig()
	if token == "" {
		return
	}

	if !h.ws.connected() {
		if h.ws.shouldReconnect() {
			h.ws.connect(h.wsURL, token)
		}
		return
	}

	now := h.now()
	for _, name := range h.order {
		h.mu.Lock()
		cmd := h.commands[name]
		due := cmd.interval > 0 && now.Sub(cmd.lastRun) >= cmd.interval
		if due {
			cmd.lastRun = now
		}
		h.mu.Unlock()
		if due {
			task := name
			h.spawn(func() { h.executeTask(task) }) // without blocking the loop
		}
	}
}

// Trigger re-runs (and re-sends) a task after an event; the appmgr Hub seam.
// Fire-and-forget like Node's un-awaited trigger() calls.
func (h *Hub) Trigger(name string) {
	h.spawn(func() { h.triggerSync(name) })
}

func (h *Hub) triggerSync(name string) {
	h.mu.Lock()
	cmd := h.commands[name]
	if cmd == nil {
		h.mu.Unlock()
		return
	}
	if cmd.interval > 0 {
		cmd.lastRun = h.now() // reset timer if it's a task
	}
	h.mu.Unlock()
	h.executeTask(name)
}

func (h *Hub) executeTask(name string) {
	if !h.ws.connected() {
		return
	}
	h.mu.Lock()
	cmd := h.commands[name]
	h.mu.Unlock()
	if cmd == nil {
		return
	}
	data, err := runFn(cmd.fn, nil)
	if err != nil {
		h.log.Log("Task %s error: %s", name, err.Error())
		return
	}
	if data != nil { // Node: undefined → nothing to send
		h.sendSignedMessage(name, data)
	}
}

// runFn invokes a command fn converting panics into errors, the equivalent
// of Node's try/catch around every handler (same policy as api.runHandler).
func runFn(fn func(payload any) (any, error), payload any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn(payload)
}

// processCommand ports processCommand(): dispatch, optional response,
// trigger fan-out.
func (h *Hub) processCommand(action string, payload any, requestID any) {
	h.mu.Lock()
	cmd := h.commands[action]
	h.mu.Unlock()
	if action == "" || cmd == nil {
		h.log.Log("Invalid or unknown command received: %s", action)
		return
	}

	h.log.Log("Processing command: %s", action)

	h.mu.Lock()
	if cmd.interval > 0 {
		cmd.lastRun = h.now()
	}
	h.mu.Unlock()

	result, err := runFn(cmd.fn, payload)
	if err != nil {
		h.log.Log("Command execution failed: %s", err.Error())
		if jsTruthy(requestID) {
			h.sendCommandResponse(requestID, cmdResult{
				success: false, hasSuccess: true,
				message: err.Error(), hasMessage: true,
			})
		}
		return
	}

	if jsTruthy(requestID) {
		h.sendCommandResponse(requestID, normalizeResult(result))
	}
	for _, task := range cmd.triggers {
		h.triggerSync(task)
	}
}

// cmdResult is the normalized {success, message, data} triple feeding
// command.response, with JS-undefined tracked per field.
type cmdResult struct {
	success    any
	hasSuccess bool
	message    any
	hasMessage bool
	data       any
	hasData    bool
}

// normalizeResult ports #sendCommandResponse's normalization: success is
// result.success falling back to result.result (Api.result envelope);
// undefined fields vanish from the JSON.
func normalizeResult(result any) cmdResult {
	switch r := result.(type) {
	case nil:
		// Node: result || {result: true}
		return cmdResult{success: true, hasSuccess: true}
	case api.Result:
		return apiCmdResult(r)
	case *api.Result:
		if r == nil {
			return cmdResult{success: true, hasSuccess: true}
		}
		return apiCmdResult(*r)
	case map[string]any:
		out := cmdResult{}
		if v, ok := r["success"]; ok {
			out.success, out.hasSuccess = v, true
		} else if v, ok := r["result"]; ok {
			out.success, out.hasSuccess = v, true
		}
		if v, ok := r["message"]; ok {
			out.message, out.hasMessage = v, true
		}
		if v, ok := r["data"]; ok {
			out.data, out.hasData = v, true
		}
		return out
	default:
		// A task envelope of another shape; send it as data like Node's
		// property reads would find nothing.
		return cmdResult{}
	}
}

func apiCmdResult(r api.Result) cmdResult {
	out := cmdResult{success: r.Status, hasSuccess: true}
	if r.Message != nil || r.NullMsg {
		out.message, out.hasMessage = r.Message, true
	}
	if r.HasData {
		out.data, out.hasData = r.Data, true
	}
	return out
}

func (h *Hub) sendCommandResponse(requestID any, r cmdResult) {
	data := jscanon.Obj{{K: "id", V: requestID}}
	if r.hasSuccess {
		data = append(data, jscanon.Field{K: "success", V: r.success})
	}
	if r.hasMessage {
		data = append(data, jscanon.Field{K: "message", V: r.message})
	}
	if r.hasData {
		data = append(data, jscanon.Field{K: "data", V: r.data})
	}
	h.sendSignedMessage("command.response", data)
}

// sendSignedMessage ports #sendSignedMessage: canonical JSON + HMAC.
func (h *Hub) sendSignedMessage(msgType string, data any) {
	canonical, err := toCanon(data)
	if err != nil {
		h.log.Error("Failed to encode "+msgType+" message:", err.Error())
		return
	}
	timestamp := h.now().Unix()
	_, secret := h.hubConfig()

	signature, err := sign(nil, msgType, canonical, timestamp, secret)
	if err != nil {
		h.log.Error("Failed to sign "+msgType+" message:", err.Error())
		return
	}
	var sigVal any
	if signature != "" {
		sigVal = signature
	}

	payload, err := jscanon.Marshal(jscanon.Obj{
		{K: "type", V: msgType},
		{K: "data", V: canonical},
		{K: "timestamp", V: timestamp},
		{K: "signature", V: sigVal},
	})
	if err != nil {
		h.log.Error("Failed to encode "+msgType+" message:", err.Error())
		return
	}
	h.ws.send(payload)
}

// toCanon converts service results into jscanon-supported values.
func toCanon(v any) (any, error) {
	switch val := v.(type) {
	case api.Result:
		return resultTree(val), nil
	case *api.Result:
		if val == nil {
			return nil, nil
		}
		return resultTree(*val), nil
	case applog.Entry:
		return entryTree(val), nil
	case []applog.Entry:
		list := make([]any, len(val))
		for i, e := range val {
			list[i] = entryTree(e)
		}
		return list, nil
	default:
		return v, nil
	}
}

// resultTree emits the Api.result envelope with Node's literal key order
// {result, message, data} and undefined-omission.
func resultTree(r api.Result) jscanon.Obj {
	obj := jscanon.Obj{{K: "result", V: r.Status}}
	if r.Message != nil || r.NullMsg {
		obj = append(obj, jscanon.Field{K: "message", V: r.Message})
	}
	if r.HasData {
		obj = append(obj, jscanon.Field{K: "data", V: r.Data})
	}
	return obj
}

func entryTree(e applog.Entry) jscanon.Obj {
	return jscanon.Obj{{K: "t", V: e.T}, {K: "d", V: e.D}, {K: "ts", V: e.TS}}
}

// handleMessage ports #handleMessage over raw wire bytes (the splice
// verifier needs them unparsed; see signer.go).
func (h *Hub) handleMessage(raw []byte) {
	_, secret := h.hubConfig()
	if ok, reason := verifyWire(raw, secret, h.now()); !ok {
		h.log.Log("WebSocket message verification failed: %s", reason)
		return
	}

	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		h.log.Log("Failed to handle WebSocket message: %s", err.Error())
		return
	}

	switch msg["type"] {
	case "disconnect":
		reason, _ := msg["reason"].(string)
		display := reason
		if display == "" {
			display = "unknown"
		}
		h.log.Log("Cloud requested disconnect: %s", display)
		if reason == "token_invalid" || reason == "signature_invalid" {
			h.log.Log("Authentication credentials invalid, clearing config")
			h.cfg.Delete("hub")
		}
		h.ws.disconnect()
	case "command":
		data, _ := msg["data"].(map[string]any)
		action, _ := data["action"].(string)
		requestID := msg["id"] // requestId = message.id || message.requestId
		if !jsTruthy(requestID) {
			requestID = msg["requestId"]
		}
		// Fire-and-forget like Node (processCommand is called without await
		// there). handleMessage runs on the websocket read loop, and
		// coder/websocket only services incoming ping/pong control frames
		// while a Read is in flight — a long command (app.create builds for
		// minutes) run inline would starve the heartbeat into terminating a
		// healthy connection, and would also delay every command sent in the
		// meantime (e.g. app.build_logs.on during that same build).
		go h.processCommand(action, data["payload"], requestID)
	}
}

// handleConfigure ports #handleConfigure: interval overrides from the cloud
// (seconds ×1000, falsy disables). Only commands that were born with an
// interval are configurable.
func (h *Hub) handleConfigure(payload any) (any, error) {
	m, _ := payload.(map[string]any)
	intervals, ok := m["intervals"].(map[string]any)
	if m == nil || !ok {
		// Node's schema check also lets a null/array intervals value
		// through (typeof null === 'object') and then throws on
		// Object.entries(null); the thrown case only differs in the
		// command.response message, which nothing parses.
		h.log.Log("Invalid configure payload")
		return nil, nil
	}

	updated := false
	keys := make([]string, 0, len(intervals))
	for k := range intervals {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic log order (Node: insertion order)

	h.mu.Lock()
	for _, key := range keys {
		value := intervals[key]
		cmd := h.commands[key]
		if cmd == nil || !cmd.hasInterval {
			continue
		}
		var next time.Duration
		if jsTruthy(value) {
			if f, isNum := value.(float64); isNum {
				next = time.Duration(f*1000) * time.Millisecond
			} else {
				continue // Node: NaN interval, task effectively disabled — skip
			}
		}
		if cmd.interval != next {
			cmd.interval = next
			updated = true
			h.log.Log("Task interval updated: %s = %sms", key, fmt.Sprint(next.Milliseconds()))
		}
	}
	h.mu.Unlock()

	if updated {
		h.log.Log("Configuration updated: intervals synced")
	}
	return nil, nil
}

// getAppStats ports getAppStats(): container stats for every running app,
// in app-list order.
func (h *Hub) getAppStats() (any, error) {
	if h.deps.App == nil || h.deps.Container == nil {
		return api.Res(true, jscanon.Obj{}), nil
	}
	res := h.deps.App.List(true)
	var apps []any
	if res != nil && res.Status {
		apps, _ = res.Data.([]any)
	}

	stats := jscanon.Obj{}
	for _, item := range apps {
		app, _ := item.(map[string]any)
		if app == nil || app["status"] != "running" {
			continue
		}
		name, _ := app["name"].(string)
		if s := h.deps.Container.GetStats(name, h.now().UnixMilli()); s != nil {
			raw, err := json.Marshal(s)
			if err != nil {
				continue
			}
			stats = append(stats, jscanon.Field{K: name, V: json.RawMessage(raw)})
		}
	}
	return api.Res(true, stats), nil
}

// hubConfig reads config.hub's credentials under the value lock.
func (h *Hub) hubConfig() (token, secret string) {
	h.cfg.View(func() {
		hubCfg := h.cfg.Map("hub")
		token, _ = hubCfg["token"].(string)
		secret, _ = hubCfg["secret"].(string)
	})
	return token, secret
}

// buildCommands fills the command table in Node's insertion order.
func (h *Hub) buildCommands() {
	appUnavailable := func() (any, error) { return nil, errors.New("App service is not available") }
	withApp := func(fn func(payload any) (any, error)) func(payload any) (any, error) {
		if h.deps.App == nil {
			return func(any) (any, error) { return appUnavailable() }
		}
		return fn
	}
	withContainer := func(fn func(payload any) (any, error)) func(payload any) (any, error) {
		if h.deps.Container == nil {
			return func(any) (any, error) { return nil, errors.New("Container service is not available") }
		}
		return fn
	}

	h.register("configure", &command{fn: h.handleConfigure})
	h.register("app.create", &command{
		fn:       withApp(func(p any) (any, error) { return h.deps.App.Create(p), nil }),
		triggers: []string{"app.list"},
	})
	h.register("app.build_stats", &command{
		fn: withApp(func(p any) (any, error) {
			m := pmap(p)
			id := firstTruthy(m["name"], m["container"], m["id"])
			return h.deps.App.GetBuildStats(id), nil
		}),
	})
	h.register("app.delete", &command{
		fn: withApp(func(p any) (any, error) {
			m := pmap(p)
			return h.deps.App.Delete(m["id"], m["purge"] != false), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.env.get", &command{
		fn: withApp(func(p any) (any, error) { return h.deps.App.GetEnv(nameOrID(p)), nil }),
	})
	h.register("app.env.delete", &command{
		fn: withApp(func(p any) (any, error) {
			return h.deps.App.DeleteEnv(nameOrID(p), toStrings(pmap(p)["keys"])), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.env.link", &command{
		fn: withApp(func(p any) (any, error) {
			return h.deps.App.LinkEnv(nameOrID(p), str(pmap(p)["target"])), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.env.set", &command{
		fn: withApp(func(p any) (any, error) {
			env, _ := pmap(p)["env"].(map[string]any)
			return h.deps.App.SetEnv(nameOrID(p), env), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.env.unlink", &command{
		fn: withApp(func(p any) (any, error) {
			return h.deps.App.UnlinkEnv(nameOrID(p), str(pmap(p)["target"])), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.list", &command{
		fn:       withApp(func(any) (any, error) { return h.deps.App.List(true), nil }),
		interval: 30 * time.Minute,
	})
	h.register("app.network.set", &command{
		fn: withApp(func(p any) (any, error) {
			networks, ok := pmap(p)["networks"].([]any)
			return h.deps.App.SetNetworks(nameOrID(p), networks, ok), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.port.set", &command{
		fn: withApp(func(p any) (any, error) {
			ports, ok := pmap(p)["ports"].([]any)
			return h.deps.App.SetPorts(nameOrID(p), ports, ok), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("app.redeploy", &command{
		fn: withApp(func(p any) (any, error) {
			m := pmap(p)
			return h.deps.App.Redeploy(appmgr.RedeployPayload{
				Container: str(m["container"]),
				URL:       str(m["url"]),
				Token:     str(m["token"]),
				Branch:    str(m["branch"]),
				CommitSha: str(m["commitSha"]),
			}), nil
		}),
		triggers: []string{"app.list", "app.stats"},
	})
	h.register("app.restart", &command{
		fn:       withApp(func(p any) (any, error) { return h.deps.App.Restart(pmap(p)["container"]), nil }),
		triggers: []string{"app.list", "app.stats"},
	})
	h.register("app.stats", &command{
		fn:       func(any) (any, error) { return h.getAppStats() },
		interval: 60 * time.Second,
	})
	h.register("app.volumes.set", &command{
		fn: withApp(func(p any) (any, error) {
			volumes, ok := pmap(p)["volumes"].([]any)
			return h.deps.App.SetVolumes(nameOrID(p), volumes, ok), nil
		}),
		triggers: []string{"app.list"},
	})
	h.register("dns.add", &command{
		fn: func(p any) (any, error) {
			h.deps.DNS.Record(pmap(p))
			return api.Res(true, lang.T("DNS record added")), nil
		},
		triggers: []string{"dns.list"},
	})
	h.register("dns.delete", &command{
		fn: func(p any) (any, error) {
			h.deps.DNS.Delete(pmap(p))
			return api.Res(true, lang.T("DNS record deleted")), nil
		},
		triggers: []string{"dns.list"},
	})
	h.register("dns.list", &command{
		fn:       func(any) (any, error) { return h.deps.DNS.List(nil), nil },
		interval: 60 * time.Minute,
	})
	h.register("domain.add", &command{
		fn: func(p any) (any, error) {
			m := pmap(p)
			return h.deps.Domain.Add(m["domain"], m["app"]), nil
		},
		triggers: []string{"domain.list", "system.info"},
	})
	h.register("domain.delete", &command{
		fn: func(p any) (any, error) {
			return h.deps.Domain.Delete(pmap(p)["domain"], false), nil
		},
		triggers: []string{"domain.list", "system.info"},
	})
	h.register("domain.list", &command{
		fn:       func(any) (any, error) { return h.deps.Domain.List(nil), nil },
		interval: 30 * time.Minute,
	})
	h.register("proxy.tunnel", &command{
		fn: func(p any) (any, error) {
			tunnels, ok := pmap(p)["tunnels"].([]any)
			if !ok {
				return api.Res(false, "Invalid tunnels payload"), nil
			}
			list := make([]dataplane.Tunnel, 0, len(tunnels))
			for _, item := range tunnels {
				entry, _ := item.(map[string]any)
				if jsTruthy(entry["domain"]) && jsTruthy(entry["token"]) && jsTruthy(entry["container"]) {
					list = append(list, dataplane.Tunnel{
						Domain:    str(entry["domain"]),
						Token:     str(entry["token"]),
						Container: str(entry["container"]),
					})
				}
			}
			count := h.deps.Proxy.SetTunnels(list)
			// Node: __('%d tunnel(s) configured', size) — Lang substitutes
			// %s only, so the count never lands in the text. Reproduced.
			return api.Res(true, lang.T("%d tunnel(s) configured", fmt.Sprint(count))), nil
		},
	})
	h.register("system.info", &command{
		fn: func(any) (any, error) {
			if h.deps.SysInfo == nil {
				return nil, errors.New("System service is not available")
			}
			return api.Res(true, h.deps.SysInfo()), nil
		},
		interval: 60 * time.Minute,
	})
	h.register("app.logs.on", &command{fn: h.logsOn})
	h.register("app.logs.off", &command{fn: h.logsOff})
	h.register("app.build_logs.on", &command{fn: withContainer(h.buildLogsOn)})
	h.register("app.build_logs.off", &command{fn: h.buildLogsOff})
	h.register("terminal.open", &command{fn: func(p any) (any, error) { return h.terminals.open(pmap(p)) }})
	h.register("terminal.close", &command{fn: func(p any) (any, error) { return h.terminals.close(pmap(p)) }})
	h.register("system.update", &command{
		fn: func(any) (any, error) {
			if h.deps.SysUpdate == nil {
				// Task 3.7 fills this seam; nothing flips before 3.8.
				return nil, errors.New("Updater is not available")
			}
			return h.deps.SysUpdate()
		},
	})
}

// pmap coerces a command payload to a map (JS property reads on non-objects
// yield undefined).
func pmap(payload any) map[string]any {
	m, _ := payload.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// nameOrID mirrors payload.name || payload.id.
func nameOrID(payload any) any {
	m := pmap(payload)
	return firstTruthy(m["name"], m["id"])
}

func firstTruthy(values ...any) any {
	for _, v := range values {
		if jsTruthy(v) {
			return v
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

// str renders a decoded-JSON value the way Node's string usage saw it,
// with null/undefined staying empty.
func str(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		return jscanon.NumberToString(val)
	default:
		return fmt.Sprint(val)
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, str(item))
	}
	return out
}
