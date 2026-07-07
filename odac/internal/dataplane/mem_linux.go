//go:build linux

package dataplane

import (
	"os"
	"strconv"
	"strings"
)

// memInfo returns total and used physical memory in bytes like Node's
// os.totalmem()/freemem() pair. The proxy uses it only as a cache-sizing
// hint (UpdateMemory in server/proxy).
func memInfo() (total, used uint64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvailable, memFree uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			memTotal = kb
		case "MemAvailable:":
			memAvailable = kb
		case "MemFree:":
			memFree = kb
		}
	}
	if memTotal == 0 {
		return 0, 0
	}
	free := memAvailable
	if free == 0 {
		free = memFree
	}
	if free > memTotal {
		free = memTotal
	}
	return memTotal * 1024, (memTotal - free) * 1024
}
