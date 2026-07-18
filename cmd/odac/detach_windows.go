//go:build windows

package main

import "syscall"

// detachAttr detaches the spawned watchdog from the CLI's console.
func detachAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	const detachedProcess = 0x00000008
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
