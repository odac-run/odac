//go:build !windows

package appmgr

import "syscall"

// pidAlive is Node's process.kill(pid, 0) (same helper as supervise's).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// pidTerminate is Node's process.kill(pid) — SIGTERM, ESRCH swallowed by the
// caller.
func pidTerminate(pid int) error {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
