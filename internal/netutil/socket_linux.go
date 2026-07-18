//go:build linux

package netutil

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// SetSocketOptions is a net.ListenConfig Control hook that enables
// SO_REUSEPORT, so a replacement binary can bind the same port while the
// old instance still holds it (zero-downtime restart overlap).
func SetSocketOptions(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		// Use unix package for SO_REUSEPORT which is more reliable across newer Go versions/Linux kernels
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
}
