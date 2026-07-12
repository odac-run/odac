// Package appmgr is the Go port of server/src/App.js + App/Create.js +
// App/Deploy.js (task 3.4e): the app list state machine and 1s check tick,
// run/stop/restart/redeploy/delete, env manual/linked resolution, the
// port/volume/device/network/privilege handlers, recipe/template/git
// creation, Blue-Green deploys and runtime port auto-discovery.
//
// State model (Node parity, quirks included): the working set `apps` mirrors
// Node's #apps — live maps shared with the config store between saves.
// SaveApps rebuilds ephemeral-stripped copies into cfg (config-schema.md
// lists the stripped fields), so the working set keeps in-memory fields
// (status, pid, ip, started …) until the next Check() reload drops them,
// exactly like Node. Every access to the working set runs under
// cfg.View/Mutate — the same lock domain the dataplane payload builders use.
//
// Deviations from Node (deliberate):
//   - The auto-discovery adoption (pollForPort) re-fetches the app from the
//     current working set at write time instead of mutating a pointer
//     captured before the poll loop. Node mutates the captured object, whose
//     array may have been rebuilt by any intervening #saveApps — the
//     adoption is then silently lost (STATE.md 2026-07-12 trap).
//   - Container log streams are explicitly stopped when their runtime log
//     control ends; Node abandons the docker socket and lets it die with
//     the container.
//   - Poll/retry cadences are fields so tests can shrink them; defaults
//     match Node's literals.
package appmgr

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/config"
	"odac/internal/docker"
	"odac/internal/lang"
	"odac/internal/logx"
	"odac/internal/ports"
)

var __ = lang.T

// sensitiveKeyPattern mirrors SENSITIVE_KEY_PATTERN (dev cd7f08b: `pass`
// dropped — DB_PASS-style keys render unmasked).
var sensitiveKeys = []string{"cert", "key", "salt", "secret", "token"}

// Docker is the slice of *docker.Client that App/Create/Deploy call.
// Narrowed to an interface so tests run against a fake (no daemon).
var _ Docker = (*docker.Client)(nil)

type Docker interface {
	Available() bool
	RunApp(name string, options docker.RunOptions, buildLog docker.BuildLog) error
	Stop(name string)
	Remove(name string)
	Rename(oldName, newName string) error
	StreamLogs(name string, stdout, stderr io.Writer) (stop func(), err error)
	IsRunning(name string) bool
	List() []docker.ContainerInfo
	GetIP(nameOrID string) (string, error)
	GetStatus(name string) docker.Status
	GetListeningPorts(name string) []int
	GetImageExposedPorts(imageName string) []int
	SetNetworks(name string, networks []string) docker.SetNetworksResult
	EnsureImage(imageName string, logw io.Writer) error
	CloneRepo(url, branch, targetDir, token string, buildLog docker.BuildLog) error
	FetchRepo(url, branch, targetDir, token, commitSha string, buildLog docker.BuildLog) error
	Build(sourceDir, imageName, appName string, buildLog docker.BuildLog) error
	RegisterBuildLogger(appName string, logger *applog.Logger)
	UnregisterBuildLogger(appName string)
	ResolveHostPath(localPath string) string
}

// TokenIssuer is the Api surface App needs: app tokens for ODAC_API_KEY and
// the run dir holding api.sock (mounted read-only into api-enabled apps).
type TokenIssuer interface {
	GenerateAppToken(appName string, permissions any) string
	HostSocketDir() string
}

// ProxyController is the Proxy surface App needs after (re)starts.
type ProxyController interface {
	SyncConfig()
	PurgeCacheForApp(appID any)
}

// Hub is the Hub surface App needs: change notifications and recipe fetch.
// Task 3.6 provides the real client; a nil Hub drops triggers and fails
// recipe fetches.
type Hub interface {
	Trigger(event string)
	GetApp(appType string) (map[string]any, error)
}

// DomainDeleter cascades app deletion into the domain table (task 3.5).
type DomainDeleter interface {
	DeleteByApp(appName string) error
}

