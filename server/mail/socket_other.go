//go:build !linux

package main

import "syscall"

// setSocketOptions is a no-op on non-Linux platforms.
// SO_REUSEPORT is Linux-specific; macOS/Windows use standard binding.
func setSocketOptions(network, address string, c syscall.RawConn) error {
	return nil
}
