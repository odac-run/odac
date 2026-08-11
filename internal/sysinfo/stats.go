package sysinfo

import (
	"math"

	"odac/internal/jscanon"
)

// cpuTicksFn / netStatsFn are test seams over the platform collectors.
var (
	cpuTicksFn = cpuTicks
	netStatsFn = netStats
)

// Stats reports live resource utilization for the Hub's system.stats task:
// CPU load as an integer percent (0-100) plus memory, disk and swap as
// used/total byte counts, and one entry per accelerator in `gpu` (empty on a
// host with none; see gpu_stats.go). It mirrors the cpu/memory/disk slice of
// Node's System.status() (getStatus never shipped a caller in Node, so this is the
// first live use); swap is an ODAC addition surfacing the elastic swap manager
// (internal/system/swap) for the dashboard.
// Fields degrade to zero on platforms whose collectors are stubbed — the
// production host is Linux; darwin covers development (see the package
// comment). Keys are alphabetical to match jscanon's literal-order contract.
func (i *Info) Stats() jscanon.Obj {
	// used is total-available (not total-free): MemAvailable counts
	// reclaimable cache as free, so this matches what htop/`free` call used.
	// total-free would over-report used by the whole buffer/cache size.
	totalKB, _, availKB := memoryKB()
	usedKB := totalKB - availKB
	if usedKB < 0 {
		usedKB = 0
	}

	diskTotal, diskFree := diskBytes()
	diskUsed := diskTotal - diskFree
	if diskUsed < 0 {
		diskUsed = 0
	}

	swapTotalKB, swapFreeKB := swapKB()
	swapUsedKB := swapTotalKB - swapFreeKB
	if swapUsedKB < 0 {
		swapUsedKB = 0
	}

	return jscanon.Obj{
		{K: "cpu", V: i.cpuUsage()},
		{K: "disk", V: jscanon.Obj{
			{K: "used", V: diskUsed},
			{K: "total", V: diskTotal},
		}},
		{K: "gpu", V: i.gpuStatsField()},
		{K: "memory", V: jscanon.Obj{
			{K: "used", V: usedKB * 1024},
			{K: "total", V: totalKB * 1024},
		}},
		{K: "network", V: i.networkUsage()},
		{K: "swap", V: jscanon.Obj{
			{K: "used", V: swapUsedKB * 1024},
			{K: "total", V: swapTotalKB * 1024},
		}},
	}
}

// networkUsage ports getNetworkUsage(): download/upload as bytes-per-second
// averaged over the window since the previous sample. rx/tx carry the raw
// cumulative received/transmitted byte counters behind those rates. The first
// sample (or an unreadable/rolled-back counter, e.g. an interface reset)
// reports 0/0 rates and re-baselines — exactly like Node's guards — while
// rx/tx still surface the current absolute counters. Keys are alphabetical to
// match jscanon's literal-order contract.
func (i *Info) networkUsage() jscanon.Obj {
	net := func(download, upload, rx, tx int64) jscanon.Obj {
		return jscanon.Obj{
			{K: "download", V: download},
			{K: "rx", V: rx},
			{K: "tx", V: tx},
			{K: "upload", V: upload},
		}
	}

	recv, sent, ok := netStatsFn()
	if !ok {
		return net(0, 0, 0, 0)
	}

	now := i.now()
	i.netMu.Lock()
	defer i.netMu.Unlock()

	if !i.hasNet {
		i.lastRecv, i.lastSent, i.lastNetTime, i.hasNet = recv, sent, now, true
		return net(0, 0, recv, sent)
	}

	secs := now.Sub(i.lastNetTime).Seconds()
	recvDiff := recv - i.lastRecv
	sentDiff := sent - i.lastSent
	i.lastRecv, i.lastSent, i.lastNetTime = recv, sent, now

	if recvDiff < 0 || sentDiff < 0 || secs <= 0 {
		return net(0, 0, recv, sent)
	}
	return net(
		int64(math.Round(float64(recvDiff)/secs)),
		int64(math.Round(float64(sentDiff)/secs)),
		recv, sent,
	)
}

// cpuUsage ports getCpuUsage(): the busy percentage between this sample and
// the previous one. The first call (or any unreadable/rolled-back counter)
// reports 0, exactly like Node's guard.
func (i *Info) cpuUsage() int {
	idle, total, ok := cpuTicksFn()
	if !ok {
		return 0
	}

	i.cpuMu.Lock()
	defer i.cpuMu.Unlock()

	if !i.hasCPU {
		i.lastIdle, i.lastTotal, i.hasCPU = idle, total, true
		return 0
	}

	idleDiff := idle - i.lastIdle
	totalDiff := total - i.lastTotal
	i.lastIdle, i.lastTotal = idle, total

	if idleDiff < 0 || totalDiff <= 0 {
		return 0
	}

	// Node: 100 - ~~((100 * idleDiff) / totalDiff), clamped to [0,100].
	usage := 100 - int(100*idleDiff/totalDiff)
	if usage < 0 {
		return 0
	}
	if usage > 100 {
		return 100
	}
	return usage
}