// Deps carries the Manager's collaborators. Hub, Domains, Api and Proxy may
// be nil until their tasks land; every use is nil-tolerant like the Node
// registry, which never resolves a missing module.
type Deps struct {
	Docker  Docker
	Api     TokenIssuer
	Proxy   ProxyController
	Hub     Hub
	Domains DomainDeleter
}

// Manager is the App.js singleton.
type Manager struct {
	cfg      *config.Store
	logsRoot string
	log      *logx.Logger
	clog     *logx.Logger // App.Create's own log module prefix
	dlog     *logx.Logger // App.Deploy's own log module prefix
	deps     Deps

	// Working set — Node's #apps. Reads require cfg.View, writes cfg.Mutate.
	apps []map[string]any

	// bg tracks fire-and-forget goroutines (check dispatches, port pollers,
	// HTTP scans) so tests — and a future graceful stop — can drain them.
	bg sync.WaitGroup

	// mu guards the bookkeeping maps below (Node relied on the event loop).
	mu         sync.Mutex
	processing map[float64]bool          // app ids mid-operation
	creating   map[string]bool           // app names mid-create
	logStreams map[string]*runtimeStream // app name -> live runtime log
	loggers    map[string]*applog.Logger // app name -> logger instance

	// Test hooks: cadences default to Node's literals; sleep defaults to
	// time.Sleep. deploySwitchDelay is Deploy.js's NODE_ENV!=='test' 5s.
	// httpProbe answers whether ip:port speaks HTTP (jest mocked the http
	// module the same way).
	httpProbe         func(ip string, port int, timeout time.Duration, method string) bool
	sleep             func(time.Duration)
	pollInterval      time.Duration // #pollForPort retry (1s)
	scanPortInterval  time.Duration // #scanAndSaveHttpStatus port wait (500ms)
	scanProbeInterval time.Duration // #scanAndSaveHttpStatus probe retry (1s)
	probeTimeout      time.Duration // #detectHttpPort per-port timeout (2.5s)
	healthTimeout     time.Duration // Deploy http health per-request (2s)
	readyInterval     time.Duration // Deploy readiness retry (1s)
	renameInterval    time.Duration // Deploy rename retry (2s)
	restartDelay      time.Duration // standard restart settle (1s)
	deploySwitchDelay time.Duration // Blue-Green pre-stop drain (5s)
}

// spawn runs fn on a tracked goroutine (Node's un-awaited promises).
func (m *Manager) spawn(fn func()) {
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		fn()
	}()
}

// runtimeStream pairs a runtime log control with its docker stream stopper.
type runtimeStream struct {
	ctrl       *applog.RuntimeControl
	stopStream func()
}

func (r *runtimeStream) end() {
	if r.stopStream != nil {
		r.stopStream()
	}
	r.ctrl.End()
}

// New builds the Manager. logsRoot is <baseDir>/logs (applog's root).
func New(cfg *config.Store, logsRoot string, deps Deps) *Manager {
	return &Manager{
		cfg:      cfg,
		logsRoot: logsRoot,
		log:      logx.New("App"),
		clog:     logx.New("App.Create"),
		dlog:     logx.New("App.Deploy"),
		deps:     deps,

		processing: map[float64]bool{},
		creating:   map[string]bool{},
		logStreams: map[string]*runtimeStream{},
		loggers:    map[string]*applog.Logger{},

		httpProbe:         realHTTPProbe,
		sleep:             time.Sleep,
		pollInterval:      time.Second,
		scanPortInterval:  500 * time.Millisecond,
		scanProbeInterval: time.Second,
		probeTimeout:      2500 * time.Millisecond,
		healthTimeout:     2 * time.Second,
		readyInterval:     time.Second,
		renameInterval:    2 * time.Second,
		restartDelay:      time.Second,
		deploySwitchDelay: 5 * time.Second,
	}
}

