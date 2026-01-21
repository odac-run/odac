//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func setSocketOptions(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		// Use unix package for SO_REUSEPORT which is more reliable across newer Go versions/Linux kernels
		// unix.SOL_SOCKET and unix.SO_REUSEPORT are the correct constants
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
}
