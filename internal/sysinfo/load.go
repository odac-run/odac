package sysinfo

import "runtime"

// Load is the host's memory and load at one moment. Backups size their
// helpers from it and postpone a run on a host that is already busy.
type Load struct {
	MemTotal     int64
	MemAvailable int64
	Load1        float64
	Cores        int
}

// HostLoad reads the host's memory (bytes) and its one-minute load
// average. Platforms without the collectors report zero memory, which
// callers read as unknown.
func HostLoad() Load {
	total, _, avail := memoryKB()
	l1, _, _ := loadAvg()
	return Load{MemTotal: total * 1024, MemAvailable: avail * 1024, Load1: l1, Cores: runtime.NumCPU()}
}
