//go:build windows

package supervise

import (
	"os"
	"syscall"
)

const stillActive = 259 // STILL_ACTIVE

// pidAlive checks liveness via OpenProcess + GetExitCodeProcess (Windows has
// no signal 0).
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

// terminate hard-kills pid (Node's process.kill on Windows is also
// TerminateProcess).
func terminate(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}

// detachAttr detaches the child from our console (same pattern as cmd/odac).
func detachAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	const detachedProcess = 0x00000008
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
