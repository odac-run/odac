//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setSocketOptions enables SO_REUSEPORT on Linux for zero-downtime updates.
// Allows the new mail binary to bind the same ports as the old instance
// during Blue-Green deployment overlap window.
func setSocketOptions(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
}
