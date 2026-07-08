package dataplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
	"odac/internal/supervise"
)

// DNSService is what Mail borrows from the DNS service: the detected
// address set for the config payload and record creation for publishing
// DKIM TXT keys (Node reads both off Odac.server('DNS')).
type DNSService interface {
	IPSource
	Record(args ...map[string]any)
}

// Mail is the Go port of server/src/Mail.js: supervise bin/odac-mail, push
// domain/TLS/IP config per contracts/mail-control.md, and run the
// orchestrator-side command logic (account CRUD pass-through, send()
// message assembly, DKIM generation on the check tick — mailcmd.go).
//
// Mail has no /ready endpoint and no waitForReady — it takes no part in the
// SO_REUSEPORT overlap handover; System.stop() always stops it.
type Mail struct {
	cfg  *config.Store
	log  *logx.Logger
	proc process
	dns  DNSService // the DNS service; nil-safe

	retryDelay time.Duration

	syncMu sync.Mutex

	mu       sync.Mutex
	active   bool
	checking bool // Node's #checking re-entrance guard for the DKIM scan
}

// NewMail wires the service; dns is normally the *DNS service.
func NewMail(cfg *config.Store, binDir string, dns DNSService) *Mail {
	m := &Mail{
		cfg:        cfg,
		log:        logx.New("Mail"),
		dns:        dns,
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

// Check runs on the 1s tick, porting Mail.check(): respawn the binary if it
// is gone, then generate DKIM keys for MX-enabled domains that lack them.
// The checking flag is Node's #checking guard — overlapping ticks skip the
// whole check while a DKIM pass is in flight. The respawn stays synchronous
// (Node's spawnMail runs before the first await); the DKIM scan runs in a
// goroutine, like the un-awaited async check() Node's tick fires.
func (m *Mail) Check() {
	m.mu.Lock()
	if m.checking || !m.active {
		m.mu.Unlock()
		return
	}
	m.checking = true
	m.mu.Unlock()

	m.proc.Ensure()

	go func() {
		defer func() {
			m.mu.Lock()
			m.checking = false
			m.mu.Unlock()
		}()
		m.dkimCheck()
	}()
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

	// Payload assembly and marshal run under the config value lock (the
	// domain/zone/ssl maps are live and 3.3's handlers mutate them); the
	// send happens outside it.
	var body []byte
	var marshalErr error
	domainCount := 0
	m.cfg.View(func() {
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
		if m.dns != nil {
			var p string
			v4, v6, p = m.dns.IPInfo()
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
		domainCount = len(mailDomains)
		body, marshalErr = json.Marshal(payload)
	})
	if marshalErr != nil {
		m.log.Error(fmt.Sprintf("Failed to sync Mail config to Go binary: %s", marshalErr))
		return
	}

	m.log.Log("Mail: Syncing %d domains to Go binary", domainCount)

	if err := postRaw(sock, "/config", body); err != nil {
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
