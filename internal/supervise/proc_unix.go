//go:build !windows

package supervise

import "syscall"

// pidAlive is Node's process.kill(pid, 0): only a clean signal-0 counts.
// EPERM (exists, not ours) is treated as not adoptable, same as Node's throw.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// terminate is Node's process.kill(pid) — SIGTERM, errors ignored.
func terminate(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
}

// detachAttr detaches the child from our session so it survives orchestrator
// restarts (Node: spawn {detached: true} + unref; same pattern as cmd/odac).
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
