// Boot-on-demand, ported from Cli.boot(): when the server is unreachable the
// CLI starts the watchdog detached and reloads config after a grace second.
// Binary resolution mirrors bin/odac's watchdog branch (task 1.2): the Go
// watchdog next to this executable wins, the Node watchdog is the fallback.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// defaultBoot is installed as app.boot by main; tests stub the field.
// Mirrors Node's `booting` guard: only the first call in a process acts.
func (a *app) defaultBoot() {
	if a.booted {
		return
	}
	a.booted = true

	fmt.Fprintln(a.out, __("Starting Odac Server..."))
	cmd := watchdogCommand()
	if cmd == nil {
		fmt.Fprintln(a.errOut, "No watchdog found (looked for odac-watchdog next to this binary and the Node watchdog).")
		return
	}
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(a.errOut, "Failed to start watchdog:", err)
		return
	}
	cmd.Process.Release()

	time.Sleep(time.Second)
	a.cfg.Reload()
}

func watchdogCommand() *exec.Cmd {
	if env := os.Getenv("ODAC_WATCHDOG_BIN"); env != "" {
		return exec.Command(env)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)

	name := "odac-watchdog"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if goBin := filepath.Join(dir, name); fileExists(goBin) {
		return exec.Command(goBin)
	}
	if script := filepath.Join(dir, "..", "watchdog", "index.js"); fileExists(script) {
		return exec.Command("node", script)
	}
	return nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
