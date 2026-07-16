//go:build !linux

package netutil

import "syscall"

// SetSocketOptions is a no-op on non-Linux platforms (SO_REUSEPORT is
// Linux-specific; dev builds bind normally).
func SetSocketOptions(network, address string, c syscall.RawConn) error {
	return nil
}
