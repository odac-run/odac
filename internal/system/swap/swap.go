// Package swap is ODAC's elastic host-swap manager (see SWAP_PLAN.md). On a
// Linux host it watches memory pressure and grows swap by adding swapfile
// increments (/swapfile.odac.N) when the system is genuinely tight, then shrinks
// by removing idle increments when pressure clears — a one-way-safe, disk-capped
// loop driven by the system orchestrator's autonomous check tick. On non-Linux
// builds every entry point is a no-op.
//
// The decision engine (decide) is platform-independent and pure, so it is unit
// tested without root or a live host; the actuator that actually calls
// swapon/swapoff/fstab lives behind the controller interface (swap_linux.go).
package swap

import (
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// swapBasename is the fixed stem of every swapfile this manager owns; the
	// full name is <dir>/swapfile.odac.<N>. Only paths under the active prefix
	// are ever touched — never a user's own swap.
	swapBasename = "swapfile.odac."

	kib = 1024
	mib = 1024 * kib
	gib = 1024 * mib
)

// swapPrefix is the absolute path stem for this manager's swapfiles in dir, e.g.
// "/app/.odac/swap/swapfile.odac." — parseSwaps rebuilds each recognized area's
// path from it and nextPath appends the increment index. The swap directory is host-backed and
// configurable because on a containerized host the container root is overlayfs,
// which the kernel refuses to swapon; the files must live on a real filesystem
// (see swap_linux.go / SWAP_PLAN.md "container hosts").
func swapPrefix(dir string) string {
	return strings.TrimRight(dir, "/") + "/" + swapBasename
}

// area is one active swap area owned by this manager (a row of /proc/swaps
// whose filename matches filePrefix). Sizes are bytes.
type area struct {
	path string
	size int64
	used int64
}

// diskFile is one of our swapfiles on disk, active or not — a reboot leaves the
// file but drops the swap area.
type diskFile struct {
	path string
	idx  int
	size int64
}

// incrementIndex is the ownership rule: a name is ours only if it is exactly
// swapfile.odac.<N> with N numeric. Anything else is never touched.
func incrementIndex(name string) (string, bool) {
	idx := strings.TrimPrefix(name, swapBasename)
	if idx == name {
		return "", false
	}
	if _, err := strconv.Atoi(idx); err != nil {
		return "", false
	}
	return idx, true
}

// snapshot is the full input to a single decide() call: host memory, swap, and
// disk state plus the pressure signal. All byte counts. ok is false when the
// platform is unsupported or /proc was unreadable, in which case decide holds.
type snapshot struct {
	memTotal     int64
	memAvail     int64
	swapTotal    int64 // system-wide, all swap areas
	swapFree     int64
	freeDisk     int64   // available bytes on the swapfile filesystem (/)
	psiSomeAvg10 float64 // /proc/pressure/memory "some avg10"
	areas        []area  // only this manager's increments, ascending by index
	ok           bool
}

// Config is the parsed system.swap config object (config/system.json). Defaults
// mirror internal/config applyDefaults so a nil/partial map still yields sane
// values.
type Config struct {
	AutoManage    bool
	Persist       bool
	MaxDiskPct    int64
	MaxIncrements int64
	AllowShrink   bool
	// Dir overrides where swapfiles are created. Empty means the manager
	// derives it from the ODAC base directory (<baseDir>/swap), which is
	// always on host-backed storage; set it to place swap on a different
	// mount (e.g. a dedicated fast SSD).
	Dir string
}

// configFromMap parses cfg.Map("swap"). Numbers may be int (fresh defaults) or
// float64 (after a JSON round-trip), so every read is type-tolerant.
func configFromMap(m map[string]any) Config {
	return Config{
		AutoManage:    cfgBool(m, "autoManage", true),
		Persist:       cfgBool(m, "persist", true),
		MaxDiskPct:    cfgInt64(m, "maxDiskPct", 25),
		MaxIncrements: cfgInt64(m, "maxIncrements", 8),
		AllowShrink:   cfgBool(m, "allowShrink", true),
		Dir:           cfgStr(m, "dir", ""),
	}
}