// Init ports App.init: default+create the apps directory, load the app list,
// sweep orphaned green log dirs.
func (m *Manager) Init() {
	m.log.Log("Initializing apps...")

	m.cfg.Mutate(func() {
		appCfg, _ := m.cfg.Get("app").(map[string]any)
		if appCfg == nil {
			appCfg = map[string]any{}
			m.cfg.Set("app", appCfg)
		}
		if pathVal, _ := appCfg["path"].(string); pathVal == "" {
			switch {
			case os.Getenv("ODAC_APPS_PATH") != "":
				appCfg["path"] = os.Getenv("ODAC_APPS_PATH")
			case runtime.GOOS == "windows" || runtime.GOOS == "darwin":
				home, _ := os.UserHomeDir()
				appCfg["path"] = home + "/Odac/apps/"
			default:
				// Linux (prod & dev): relative path inside the container.
				appCfg["path"] = "/app/.odac/apps/"
			}
			m.cfg.Touch("app")
		}
	})

	if err := os.MkdirAll(m.appsPath(), 0o755); err != nil {
		m.log.Error("Failed to create apps directory: " + err.Error())
	}

	m.cfg.Mutate(func() {
		m.apps = m.loadAppsFromConfig()
	})

	// Best-effort sweep — green log dirs are short-lived by design, so any
	// surviving at startup is an orphan from a past Blue-Green deploy.
	m.spawn(func() {
		if err := m.CleanupStaleGreenLogs(); err != nil {
			m.log.Error("Stale green log sweep failed: " + err.Error())
		}
	})
}

// Check ports App.check — the 1s watchdog pulse: reload the working set from
// config and (re)start active apps that are not running.
func (m *Manager) Check() {
	type pulse struct {
		id     float64
		name   string
		status string
	}
	var pulses []pulse

	m.cfg.Mutate(func() {
		m.apps = m.loadAppsFromConfig()
		for _, app := range m.apps {
			if app["active"] != true {
				continue
			}
			id, _ := app["id"].(float64)
			name, _ := app["name"].(string)
			status, _ := app["status"].(string)
			pulses = append(pulses, pulse{id: id, name: name, status: status})
		}
	})

	for _, p := range pulses {
		m.mu.Lock()
		busy := m.processing[p.id]
		hasStream := m.logStreams[p.name] != nil
		m.mu.Unlock()
		if busy {
			continue
		}

		isRunning := m.isAppRunning(p.id)

		// Re-attach logger for running apps (if missing). Fire-and-forget
		// like Node's un-awaited #attachLogger(app).catch(...).
		if isRunning && !hasStream {
			name := p.name
			m.spawn(func() {
				if err := m.attachLogger(name); err != nil {
					m.log.Error("[Watchdog] Failed to reattach logger for %s: %s", name, err.Error())
				}
			})
		}

		// Node dispatches #run without awaiting it — the tick must not block
		// behind a slow container start. The processing lock is claimed
		// synchronously (Node's pre-await section of #run) so back-to-back
		// pulses cannot double-start the app.
		shouldRun := false
		if !isRunning && p.status == "running" {
			m.log.Log("App %s is not running. Restarting...", p.name)
			shouldRun = true
		} else if !isRunning && p.status != "stopped" && p.status != "errored" && p.status != "starting" && p.status != "installing" {
			shouldRun = true
		}
		if shouldRun && m.tryLockProcessing(p.id) {
			id := p.id
			m.spawn(func() {
				defer m.unlockProcessing(id)
				m.runHeld(id, nil)
			})
		}
	}
}

// ---- working-set primitives (Node's private App data management) ----

// appsPath returns config.app.path.
func (m *Manager) appsPath() string {
	var p string
	m.cfg.View(func() {
		if appCfg, _ := m.cfg.Get("app").(map[string]any); appCfg != nil {
			p, _ = appCfg["path"].(string)
		}
	})
	return p
}

// loadAppsFromConfig ports #loadAppsFromConfig. Caller holds cfg.Mutate
// (Normalize rewrites legacy port entries in place).
func (m *Manager) loadAppsFromConfig() []map[string]any {
	raw, _ := m.cfg.Get("apps").([]any)
	apps := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		if app, _ := a.(map[string]any); app != nil {
			if pl, _ := app["ports"].([]any); pl != nil {
				ports.Normalize(pl)
			}
			apps = append(apps, app)
		}
	}
	return apps
}

