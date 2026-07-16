package sysinfo

import (
	"encoding/binary"
	"time"

	"golang.org/x/sys/unix"
)

func kernelRelease() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// memoryKB: total via hw.memsize; free is not exposed by sysctl without
// mach host_statistics — reported as 0 (development platform; see the
// package comment).
func memoryKB() (total, free int64) {
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		total = int64(mem / 1024)
	}
	return total, 0
}

// loadAvg reads vm.loadavg: struct loadavg { fixpt_t ldavg[3]; long fscale }.
func loadAvg() (l1, l2, l3 float64) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0, 0, 0
	}
	scale := float64(binary.LittleEndian.Uint64(raw[16:24]))
	if scale == 0 {
		return 0, 0, 0
	}
	l1 = float64(binary.LittleEndian.Uint32(raw[0:4])) / scale
	l2 = float64(binary.LittleEndian.Uint32(raw[4:8])) / scale
	l3 = float64(binary.LittleEndian.Uint32(raw[8:12])) / scale
	return l1, l2, l3
}

func uptimeSeconds() float64 {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0
	}
	boot := time.Unix(tv.Sec, int64(tv.Usec)*1000)
	return time.Since(boot).Seconds()
}

func cpuModel() string {
	if model, err := unix.Sysctl("machdep.cpu.brand_string"); err == nil && model != "" {
		return model
	}
	return "unknown"
}
