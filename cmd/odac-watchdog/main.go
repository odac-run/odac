// odac-watchdog supervises the ODAC server process (Go port of
// watchdog/index.js — see docs/migration/contracts/lifecycle.md).
//
// The supervised server is chosen at startup: an `odac-server` binary next to
// this one wins (the Go orchestrator — how a 3.8 staging image flips the
// cutover: same image plus that single binary); otherwise the Node
// orchestrator (`node <repo>/server/index.js`) runs as before.
// ODAC_SERVER_SCRIPT forces a specific Node entrypoint and skips the probe.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"odac/internal/config"
	"odac/internal/watchdog"
)

func main() {
	cfg, err := config.Open(config.DefaultBaseDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to load config:", err)
		os.Exit(1)
	}

	watchdog.New(cfg, serverCommand()).Run()
}

// serverCommand resolves what to supervise: an explicit Node entrypoint via
// ODAC_SERVER_SCRIPT, else a sibling Go odac-server binary, else the default
// Node entrypoint.
func serverCommand() []string {
	if script := os.Getenv("ODAC_SERVER_SCRIPT"); script != "" {
		return []string{"node", script}
	}
	if bin := siblingGoServer(); bin != "" {
		return []string{bin}
	}
	return []string{"node", defaultServerScript()}
}

// siblingGoServer returns the path of an executable odac-server binary in
// this binary's own directory, or "" when absent.
func siblingGoServer() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	name := "odac-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(filepath.Dir(exe), name)
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		return ""
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return bin
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
