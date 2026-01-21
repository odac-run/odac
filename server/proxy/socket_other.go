//go:build !linux

package main

import "syscall"

func setSocketOptions(network, address string, c syscall.RawConn) error {
	// No-op for non-Linux platforms
	return nil
}
