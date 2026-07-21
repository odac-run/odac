//go:build !linux && !darwin

package sysinfo

// Non-unix builds (windows cross-build) compile with zeroed metrics; the
// production host is Linux and darwin covers development. Node's win32
// paths shelled out to netstat/wmic and are not worth porting for a
// never-run configuration.

func kernelRelease() string { return "" }

func memoryKB() (total, free, available int64) { return 0, 0, 0 }

func swapKB() (total, free int64) { return 0, 0 }

func loadAvg() (l1, l2, l3 float64) { return 0, 0, 0 }

func uptimeSeconds() float64 { return 0 }

func cpuTicks() (idle, total int64, ok bool) { return 0, 0, false }

func diskBytes() (total, free int64) { return 0, 0 }

func netStats() (recv, sent int64, ok bool) { return 0, 0, false }

func cpuModel() string { return "unknown" }
