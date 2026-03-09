//go:build !linux

package main

import "syscall"

// setSocketOptions is a no-op on non-Linux platforms.
// SO_REUSEPORT is Linux-specific; macOS and Windows don't need it
// for the DNS server's use case.
func setSocketOptions(network, address string, c syscall.RawConn) error {
	return nil
}
