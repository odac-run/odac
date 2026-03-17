//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setSocketOptions enables SO_REUSEPORT on Linux for the DNS UDP/TCP listeners.
// This allows multiple server instances to bind to the same port, enabling
// seamless restarts and load distribution across CPU cores.
func setSocketOptions(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
}
