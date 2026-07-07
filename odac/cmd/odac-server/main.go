// odac-server is the Go orchestrator (Phase 3 of the Node→Go migration; the
// counterpart of node server/index.js). Until task 3.8 flips the cutover it
// is built and tested but never wired into bin/odac or the Docker image —
// the Node orchestrator keeps running production.
//
// Lifecycle notes (parity with Node, see contracts/lifecycle.md):
//   - No signal handlers, like Node: SIGTERM/SIGINT kill the process; the
//     watchdog treats any non-zero exit as a crash and restarts. Config loss
//     is bounded by the 500ms autosave; explicit ForceSave happens only in
//     the update handover (task 3.7).
//   - No panic recovery: Node's uncaughtException handler logs and exits 1.
//     A Go panic prints its stack and exits 2 — same watchdog outcome.
//     Node's global ECONNRESET/EPIPE swallowing needs no equivalent: those
//     surface as ordinary error returns at their call sites in Go.
package main

import (
	"os"
	"path/filepath"
	"time"

	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/logx"
	"odac/internal/system"
)

func main() {
	log := logx.New("Server")

	baseDir := config.DefaultBaseDir()
	cfg, err := config.Open(baseDir)
	if err != nil {
		log.Error("Config initialization failed:", err.Error())
		os.Exit(1)
	}

	// Server-side autosave per contract 0.6; the stop function is unused —
	// autosave runs for the life of the process, exactly like Node's
	// unref'd 500ms interval.
	cfg.AutoSave(500*time.Millisecond, func(err error) {
		log.Error("Config autosave failed:", err.Error())
	})

	binDir := moduleBinDir()
	dnsSvc := dataplane.NewDNS(cfg, binDir)
	proxySvc := dataplane.NewProxy(cfg, binDir, nil) // container IP resolver lands with 3.4
	mailSvc := dataplane.NewMail(cfg, binDir, dnsSvc)

	sys := system.New(cfg, system.Services{
		Proxy: proxySvc,
		DNS:   dnsSvc,
		Mail:  mailSvc,
		// Remaining slots fill in as migration tasks land:
		// Api (3.3), App (3.4), SSL (3.5), Hub (3.6).
	}, system.NewStartupGate(baseDir))

	if err := sys.Init(); err != nil {
		log.Error("System initialization failed:", err.Error())
		os.Exit(1)
	}
	log.Log("Odac server started")

	select {} // run until the watchdog (or a signal) kills us
}

// moduleBinDir locates the data-plane binaries (odac-proxy/dns/mail). They
// ship next to this executable in bin/ (Node resolved the same directory as
// ../../bin relative to server/src). ODAC_BIN_DIR overrides for tests only.
func moduleBinDir() string {
	if dir := os.Getenv("ODAC_BIN_DIR"); dir != "" {
		return dir
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
