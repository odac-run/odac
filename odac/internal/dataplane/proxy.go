package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
	"odac/internal/ports"
	"odac/internal/supervise"
)

// ContainerIPs resolves container IPs for container-network apps. The real
// implementation lands with the Container port (task 3.4); a nil resolver
// falls back to each app's cached ip, exactly like Node when Docker is
// unreachable.
type ContainerIPs interface {
	// Available reports whether Docker is reachable (Container.available).
	Available() bool
	// GetIP resolves a container name or ID to its network IP.
	GetIP(nameOrID string) (string, error)
}

// Tunnel is one Hub-managed tunnel route.
type Tunnel struct {
	Domain    string
	Container string
	Token     string
}

// Proxy is the Go port of server/src/Proxy.js: supervises bin/odac-proxy and
// pushes routing/firewall/tunnel config per contracts/proxy-control.md.
type Proxy struct {
	cfg        *config.Store
	log        *logx.Logger
	proc       process
	containers ContainerIPs

	retryDelay time.Duration // sync retry spacing (1s)
	readyPoll  time.Duration // waitForReady poll (200ms)
	dockerWait time.Duration // max Docker wait on first sync (3s)
	dockerPoll time.Duration // Docker availability poll (100ms)

	syncMu sync.Mutex // serializes config pushes (Node's event loop did)

	mu      sync.Mutex
	active  bool
	tunnels map[string]Tunnel // by domain; full-replace semantics
}

// NewProxy wires the service. containers may be nil until task 3.4.
func NewProxy(cfg *config.Store, binDir string, containers ContainerIPs) *Proxy {
	p := &Proxy{
		cfg:        cfg,
		log:        logx.New("Proxy"),
		containers: containers,
		retryDelay: time.Second,
		readyPoll:  200 * time.Millisecond,
		dockerWait: 3 * time.Second,
		dockerPoll: 100 * time.Millisecond,
		tunnels:    map[string]Tunnel{},
	}
	p.proc = supervise.New(supervise.Options{
		Name:      "proxy",
		Binary:    "odac-proxy",
		SocketEnv: "ODAC_SOCKET_PATH",
		Display:   "Proxy",
		BinDir:    binDir,
		RunDir:    filepath.Join(cfg.BaseDir(), "run"),
		Log:       p.log,
		OnSync:    p.SyncConfig,
	})
	return p
}

// Start ports start(): restore persisted tunnels, then spawn/adopt.
func (p *Proxy) Start() {
	p.mu.Lock()
	p.active = true
	for domain, v := range p.cfg.Map("tunnels") {
		val, _ := v.(map[string]any)
		if val != nil && truthy(val["token"]) && truthy(val["container"]) {
			p.tunnels[domain] = Tunnel{Domain: domain, Container: str(val["container"]), Token: str(val["token"])}
		}
	}
	restored := len(p.tunnels)
	p.mu.Unlock()
	if restored > 0 {
		p.log.Log("Restored %d tunnel(s) from config", restored)
	}
	p.proc.Ensure()
}

// Stop ports stop(): SIGTERM the binary, unlink its socket, cancel timers.
func (p *Proxy) Stop() {
	p.mu.Lock()
	p.active = false
	p.mu.Unlock()
	p.proc.Stop()
}

// Check runs on the 1s tick: respawn the binary if it is gone.
func (p *Proxy) Check() {
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if active {
		p.proc.Ensure()
	}
}

// PurgeCache ports purgeCache(): flush the proxy's static-asset cache via
// POST /cache/purge (contract 0.3) — whole cache when domain is empty.
// Returns the purged entry count; transport errors log and return 0 like
// Node's catch.
func (p *Proxy) PurgeCache(domain string) int {
	sock := p.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return 0 // proxy not running / socket gone (Node: !#proxyProcess)
	}

	payload := map[string]any{}
	if domain != "" {
		payload["domain"] = domain
	}

	envelope, err := requestJSON(sock, "POST", "/cache/purge", payload)
	if err != nil {
		p.log.Error("Failed to purge cache: %s", err.Error())
		return 0
	}
	purged := 0
	if n, ok := envelope["purged"].(float64); ok {
		purged = int(n)
	}
	if purged > 0 {
		suffix := ""
		if domain != "" {
			suffix = " for " + domain
		}
		p.log.Log("Cache purged: " + fmt.Sprintf("%d", purged) + " entries" + suffix)
	}
	return purged
}

