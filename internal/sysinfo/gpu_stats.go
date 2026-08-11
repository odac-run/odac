package sysinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"odac/internal/gpu"
	"odac/internal/jscanon"
)

// gpuSampleMinInterval floors how often the live metrics are collected. The
// Hub's system.stats task runs every 60s, but the Cloud can reconfigure that
// interval down to a second (handleConfigure), and each NVIDIA sample costs
// a process spawn — on a host whose whole job is inference, burning a slice
// of a core on nvidia-smi would be absurd. Samples inside the window are
// reused.
const gpuSampleMinInterval = 5 * time.Second

// nvidiaSMIStatsQuery is a test seam over the live-metrics exec.
var nvidiaSMIStatsQuery = runNvidiaSMIStats

// gpuDeviceStats is one accelerator's live utilization. Every field degrades
// to zero when its source is unavailable, the same contract the rest of the
// stats payload follows (see the package comment): an Intel card exposes no
// busy counter, an NVIDIA card without nvidia-smi exposes nothing at all.
type gpuDeviceStats struct {
	usagePercent int
	memUsedBytes int64
	memTotBytes  int64
	tempCelsius  int
}

// gpuStatsField renders the `gpu` member of system.stats: one entry per
// device, in the same order as the `devices` array of system.info, so the
// Cloud can join the two positionally. Empty on a host with no accelerator.
//
// Memory is in bytes, the same unit system.info's `vram` and every other
// figure in this payload use.
func (i *Info) gpuStatsField() []any {
	samples := i.gpuStats()
	out := make([]any, 0, len(samples))
	for _, s := range samples {
		out = append(out, jscanon.Obj{
			{K: "memory", V: jscanon.Obj{
				{K: "used", V: s.memUsedBytes},
				{K: "total", V: s.memTotBytes},
			}},
			{K: "temperature", V: s.tempCelsius},
			{K: "usage", V: s.usagePercent},
		})
	}
	return out
}

// gpuStats returns the cached sample, re-collecting at most every
// gpuSampleMinInterval. Collection runs under the lock so concurrent callers
// coalesce instead of spawning parallel nvidia-smi processes.
func (i *Info) gpuStats() []gpuDeviceStats {
	devices := i.gpuState().devices // cached inventory: order and sysfs paths
	if len(devices) == 0 {
		return nil
	}

	i.gpuStatsMu.Lock()
	defer i.gpuStatsMu.Unlock()
	now := i.now()
	if i.gpuSamples != nil && now.Sub(i.gpuSampleAt) < gpuSampleMinInterval &&
		len(i.gpuSamples) == len(devices) {
		return i.gpuSamples
	}
	i.gpuSamples = collectGPUStats(devices)
	i.gpuSampleAt = now
	return i.gpuSamples
}

// collectGPUStats fills one sample per device: NVIDIA cards come from a
// single nvidia-smi query (merged back by PCI address), AMD cards from
// amdgpu's own sysfs counters — no exec needed there.
func collectGPUStats(devices []gpuDevice) []gpuDeviceStats {
	samples := make([]gpuDeviceStats, len(devices))
	needNvidia := false
	for idx, device := range devices {
		samples[idx].memTotBytes = device.vramBytes
		switch device.vendor {
		case gpu.VendorNvidia:
			needNvidia = true
		case gpu.VendorAMD:
			amdDeviceStats(device.path, &samples[idx])
		}
	}

	if needNvidia {
		for _, entry := range nvidiaSMIStatsQuery() {
			idx := indexByAddr(devices, entry.addr)
			if idx < 0 {
				continue // a card that appeared since the inventory probe
			}
			samples[idx] = entry.stats
		}
	}
	return samples
}

// amdDeviceStats reads amdgpu's sysfs counters: gpu_busy_percent is the
// utilization the driver itself reports, VRAM is exact, and the temperature
// lives one level down in hwmon (millidegrees).
func amdDeviceStats(path string, out *gpuDeviceStats) {
	if path == "" {
		return
	}
	if busy, err := strconv.Atoi(sysfsValue(path, "gpu_busy_percent")); err == nil {
		out.usagePercent = clampPercent(busy)
	}
	if used, err := strconv.ParseInt(sysfsValue(path, "mem_info_vram_used"), 10, 64); err == nil && used > 0 {
		out.memUsedBytes = used
	}
	if total, err := strconv.ParseInt(sysfsValue(path, "mem_info_vram_total"), 10, 64); err == nil && total > 0 {
		out.memTotBytes = total
	}
	out.tempCelsius = hwmonCelsius(path)
}

// hwmonCelsius reads temp1_input from the first hwmon node under a PCI
// device (millidegrees Celsius), 0 when the driver publishes none.
func hwmonCelsius(path string) int {
	entries, err := os.ReadDir(filepath.Join(path, "hwmon"))
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		raw := sysfsValue(filepath.Join(path, "hwmon", entry.Name()), "temp1_input")
		if milli, err := strconv.Atoi(raw); err == nil {
			return milli / 1000
		}
	}
	return 0
}

// nvidiaSMIStatsEntry is one parsed live-metrics row.
type nvidiaSMIStatsEntry struct {
	addr  string
	stats gpuDeviceStats
}

// runNvidiaSMIStats queries every card's live counters in one exec. An
// absent binary yields nothing and the samples stay zero.
func runNvidiaSMIStats() []nvidiaSMIStatsEntry {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSMITimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin,
		"--query-gpu=pci.bus_id,utilization.gpu,memory.used,memory.total,temperature.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMIStats(string(out))
}

// parseNvidiaSMIStats reads the csv rows of runNvidiaSMIStats' query. The
// memory columns are MiB under --format=nounits and are converted to bytes
// here; unavailable columns ("[N/A]", as on some vGPU setups) parse to 0
// rather than dropping the card.
func parseNvidiaSMIStats(out string) []nvidiaSMIStatsEntry {
	var entries []nvidiaSMIStatsEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 5 {
			continue
		}
		addr := normalizeAddr(fields[0])
		if addr == "" {
			continue
		}
		usage, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		usedMB, _ := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		totalMB, _ := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		temp, _ := strconv.Atoi(strings.TrimSpace(fields[4]))
		entries = append(entries, nvidiaSMIStatsEntry{
			addr: addr,
			stats: gpuDeviceStats{
				usagePercent: clampPercent(usage),
				memUsedBytes: usedMB * 1024 * 1024,
				memTotBytes:  totalMB * 1024 * 1024,
				tempCelsius:  temp,
			},
		})
	}
	return entries
}

// clampPercent keeps a driver-reported percentage inside 0-100, matching the
// guard cpuUsage applies to its own figure.
func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
