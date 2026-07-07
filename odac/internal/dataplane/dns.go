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

// IPEntry is one detected address in the payload shape shared by the dns and
// mail control APIs. PTR is "" when unknown — Node normalizes null → ""
// before sending, so the Go services simply never hold a null.
type IPEntry struct {
	Address string `json:"address"`
	PTR     string `json:"ptr"`
	Public  bool   `json:"public"`
}

// IPSource is what Mail borrows from DNS: the detected address set.
type IPSource interface {
	IPInfo() (ipv4, ipv6 []IPEntry, primary string)
}

// DNS is the Go port of server/src/DNS.js's process-management half:
// supervise bin/odac-dns, detect the host's IPs, push zones + IPs per
// contracts/dns-control.md. Zone CRUD, SOA serials and the dns.list API stay
// with the command handlers (task 3.3), as the contract assigns them.
//
// Note there is deliberately no Check method: the Node 1s tick never checks
// DNS (System.js omits it), so a crashed odac-dns stays down until the
// orchestrator restarts — Node parity, revisit only with evidence.
type DNS struct {
	cfg    *config.Store
	log    *logx.Logger
	proc   process
	detect *ipDetector

	retryDelay time.Duration
	readyPoll  time.Duration

	syncMu sync.Mutex

	mu      sync.Mutex
	active  bool
	ipv4    []IPEntry
	ipv6    []IPEntry
	primary string
}

// NewDNS wires the service.
func NewDNS(cfg *config.Store, binDir string) *DNS {
	d := &DNS{
		cfg:        cfg,
		log:        logx.New("DNS"),
		detect:     newIPDetector(),
		retryDelay: time.Second,
		readyPoll:  200 * time.Millisecond,
		primary:    "127.0.0.1",
	}
	d.proc = supervise.New(supervise.Options{
		Name:      "dns",
		Binary:    "odac-dns",
		SocketEnv: "ODAC_DNS_SOCKET_PATH", // proxy uses ODAC_SOCKET_PATH — keep the asymmetry
		Display:   "DNS",
		BinDir:    binDir,
		RunDir:    filepath.Join(cfg.BaseDir(), "run"),
		Log:       d.log,
		OnSync:    d.SyncConfig,
	})
	return d
}

// Start ports DNS.start(): detect IPs, then spawn. Node's start() is async
// and System.init() does not await it, so detection must not hold up the
// other services here either — it runs in a goroutine, the spawn follows.
func (d *DNS) Start() {
	d.mu.Lock()
	if d.active {
		d.mu.Unlock()
		return
	}
	d.active = true
	v4 := append([]IPEntry(nil), d.ipv4...)
	v6 := append([]IPEntry(nil), d.ipv6...)
	primary := d.primary
	d.mu.Unlock()

	go func() {
		v4, v6, primary = d.detect.run(d.log, v4, v6, primary)
		d.mu.Lock()
		d.ipv4, d.ipv6, d.primary = v4, v6, primary
		active := d.active
		d.mu.Unlock()
		if active { // stopped mid-detection → don't spawn
			d.proc.Ensure()
		}
	}()
}

// Stop ports stop(): SIGTERM the binary, unlink its socket.
func (d *DNS) Stop() {
	d.mu.Lock()
	d.active = false
	d.mu.Unlock()
	d.proc.Stop()
}

// IPInfo returns copies of the detected addresses (Mail's payload borrows
// them; task 3.3's dynamic-record resolution will too).
func (d *DNS) IPInfo() (ipv4, ipv6 []IPEntry, primary string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]IPEntry(nil), d.ipv4...), append([]IPEntry(nil), d.ipv6...), d.primary
}

// WaitForReady ports waitForReady(): poll /ready every 200ms until UDP:53 and
// TCP:53 are bound, then push zones immediately. Used by the updater (3.7).
func (d *DNS) WaitForReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.proc.Running() {
			sock := d.proc.SocketPath()
			if _, err := os.Stat(sock); err == nil {
				if status, err := getStatus(sock, "/ready", time.Second); err == nil && status == 200 {
					d.SyncConfig()
					return true
				}
			}
		}
		time.Sleep(d.readyPoll)
	}
	return false
}

// SyncConfig pushes zones + IPs (DNS.js syncConfig): config.dns verbatim,
// full replace, atomically swapped in the resolver.
func (d *DNS) SyncConfig() {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.syncConfig(0)
}

func (d *DNS) syncConfig(retry int) {
	d.log.Log("DNS: syncConfig called (Retry: %d)", retry)
	if !d.proc.Running() {
		return
	}
	sock := d.proc.SocketPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}

	zones := d.cfg.Get("dns")
	if !truthy(zones) {
		zones = map[string]any{}
	}
	v4, v6, primary := d.IPInfo()
	payload := map[string]any{
		"ips":   map[string]any{"ipv4": entries(v4), "ipv6": entries(v6), "primary": primary},
		"zones": zones,
	}

	zoneCount := 0
	if zm, ok := zones.(map[string]any); ok {
		zoneCount = len(zm)
	}
	d.log.Log("DNS: Syncing %d zones to Go binary", zoneCount)

	if err := postJSON(sock, "/config", payload); err != nil {
		if retry < syncRetries && retryable(err) {
			d.log.Log(fmt.Sprintf("DNS config sync failed (%s). Retrying in 1s...", errCode(err)))
			time.Sleep(d.retryDelay)
			d.syncConfig(retry + 1)
			return
		}
		d.log.Error(fmt.Sprintf("Failed to sync DNS config to Go binary: %s", err))
	}
}

// entries returns a non-nil slice so JSON encodes [] rather than null.
func entries(set []IPEntry) []IPEntry {
	if set == nil {
		return []IPEntry{}
	}
	return set
}
