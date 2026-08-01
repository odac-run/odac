//go:build windows

package appmgr

import (
	"os"
	"syscall"
)

const stillActive = 259 // STILL_ACTIVE

// pidAlive checks liveness via OpenProcess + GetExitCodeProcess (same helper
// as supervise's — Windows has no signal 0).
func pidAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// pidTerminate is Node's process.kill(pid) (TerminateProcess on Windows); a
// missing process is not an error.
func pidTerminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
}
