//go:build !windows

package main

import "syscall"

// detachAttr detaches the spawned watchdog from the CLI's session so it
// survives the CLI exiting (Node: spawn {detached: true} + unref).
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
