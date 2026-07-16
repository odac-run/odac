//go:build !linux

package dataplane

// memInfo returns zeros on non-Linux platforms; the proxy's cache manager
// ignores a zero total and keeps its defaults (UpdateMemory early-returns).
// Production orchestrators run on Linux — parity where it matters.
func memInfo() (total, used uint64) { return 0, 0 }