// PurgeCacheForApp ports purgeCacheForApp(): purge every domain mapped to
// the app (matched by name or id, whichever config.domains recorded).
// Deviation from Node (deliberate): Node's `if (!appId) return` also skips
// the numeric id 0 — the FIRST app ever created (ids start at 0) never got
// its cache purged. Only nil/"" no-op here.
func (p *Proxy) PurgeCacheForApp(appID any) {
	if appID == nil || appID == "" {
		return
	}

	var targets []string
	p.cfg.View(func() {
		domains, _ := p.cfg.Get("domains").(map[string]any)
		for domainName, rec := range domains {
			record, _ := rec.(map[string]any)
			if record != nil && record["appId"] == appID {
				targets = append(targets, domainName)
			}
		}
	})

	sort.Strings(targets) // map order is random; Node walked insertion order
	for _, domain := range targets {
		p.PurgeCache(domain)
	}
}

// SetACMEChallenge ports setACMEChallenge(): push an HTTP-01 challenge token
// to the proxy via POST /acme/challenge (contract 0.3) so it answers
// /.well-known/acme-challenge/<token> on port 80. Unlike the other pushes
// this one PROPAGATES failure — the SSL module must fall back to DNS-01 when
// the proxy cannot serve the token (Node throws on !== 200 the same way).
func (p *Proxy) SetACMEChallenge(token, keyAuthorization string) error {
	if !p.proc.Running() {
		return errors.New("Proxy process not running")
	}
	sock := p.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return errors.New("Proxy API not available")
	}

	status, err := requestStatus(sock, "POST", "/acme/challenge",
		map[string]any{"keyAuthorization": keyAuthorization, "token": token})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Proxy returned HTTP %d for ACME challenge", status)
	}
	return nil
}

// DeleteACMEChallenge ports deleteACMEChallenge(): best-effort removal of a
// served HTTP-01 token via DELETE /acme/challenge. Failures only log — the
// token expires with the proxy's own TTL anyway.
func (p *Proxy) DeleteACMEChallenge(token string) {
	if !p.proc.Running() {
		return
	}
	sock := p.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}
	if _, err := requestStatus(sock, "DELETE", "/acme/challenge", map[string]any{"token": token}); err != nil {
		p.log.Error("Failed to delete ACME challenge token: %s", err.Error())
	}
}

// SetTunnels ports setTunnels(): the Hub always sends the complete list, so
// this is a full replace — missing entries are deletions. Persists
// config.tunnels and syncs immediately. Returns the configured count; the
// API-result envelope around it lands with tasks 3.3/3.6.
func (p *Proxy) SetTunnels(tunnels []Tunnel) int {
	incoming := map[string]Tunnel{}
	persist := map[string]any{}
	for _, t := range tunnels {
		if t.Domain == "" || t.Token == "" || t.Container == "" {
			continue
		}
		incoming[t.Domain] = t
		persist[t.Domain] = map[string]any{"container": t.Container, "token": t.Token}
	}

	p.mu.Lock()
	p.tunnels = incoming
	p.mu.Unlock()
	p.cfg.Set("tunnels", persist)

	p.log.Log("Tunnel config replaced: %d tunnel(s)", len(incoming))
	p.SyncConfig()
	return len(incoming)
}

// WaitForReady ports waitForReady(): poll /ready every 200ms until both :80
// and :443 are bound, then push config immediately so the binary routes with
// current state the moment it serves. Used by the updater handshake (3.7).
func (p *Proxy) WaitForReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.proc.Running() {
			sock := p.proc.SocketPath()
			if _, err := os.Stat(sock); err == nil {
				if status, err := getStatus(sock, "/ready", time.Second); err == nil && status == 200 {
					p.SyncConfig()
					return true
				}
			}
		}
		time.Sleep(p.readyPoll)
	}
	return false
}

