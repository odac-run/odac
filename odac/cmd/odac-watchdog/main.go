// odac-watchdog supervises the ODAC server process (Go port of
// watchdog/index.js — see docs/migration/contracts/lifecycle.md).
//
// During the migration the supervised server is still the Node orchestrator
// (`node <repo>/server/index.js`); once cmd/odac-server exists, the command
// below switches to it (task 3.x).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"odac/internal/config"
	"odac/internal/watchdog"
)

func main() {
	cfg, err := config.Open(config.DefaultBaseDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to load config:", err)
		os.Exit(1)
	}

	script := os.Getenv("ODAC_SERVER_SCRIPT")
	if script == "" {
		script = defaultServerScript()
	}

	watchdog.New(cfg, []string{"node", script}).Run()
}

// defaultServerScript resolves <repo>/server/index.js relative to this binary,
// which lives in <repo>/bin like the data-plane binaries.
func defaultServerScript() string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		return filepath.Join(filepath.Dir(exe), "..", "server", "index.js")
	}
	return filepath.Join("server", "index.js")
}
