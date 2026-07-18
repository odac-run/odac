//go:build !linux && !darwin

package sysinfo

// Non-unix builds (windows cross-build) compile with zeroed metrics; the
// production host is Linux and darwin covers development. Node's win32
// paths shelled out to netstat/wmic and are not worth porting for a
// never-run configuration.

func kernelRelease() string { return "" }

func memoryKB() (total, free int64) { return 0, 0 }

func loadAvg() (l1, l2, l3 float64) { return 0, 0, 0 }

func uptimeSeconds() float64 { return 0 }

func cpuModel() string { return "unknown" }
