package sysinfo

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// kernelRelease is os.release(): uname -r.
func kernelRelease() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// memoryKB ports Math.floor(os.totalmem()/1024) / freemem: /proc/meminfo
// MemTotal/MemFree (already in KB).
func memoryKB() (total, free int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, _ = strconv.ParseInt(fields[1], 10, 64)
		case "MemFree:":
			free, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return total, free
}

// loadAvg is os.loadavg(): /proc/loadavg's first three fields.
func loadAvg() (l1, l2, l3 float64) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) >= 3 {
		l1, _ = strconv.ParseFloat(fields[0], 64)
		l2, _ = strconv.ParseFloat(fields[1], 64)
		l3, _ = strconv.ParseFloat(fields[2], 64)
	}
	return l1, l2, l3
}

// uptimeSeconds is os.uptime(): /proc/uptime's first field (fractional
// seconds, like libuv).
func uptimeSeconds() float64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	up, _ := strconv.ParseFloat(fields[0], 64)
	return up
}

// cpuModel is os.cpus()[0].model: the first "model name" in /proc/cpuinfo.
func cpuModel() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, value, found := strings.Cut(line, ":"); found &&
			strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}