func cfgStr(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

func cfgBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

func cfgInt64(m map[string]any, key string, def int64) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return def
}

// --- pure /proc parsers (kept platform-independent so they test off-host) ---

// parseMeminfoBytes reads MemTotal/MemAvailable/SwapTotal/SwapFree from
// /proc/meminfo (KB) and returns bytes. MemAvailable falls back to
// MemFree+Buffers+Cached on kernels before 3.14 that lack the field — the same
// honest "available" the sysinfo memory-used fix uses.
func parseMeminfoBytes(raw []byte) (memTotal, memAvail, swapTotal, swapFree int64) {
	var memFree, buffers, cached int64
	haveAvail := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		v *= kib // /proc/meminfo is in KB
		switch fields[0] {
		case "MemTotal:":
			memTotal = v
		case "MemFree:":
			memFree = v
		case "MemAvailable:":
			memAvail, haveAvail = v, true
		case "Buffers:":
			buffers = v
		case "Cached:": // distinct from "SwapCached:"
			cached = v
		case "SwapTotal:":
			swapTotal = v
		case "SwapFree:":
			swapFree = v
		}
	}
	if !haveAvail {
		memAvail = memFree + buffers + cached
	}
	return memTotal, memAvail, swapTotal, swapFree
}

// parsePressureSomeAvg10 extracts the "some avg10=" figure from
// /proc/pressure/memory — the share of the last 10s in which at least one task
// stalled on memory. It is the truest "are we actually tight" signal. Returns 0
// when the line/field is absent (PSI disabled or unreadable).
func parsePressureSomeAvg10(raw []byte) float64 {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if rest, ok := strings.CutPrefix(f, "avg10="); ok {
				v, _ := strconv.ParseFloat(rest, 64)
				return v
			}
		}
	}
	return 0
}

// parseSwaps returns the swap areas this manager owns, ascending by increment
// index. /proc/swaps columns are "Filename Type Size Used Priority" with
// Size/Used in KB. A user's own swap and swap partitions are ignored.
//
// Ownership is decided by basename, not the full path: the increment name
// swapfile.odac.<N> is this manager's exclusive stem. We deliberately do NOT
// match the full directory prefix, because inside a container /proc/swaps can
// render a bind-mounted swapfile under the mount's source root (e.g.
// "/.odac/swap/swapfile.odac.1") instead of the path we created it at
// ("/app/.odac/swap/swapfile.odac.1"). A full-path match there silently misses
// our own swap and the manager loops forever trying to recreate the baseline
// ("Text file busy"). For each match we rebuild the path as <prefix><N> — the
// real, accessible path under our dir — so swapoff/rm never depend on how the
// kernel happened to render the row.
func parseSwaps(raw []byte, prefix string) []area {
	var areas []area
	for i, line := range strings.Split(string(raw), "\n") {
		if i == 0 { // header row
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		idx, ok := incrementIndex(filepath.Base(fields[0]))
		if !ok {
			continue
		}
		size, _ := strconv.ParseInt(fields[2], 10, 64)
		used, _ := strconv.ParseInt(fields[3], 10, 64)
		areas = append(areas, area{
			path: prefix + idx, // real path under our dir, not the /proc-rendered one
			size: size * kib,
			used: used * kib,
		})
	}
	sortAreasByIndex(areas, prefix)
	return areas
}

// sortAreasByIndex orders <prefix>N ascending by N so SHRINK can pop the highest
// (LIFO) deterministically. Insertion sort — the slice is tiny (<=32).
func sortAreasByIndex(areas []area, prefix string) {
	for i := 1; i < len(areas); i++ {
		for j := i; j > 0 && indexOf(areas[j-1].path, prefix) > indexOf(areas[j].path, prefix); j-- {
			areas[j-1], areas[j] = areas[j], areas[j-1]
		}
	}
}

// indexOf returns the N in <prefix>N, or -1 if the suffix is not numeric.
func indexOf(path, prefix string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(path, prefix))
	if err != nil {
		return -1
	}
	return n
}