// getLocked ports #get: find by id, name or file (strict, like ===). Caller
// holds cfg.View or cfg.Mutate. Node's lazy first load is NOT reproduced
// here — it would write the working set under a read lock; Init()/Check()
// load it under the write lock instead, and both always run before any
// handler (main wires Init before the api server starts).
func (m *Manager) getLocked(id any) map[string]any {
	for _, app := range m.apps {
		if app["id"] == id || app["name"] == id || app["file"] == id {
			return app
		}
	}
	return nil
}

// setLocked ports #set: merge updates into the app and persist. Caller
// holds cfg.Mutate. A nil value deletes nothing — it is stored like Node's
// explicit null (saveApps strips the ephemeral ones anyway).
func (m *Manager) setLocked(id any, updates map[string]any) bool {
	app := m.getLocked(id)
	if app == nil {
		return false
	}
	for k, v := range updates {
		app[k] = v
	}
	m.saveAppsLocked()
	return true
}

// set is setLocked wrapped in its own Mutate (for callers holding no lock).
func (m *Manager) set(id any, updates map[string]any) bool {
	var ok bool
	m.cfg.Mutate(func() { ok = m.setLocked(id, updates) })
	return ok
}

// ephemeralFields are stripped by saveApps on every persist (contract
// config-schema.md).
var ephemeralFields = []string{"_appIdentity", "status", "pid", "uptime", "build", "health", "ip", "started"}

// saveAppsLocked ports #saveApps: rebuild ephemeral-stripped copies into the
// config store. The working set keeps the live (unstripped) maps — Node
// parity: #apps holds them until the next check-tick reload. Caller holds
// cfg.Mutate.
func (m *Manager) saveAppsLocked() {
	clean := make([]any, len(m.apps))
	for i, app := range m.apps {
		copy := make(map[string]any, len(app))
		for k, v := range app {
			copy[k] = v
		}
		for _, k := range ephemeralFields {
			delete(copy, k)
		}
		clean[i] = copy
	}
	m.cfg.Set("apps", clean)
}

// addLocked ports #add (script apps from App.start). Caller holds cfg.Mutate.
func (m *Manager) addLocked(file, typ string) map[string]any {
	name := filepath.Base(file)
	if typ == "script" {
		for _, ext := range scriptExts {
			if filepath.Ext(name) == ext {
				name = name[:len(name)-len(ext)]
				break
			}
		}
	}
	name = m.generateUniqueNameLocked(name)

	app := map[string]any{
		"id":      m.getNextIDLocked(),
		"name":    name,
		"file":    file,
		"type":    typ,
		"active":  true,
		"created": float64(time.Now().UnixMilli()),
	}
	m.apps = append(m.apps, app)
	m.saveAppsLocked()
	return app
}

// getNextIDLocked ports #getNextId. Caller holds cfg.View/Mutate.
func (m *Manager) getNextIDLocked() float64 {
	max := float64(-1)
	for _, app := range m.apps {
		if id, ok := app["id"].(float64); ok && id > max {
			max = id
		}
	}
	return max + 1
}

// generateUniqueNameLocked ports #generateUniqueName. Caller holds
// cfg.View/Mutate.
func (m *Manager) generateUniqueNameLocked(baseName string) string {
	name := baseName
	for counter := 1; m.getLocked(name) != nil; counter++ {
		name = baseName + "-" + itoa(counter)
	}
	return name
}

// ---- shared service shims (nil-tolerant Odac.server(...) lookups) ----

// SetHub fills the Hub seam after construction: the Hub client depends on
// the Manager (its command table calls App methods), so the two are wired
// in two steps — Node's DI registry resolved the same cycle lazily. Must be
// called before System.Init starts the tick (no lock: single-threaded
// startup wiring).
func (m *Manager) SetHub(hub Hub) { m.deps.Hub = hub }

func (m *Manager) hubTrigger(event string) {
	if m.deps.Hub != nil {
		m.deps.Hub.Trigger(event)
	}
}

func (m *Manager) proxySync() {
	if m.deps.Proxy != nil {
		m.deps.Proxy.SyncConfig()
	}
}

func (m *Manager) proxyPurge(appID any) {
	if m.deps.Proxy != nil {
		m.deps.Proxy.PurgeCacheForApp(appID)
	}
}

func res(status bool, message any, data ...any) *api.Result {
	r := api.Res(status, message, data...)
	return &r
}