// SyncConfig pushes the full routing config (Proxy.js syncConfig).
func (p *Proxy) SyncConfig() {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	p.syncConfig(0)
}

func (p *Proxy) syncConfig(retry int) {
	p.log.Log("Proxy: syncConfig called (Retry: %d)", retry)
	if !p.proc.Running() {
		return
	}
	sock := p.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return // socket not ready yet
	}

	p.dockerWaitFirstSync(retry)

	// Payload assembly and marshal run under the config value WRITE lock:
	// resolveBackend caches container IPs back onto the live app entries
	// (app.ip), so this is a mutation, not just a read. The send happens
	// outside the lock.
	var body []byte
	var marshalErr error
	p.cfg.Mutate(func() {
		body, marshalErr = json.Marshal(p.buildPayload())
	})
	if marshalErr != nil {
		p.log.Error(fmt.Sprintf("Failed to sync config to proxy: %s", marshalErr))
		return
	}
	if err := postRaw(sock, "/config", body); err != nil {
		if retry < syncRetries && retryable(err) {
			p.log.Log(fmt.Sprintf("Config sync failed (%s). Retrying in 1s...", errCode(err)))
			time.Sleep(p.retryDelay)
			p.syncConfig(retry + 1)
			return
		}
		p.log.Error(fmt.Sprintf("Failed to sync config to proxy: %s", err))
	}
}

// dockerWaitFirstSync ports the boot guard: with container-network apps
// present, the first attempt waits ≤3s for Docker so backends don't fall
// back to 127.0.0.1. Runs before the value lock — it sleeps.
func (p *Proxy) dockerWaitFirstSync(retry int) {
	if retry != 0 || p.containers == nil {
		return
	}
	hasContainer := false
	p.cfg.View(func() {
		apps, _ := p.cfg.Get("apps").([]any)
		hasContainer = hasContainerApps(apps)
	})
	if !hasContainer {
		return
	}
	deadline := time.Now().Add(p.dockerWait)
	for !p.containers.Available() && time.Now().Before(deadline) {
		time.Sleep(p.dockerPoll)
	}
}

// buildPayload assembles the /config body. Caller holds cfg.Mutate (see
// syncConfig).
func (p *Proxy) buildPayload() map[string]any {
	domains := p.cfg.Map("domains")
	apps, _ := p.cfg.Get("apps").([]any)

	proxyDomains := map[string]any{}
	for name, rec := range domains {
		record, _ := rec.(map[string]any)
		if record == nil {
			continue
		}
		app := findApp(apps, record["appId"])
		if app == nil {
			p.log.Log("Proxy: App %s not found for domain %s", record["appId"], name)
			continue
		}
		backend := p.resolveBackend(app)
		if backend == nil {
			p.log.Log("Proxy: No port found for app %s (domain: %s)", app["name"], name)
			continue
		}

		target := "Host Network"
		if backend.internal {
			target = "Container Network"
		}
		p.log.Log("Proxy: Routing domain [%s] -> App [%s] via %s (%s:%d)", name, app["name"], target, backend.host, backend.port)

		entry := map[string]any{
			"domain":      name,
			"port":        backend.port,
			"subdomain":   orList(record["subdomain"]),
			"cert":        orMap(record["cert"]),
			"containerIP": backend.host,
		}
		if backend.internal {
			entry["container"] = backend.host
		}
		proxyDomains[name] = entry
	}

	tunnels := []any{}
	for _, tn := range p.tunnelList() {
		app := findAppByName(apps, tn.Container)
		if app == nil {
			p.log.Log(fmt.Sprintf("Tunnel: App not found for container %s (domain: %s)", tn.Container, tn.Domain))
			continue
		}
		backend := p.resolveBackend(app)
		if backend == nil {
			p.log.Log(fmt.Sprintf("Tunnel: No port found for %s (domain: %s)", tn.Container, tn.Domain))
			continue
		}
		tunnels = append(tunnels, map[string]any{
			"domain": tn.Domain, "host": backend.host, "port": backend.port, "token": tn.Token,
		})
	}

	p.log.Log("Proxy: Syncing %d domains, %d tunnels", len(proxyDomains), len(tunnels))

	total, used := memInfo()
	firewall := p.cfg.Get("firewall")
	if !truthy(firewall) {
		firewall = map[string]any{"enabled": true}
	}
	var ssl any // JSON null when unset (Node: config.ssl || null)
	if v := p.cfg.Get("ssl"); truthy(v) {
		ssl = v
	}

	return map[string]any{
		"domains":  proxyDomains,
		"firewall": firewall,
		"memory":   map[string]any{"total": total, "used": used},
		"ssl":      ssl,
		"tunnels":  tunnels,
	}
}

