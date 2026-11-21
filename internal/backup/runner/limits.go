package runner

import (
	"time"

	"odac/internal/docker"
)

// DefaultLimits sizes helpers and dumps from the host. A helper gets a
// quarter of memory within 512 MiB to 2 GiB (Kopia's index and upload
// buffers grow with the repository, not with the host), half the cores
// and a quarter of the default CPU weight; a dump gets an eighth of memory
// within 256 MiB to 1 GiB and one core. Both get a low I/O weight, so an
// app's own disk traffic goes first.
func DefaultLimits(memTotal int64, cores int) (helper, dump docker.Limits) {
	clamp := func(v, lo, hi int64) int64 { return max(lo, min(hi, v)) }
	helperCores := max(1, cores/2)
	helper = docker.Limits{
		NanoCPUs:    int64(helperCores) * 1e9,
		CPUShares:   256,
		Memory:      clamp(memTotal/4, 512<<20, 2<<30),
		PidsLimit:   256,
		BlkioWeight: 100,
	}
	dump = docker.Limits{
		NanoCPUs:    1e9,
		CPUShares:   256,
		Memory:      clamp(memTotal/8, 256<<20, 1<<30),
		PidsLimit:   64,
		BlkioWeight: 100,
	}
	return helper, dump
}

// DefaultDumpRate is how fast a dump is read unless configured: 64 MiB/s,
// about 230 GB an hour, which leaves most disks room for the app.
const DefaultDumpRate = 64 << 20

// DefaultDumpIdle is how long a dump may deliver nothing before it is
// ended as stalled: far longer than any lock wait the dump tools bound
// themselves (30s), and short enough that a dump queued behind a lock its
// tool cannot bound gives way long before the run's time limit.
const DefaultDumpIdle = 5 * time.Minute
