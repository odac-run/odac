package dataplane

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
	"odac/internal/supervise"
)

// Mail is the Go port of server/src/Mail.js's process-management half:
// supervise bin/odac-mail and push domain/TLS/IP config per
// contracts/mail-control.md. Account CRUD, send() and DKIM generation stay
// with the command handlers (task 3.3), as the contract assigns them.
//
// Mail has no /ready endpoint and no waitForReady — it takes no part in the
// SO_REUSEPORT overlap handover; System.stop() always stops it.
type Mail struct {
	cfg  *config.Store
	log  *logx.Logger
	proc process
	ips  IPSource // the DNS service; nil-safe

	retryDelay time.Duration

	syncMu sync.Mutex

	mu     sync.Mutex
	active bool
}

// NewMail wires the service; ips is normally the *DNS service.
func NewMail(cfg *config.Store, binDir string, ips IPSource) *Mail {
	m := &Mail{
		cfg:        cfg,
		log:        logx.New("Mail"),
		ips:        ips,
		retryDelay: time.Second,
	}
	m.proc = supervise.New(supervise.Options{
		Name:      "mail",
		Binary:    "odac-mail",
		SocketEnv: "ODAC_MAIL_SOCKET_PATH",
		Display:   "Mail",
		BinDir:    binDir,
		RunDir:    filepath.Join(cfg.BaseDir(), "run"),
		Log:       m.log,
		OnSync:    m.SyncConfig,
	})
	return m
}

// Start ports start(): spawn/adopt the binary.
func (m *Mail) Start() {
	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		return
	}
	m.active = true
	m.mu.Unlock()
	m.proc.Ensure()
}

// Stop ports stop(): SIGTERM the binary, unlink its socket, cancel timers.
func (m *Mail) Stop() {
	m.mu.Lock()
	m.active = false
	m.mu.Unlock()
	m.proc.Stop()
}

// Check runs on the 1s tick: respawn the binary if it is gone. Node's
// check() also drives DKIM key generation; that ports together with the mail
// command handlers in task 3.3 (it needs DNS record CRUD, which lands there).
func (m *Mail) Check() {
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active {
		m.proc.Ensure()
	}
}

// SyncConfig pushes domains/TLS/IP data (Mail.js syncConfig).
func (m *Mail) SyncConfig() {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.syncConfig(0)
}

func (m *Mail) syncConfig(retry int) {
	m.log.Log("Mail: syncConfig called (Retry: %d)", retry)
	if !m.proc.Running() {
		return
	}
	sock := m.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}

	dnsZones := m.cfg.Map("dns")
	mailDomains := map[string]any{}
	for name, rec := range m.cfg.Map("domains") {
		record, _ := rec.(map[string]any) // nil map reads as absent keys below
		mailDomains[name] = map[string]any{
			"cert":       orMap(record["cert"]),
			"mxEnabled":  zoneHasMX(dnsZones[name]), // exact-name zone only
			"subdomains": orList(record["subdomain"]),
		}
	}

	var v4, v6 []IPEntry
	primary := "127.0.0.1"
	if m.ips != nil {
		var p string
		v4, v6, p = m.ips.IPInfo()
		if p != "" {
			primary = p
		}
	}

	hostname, _ := os.Hostname()
	payload := map[string]any{
		"accounts": []any{}, // reserved; accounts live in the Go binary's SQLite
		"domains":  mailDomains,
		"hostname": hostname,
		"ips":      map[string]any{"ipv4": entries(v4), "ipv6": entries(v6), "primary": primary},
		"ssl":      orMap(m.cfg.Get("ssl")),
	}

	m.log.Log("Mail: Syncing %d domains to Go binary", len(mailDomains))

	if err := postJSON(sock, "/config", payload); err != nil {
		if retry < syncRetries && retryable(err) {
			m.log.Log(fmt.Sprintf("Mail config sync failed (%s). Retrying in 1s...", errCode(err)))
			time.Sleep(m.retryDelay)
			m.syncConfig(retry + 1)
			return
		}
		m.log.Error(fmt.Sprintf("Failed to sync Mail config to Go binary: %s", err))
	}
}

// zoneHasMX ports zone?.records?.some(r => r.type === 'MX').
func zoneHasMX(zone any) bool {
	zm, _ := zone.(map[string]any)
	records, _ := zm["records"].([]any)
	for _, r := range records {
		rm, _ := r.(map[string]any)
		if rm != nil && rm["type"] == "MX" {
			return true
		}
	}
	return false
}