// tunnelList snapshots the tunnel map, sorted by domain for a deterministic
// payload (Node emitted Map insertion order; the binary treats the list as a
// set, so ordering is free to differ).
func (p *Proxy) tunnelList() []Tunnel {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Tunnel, 0, len(p.tunnels))
	for _, t := range p.tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

type backendInfo struct {
	host     string
	port     int
	internal bool
}

// resolveBackend ports #resolveAppBackend (dev 5a23c38/db7cdb2 semantics):
// Ports.primary picks the first proxy-routed entry (missing host or the
// 'proxy' sentinel), else ports[0]; a published primary routes to its host
// port, a proxy-routed one to its container port over the container network;
// then app.port → app.http; no port → nil, domain skipped. Container-network
// backends resolve the container IP, falling back to the cached app.ip.
func (p *Proxy) resolveBackend(app map[string]any) *backendInfo {
	port, host, internal := 0, "127.0.0.1", false

	portList, _ := app["ports"].([]any)
	if primary := ports.Primary(portList); primary != nil {
		if ports.IsPublished(primary) {
			port = jsParseInt(primary["host"])
		} else if truthy(primary["container"]) {
			port = jsParseInt(primary["container"])
			internal = true
		}
	} else if truthy(app["port"]) {
		port = jsParseInt(app["port"])
	} else if truthy(app["http"]) {
		port = jsParseInt(app["http"])
		internal = true
	}
	if port == 0 {
		return nil
	}

	if internal {
		target := app["activeContainerId"]
		if !truthy(target) {
			target = app["name"]
		}
		resolved := ""
		if p.containers != nil {
			if ip, err := p.containers.GetIP(str(target)); err == nil {
				resolved = ip
			}
		}
		if resolved != "" {
			// Cache like Node (app.ip = containerIP): the live config map is
			// mutated without marking the module dirty — it persists whenever
			// the app module is next saved for another reason.
			app["ip"] = resolved
			host = resolved
		} else if truthy(app["ip"]) {
			host = str(app["ip"])
		}
	}

	return &backendInfo{host: host, port: port, internal: internal}
}

// findApp ports apps.find(a => a.name === record.appId || a.id === record.appId).
func findApp(apps []any, appID any) map[string]any {
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app == nil {
			continue
		}
		if jsEqual(app["name"], appID) || jsEqual(app["id"], appID) {
			return app
		}
	}
	return nil
}

// findAppByName ports apps.find(a => a.name === val.container).
func findAppByName(apps []any, name string) map[string]any {
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app != nil && jsEqual(app["name"], name) {
			return app
		}
	}
	return nil
}

// hasContainerApps ports apps.some(a => a.ports?.some(p => p.container && Ports.isProxy(p))).
func hasContainerApps(apps []any) bool {
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app == nil {
			continue
		}
		portList, _ := app["ports"].([]any)
		for _, pp := range portList {
			pm, _ := pp.(map[string]any)
			if pm != nil && truthy(pm["container"]) && ports.IsProxy(pm) {
				return true
			}
		}
	}
	return false
}
