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

// memoryKB: total via hw.memsize; free/available are not exposed by sysctl
// without mach host_statistics — reported as 0 (development platform; see the
// package comment).
func memoryKB() (total, free, available int64) {
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		total = int64(mem / 1024)
	}
	return total, 0, 0
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

// cpuTicks: aggregate CPU times need mach host_statistics (HOST_CPU_LOAD_INFO),
// which is unreachable without cgo. Development platform — report unavailable
// so cpuUsage() degrades to 0 (see the package comment).
func cpuTicks() (idle, total int64, ok bool) { return 0, 0, false }

// diskBytes reports the root filesystem's total and available bytes via
// statfs; Bsize is uint32 on darwin, widened to match the linux collector.
func diskBytes() (total, free int64) {
	var st unix.Statfs_t
	if unix.Statfs("/", &st) != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs
}

// netStats: interface byte counters need getifaddrs/AF_LINK plumbing (Node
// shelled out to netstat -ib). Development platform — report unavailable so
// networkUsage() degrades to 0/0 (see the package comment).
func netStats() (recv, sent int64, ok bool) { return 0, 0, false }

func cpuModel() string {
	if model, err := unix.Sysctl("machdep.cpu.brand_string"); err == nil && model != "" {
		return model
	}
	return "unknown"
}
