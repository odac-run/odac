package dataplane

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
	"odac/internal/rundir"
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
type DNS struct {
	cfg    *config.Store
	log    *logx.Logger
	proc   process
	detect *ipDetector

	retryDelay time.Duration
	readyPoll  time.Duration
	clock      func() time.Time // SOA-serial test injection; nil = time.Now

	syncMu sync.Mutex

	mu     sync.Mutex
	active bool
	// spawnable is set once this Start's IP detection has finished, so the
	// check tick never spawns odac-dns with an empty address set. gen tells a
	// detection goroutine whether it still belongs to the current Start.
	spawnable bool
	gen       uint64
	ipv4      []IPEntry
	ipv6      []IPEntry
	primary   string
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
		Name:         "dns",
		Binary:       "odac-dns",
		SocketEnv:    "ODAC_DNS_SOCKET_PATH", // proxy uses ODAC_SOCKET_PATH — keep the asymmetry
		Display:      "DNS",
		BinDir:       binDir,
		RunDir:       rundir.Control(cfg.BaseDir()),
		LegacyRunDir: rundir.App(cfg.BaseDir()),
		Log:          d.log,
		OnSync:       d.SyncConfig,
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
	d.spawnable = false
	d.gen++
	gen := d.gen
	v4 := append([]IPEntry(nil), d.ipv4...)
	v6 := append([]IPEntry(nil), d.ipv6...)
	primary := d.primary
	d.mu.Unlock()

	go func() {
		v4, v6, primary = d.detect.run(d.log, v4, v6, primary)
		d.mu.Lock()
		if d.gen != gen { // a later Start owns detection now
			d.mu.Unlock()
			return
		}
		d.ipv4, d.ipv6, d.primary = v4, v6, primary
		active := d.active // stopped mid-detection → don't spawn
		d.spawnable = active
		d.mu.Unlock()
		if active {
			d.proc.Ensure()
		}
	}()
}

// Check runs on the 1s tick: respawn odac-dns if it is gone. Without it a
// crash left every zone ODAC serves unresolvable until the server restarted.
// The respawn's OnSync pushes the zones again.
func (d *DNS) Check() {
	d.mu.Lock()
	ready := d.active && d.spawnable
	d.mu.Unlock()
	if ready {
		d.proc.Ensure()
	}
}

// Stop ports stop(): SIGTERM the binary, unlink its socket.
func (d *DNS) Stop() {
	d.mu.Lock()
	d.active = false
	d.spawnable = false
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

	// Payload assembly and marshal run under the config value lock (the
	// zone maps are live and 3.3's handlers mutate them); the send happens
	// outside it.
	var body []byte
	var marshalErr error
	zoneCount := 0
	d.cfg.View(func() {
		zones := d.cfg.Get("dns")
		if !truthy(zones) {
			zones = map[string]any{}
		}
		v4, v6, primary := d.IPInfo()
		payload := map[string]any{
			"ips":   map[string]any{"ipv4": entries(v4), "ipv6": entries(v6), "primary": primary},
			"zones": zones,
		}
		if zm, ok := zones.(map[string]any); ok {
			zoneCount = len(zm)
		}
		body, marshalErr = json.Marshal(payload)
	})
	if marshalErr != nil {
		d.log.Error(fmt.Sprintf("Failed to sync DNS config to Go binary: %s", marshalErr))
		return
	}
	d.log.Log("DNS: Syncing %d zones to Go binary", zoneCount)

	if err := postRaw(sock, "/config", body); err != nil {
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
