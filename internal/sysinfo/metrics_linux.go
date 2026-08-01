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

// memoryKB reads /proc/meminfo (values already in KB). total is MemTotal and
// free is MemFree (os.freemem() parity — the figure Get reports). available is
// MemAvailable, the kernel's estimate of memory obtainable without swapping: it
// counts reclaimable cache as free, so total-available is the honest "used"
// that matches htop/`free`. MemFree over-reports used by the whole cache size.
// On kernels before 3.14 (no MemAvailable) it falls back to Free+Buffers+Cached.
func memoryKB() (total, free, available int64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	return parseMeminfoKB(raw)
}

// parseMeminfoKB is the pure /proc/meminfo parser behind memoryKB, split out so
// the MemAvailable fallback is unit-testable without a live host.
func parseMeminfoKB(raw []byte) (total, free, available int64) {
	var buffers, cached int64
	haveAvail := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemFree:":
			free = v
		case "MemAvailable:":
			available, haveAvail = v, true
		case "Buffers:":
			buffers = v
		case "Cached:": // note: distinct from "SwapCached:"
			cached = v
		}
	}
	if !haveAvail {
		available = free + buffers + cached
	}
	return total, free, available
}

// swapKB reports swap total/free from /proc/meminfo (already in KB). Zero when
// the host has no swap or the file is unreadable — the same source the swap
// manager acts on, surfaced for the system.stats dashboard.
func swapKB() (total, free int64) {
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
		case "SwapTotal:":
			total, _ = strconv.ParseInt(fields[1], 10, 64)
		case "SwapFree:":
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

// cpuTicks reads the aggregate "cpu" line of /proc/stat. idle mirrors
// libuv/os.cpus() which counts only the idle column (not iowait); total is
// the sum of every column. ok is false when the file is unreadable.
func cpuTicks() (idle, total int64, ok bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		for i := 1; i < len(fields); i++ {
			v, _ := strconv.ParseInt(fields[i], 10, 64)
			total += v
			if i == 4 { // idle column
				idle = v
			}
		}
		return idle, total, true
	}
	return 0, 0, false
}

// diskBytes reports the root filesystem's total and available bytes, matching
// Node's `df -k /` (available blocks, so total-free is the used figure a
// non-root process sees).
func diskBytes() (total, free int64) {
	var st unix.Statfs_t
	if unix.Statfs("/", &st) != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs
}

// netStats reads /proc/net/dev, returning the received/transmitted byte
// counters of the first physical-looking interface (Node grepped
// eth0|ens|enp and took the first match; loopback and virtual bridges are
// skipped). ok is false when the file is unreadable or no interface matches.
func netStats() (recv, sent int64, ok bool) {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue // header rows have no colon
		}
		name = strings.TrimSpace(name)
		if !(name == "eth0" || strings.HasPrefix(name, "ens") || strings.HasPrefix(name, "enp")) {
			continue
		}
		// Receive columns: bytes packets errs drop fifo frame compressed
		// multicast; Transmit bytes is the 9th field of rest.
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return 0, 0, false
		}
		recv, _ = strconv.ParseInt(fields[0], 10, 64)
		sent, _ = strconv.ParseInt(fields[8], 10, 64)
		return recv, sent, true
	}
	return 0, 0, false
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
