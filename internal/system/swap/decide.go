package swap

// The manager keeps one empty swap increment ahead of the fill frontier: a
// "hot" demand tier (RAM, or any swapfile, whose fill crosses growHotPct) must
// always have a pre-provisioned empty increment beyond it, so a burst has an
// active landing pad instantly — no waiting for a reactive mkswap/swapon. The
// target count is therefore hotTiers + 1. SHRINK only reclaims a surplus spare
// once every tier has cooled below coolPct; the wide gap between growHotPct and
// coolPct is the hysteresis that prevents flapping. Grow is fast, shrink is slow
// and deliberate.
const (
	growHotPct       = 80   // a tier at/above this fill is "hot" (needs a spare below)
	coolPct          = 40   // a tier must fall below this to stop demanding a spare
	emptyPct         = 10   // an increment at/below this fill is an idle, removable spare
	growPSIThreshold = 20.0 // PSI "some avg10" flags RAM hot regardless of fill%
	shrinkPSIMax     = 1.0  // pressure must be essentially gone before a tier cools

	growStreakNeeded   = 2  // ~60s confirm before opening (drops transient blips)
	shrinkStreakNeeded = 10 // ~5min cooldown before reclaiming a surplus spare

	baselineFloor = 1 // once managing, always keep at least one increment

	minIncrement     = 1 * gib  // never bother allocating less than this
	maxIncrementStep = 16 * gib // per-increment ceiling
)

type action int

const (
	hold action = iota
	grow
	shrink
)

// decision is the pure output of decide: what to do this tick, with the byte
// size (grow) or target path (shrink) and a human reason for the log.
type decision struct {
	action action
	size   int64  // grow: bytes to allocate
	target string // shrink: swapfile path to remove
	reason string
}

// counters carry the consecutive-tick streaks across decide calls so a single
// spike never triggers action — the condition must be sustained.
type counters struct {
	growStreak   int
	shrinkStreak int
}

// decide is the platform-independent heart of the manager: given a live
// snapshot, the parsed config, and the running streak state, it returns the one
// action to take this tick. Pure and side-effect free except for the streak
// counters, so it is exhaustively unit tested without a host.
func decide(s snapshot, cfg Config, st *counters) decision {
	if !cfg.AutoManage || !s.ok {
		st.reset()
		return decision{action: hold, reason: "auto-manage off or no snapshot"}
	}

	have := int64(len(s.areas))
	// wantGrow counts tiers hot at the high threshold; wantCool at the low one.
	// wantCool >= wantGrow always, so there is a proper hold band between them.
	wantGrow := desiredBuffers(s, growHotPct, growPSIThreshold)
	wantCool := desiredBuffers(s, coolPct, shrinkPSIMax)

	// GROW: fewer increments than the hot frontier needs — open a fresh spare.
	if have < wantGrow {
		st.shrinkStreak = 0
		st.growStreak++
		if st.growStreak < growStreakNeeded {
			return decision{action: hold, reason: "buffer low, confirming"}
		}
		if size, ok := plannedGrowSize(s, cfg, sumAreas(s.areas)); ok {
			st.growStreak = 0
			reason := "add empty buffer ahead of frontier"
			if have == 0 {
				reason = "provision baseline swap"
			}
			return decision{action: grow, size: size, reason: reason}
		}
		return decision{action: hold, reason: "grow wanted but disk/increment ceiling"}
	}
	st.growStreak = 0

	// SHRINK: more increments than even the cooled frontier needs — reclaim the
	// idle top spare, never below the baseline.
	if cfg.AllowShrink && have > wantCool && have > baselineFloor {
		st.shrinkStreak++
		if st.shrinkStreak < shrinkStreakNeeded {
			return decision{action: hold, reason: "surplus buffer, cooling down"}
		}
		if target, ok := plannedShrinkTarget(s); ok {
			st.shrinkStreak = 0
			return decision{action: shrink, target: target, reason: "reclaim idle spare"}
		}
		return decision{action: hold, reason: "surplus but top not safe to reclaim"}
	}
	st.shrinkStreak = 0
	return decision{action: hold, reason: "steady"}
}

func (st *counters) reset() { st.growStreak, st.shrinkStreak = 0, 0 }

// desiredBuffers returns how many increments we want: enough working capacity to
// hold the current footprint below fillThreshold, plus one empty reserve on top.
//
// The footprint (poolUsed) is the honest non-reclaimable anonymous demand —
// RAM used (MemTotal-MemAvailable, so page cache is not counted) plus everything
// already on our swap. The kernel splits that demand between RAM and swap however
// it likes; measuring the *sum* against pool capacity is what stops a footprint
// spread thin across tiers from looking like slack (the bug where Linux offloads
// to swap, RAM's memAvail recovers, and every tier reads below threshold at once).
//
// The working pool is RAM plus every increment except the top reserve, so we size
// the working increments needed to keep fill under threshold, then add the +1
// reserve. A PSI spike forces at least one spare beyond what we hold — sudden
// pressure the memAvail figure has not caught yet — but never below the fill need.
func desiredBuffers(s snapshot, fillThreshold int64, psiThreshold float64) int64 {
	poolUsed := memUsedBytes(s) + sumAreasUsed(s.areas)
	target := poolUsed * 100 / clampMin64(fillThreshold, 1) // working capacity to stay at threshold

	// Grow the desired count until the working pool — RAM plus every increment
	// except the top reserve spare — can hold the footprint at <= threshold. Each
	// existing increment contributes its real size; any increment beyond the
	// current set is estimated at the nominal step. desired always keeps 1 reserve.
	workingCap := s.memTotal
	desired := int64(1)
	step := incrementStep(s)
	for workingCap < target {
		add := step
		if idx := desired - 1; idx < int64(len(s.areas)) && s.areas[idx].size > 0 {
			add = s.areas[idx].size
		}
		workingCap += add
		desired++
	}
	if desired < baselineFloor {
		desired = baselineFloor
	}
	// PSI override: sudden pressure the memAvail figure has not caught yet forces
	// at least one spare beyond what we hold, but never below the fill-based need.
	if have := int64(len(s.areas)); s.psiSomeAvg10 > psiThreshold && desired < have+1 {
		desired = have + 1
	}
	return desired
}

// plannedGrowSize returns the byte size of the next increment, honoring the
// increment-count cap and the disk budget (total odac swap must stay within
// MaxDiskPct of the disk, counting our own swapfiles as reclaimable space).
// ok is false when a cap blocks growth.
func plannedGrowSize(s snapshot, cfg Config, totalOdac int64) (int64, bool) {
	if int64(len(s.areas)) >= cfg.MaxIncrements {
		return 0, false
	}
	// freeDisk already excludes our swapfiles; add them back so the percentage
	// is against a stable base and does not double-count.
	maxTotal := pct(s.freeDisk+totalOdac, cfg.MaxDiskPct)
	budget := maxTotal - totalOdac
	if budget < minIncrement {
		return 0, false
	}
	return min64(incrementStep(s), budget), true
}

// planReconcile decides what to do with our swapfiles that the kernel is not
// using — what a reboot leaves behind. Adopting is the default, and MaxDiskPct
// deliberately does not gate it: the file is already allocated, so swapon costs
// no disk at all, and deleting it would trade away swap capacity for space that
// was never at stake. The disk cap governs creating new swap (plannedGrowSize);
// a surplus that is genuinely idle is reclaimed by the ordinary shrink path
// instead, one increment at a time and only once it is provably empty.
//
// MaxIncrements still gates, because the LIFO indexing depends on the count.
// Lowest index first, so the baseline is the last thing sacrificed.
func planReconcile(files []diskFile, active []area, cfg Config) (adopt, discard []string) {
	live := make(map[string]bool, len(active))
	for _, a := range active {
		live[a.path] = true
	}
	var candidates []diskFile
	for _, f := range files {
		if !live[f.path] {
			candidates = append(candidates, f)
		}
	}
	sortFilesByIndex(candidates)

	count := int64(len(active))
	for _, f := range candidates {
		if count >= cfg.MaxIncrements {
			discard = append(discard, f.path)
			continue
		}
		adopt = append(adopt, f.path)
		count++
	}
	return adopt, discard
}

// sortFilesByIndex orders ascending by increment index: directory listings are
// lexical, which would put .10 before .2.
func sortFilesByIndex(files []diskFile) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j-1].idx > files[j].idx; j-- {
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}

// incrementStep is the nominal size of one increment: half of RAM, clamped to
// [minIncrement, maxIncrementStep]. Used both to plan a real grow and to size the
// "working increments needed" estimate in desiredBuffers, so the two agree.
func incrementStep(s snapshot) int64 {
	return clamp64(s.memTotal/2, minIncrement, maxIncrementStep)
}

// plannedShrinkTarget picks the highest-index increment (LIFO) when it is a safe
// idle spare to remove: it must be near-empty and the little it holds must fit
// back into current available RAM. The baseline increment is always kept.
func plannedShrinkTarget(s snapshot) (string, bool) {
	if int64(len(s.areas)) <= baselineFloor {
		return "", false
	}
	top := s.areas[len(s.areas)-1]
	if areaFillPct(top) > emptyPct {
		return "", false // still holding pages — not an idle spare
	}
	if top.used >= s.memAvail {
		return "", false // its pages would not comfortably fit back in RAM
	}
	return top.path, true
}

// ramUsedPct is RAM utilization from the honest MemAvailable figure.
func ramUsedPct(s snapshot) int64 {
	if s.memTotal <= 0 {
		return 0
	}
	return memUsedBytes(s) * 100 / s.memTotal
}

// memUsedBytes is non-reclaimable RAM: MemTotal-MemAvailable, so reclaimable page
// cache does not count as "used" (mirrors the sysinfo memory-used fix).
func memUsedBytes(s snapshot) int64 {
	if u := s.memTotal - s.memAvail; u > 0 {
		return u
	}
	return 0
}

// sumAreasUsed totals the pages currently held across this manager's swapfiles.
func sumAreasUsed(areas []area) int64 {
	var t int64
	for _, a := range areas {
		t += a.used
	}
	return t
}

// poolFillPct is the combined footprint (RAM used + all swap used) as a percent
// of the working pool (RAM + every increment except the top reserve spare). It is
// the single signal that drives grow/shrink; logged so a decision is explainable.
func poolFillPct(s snapshot) int64 {
	cap := s.memTotal + sumAreas(s.areas)
	if n := len(s.areas); n > 0 {
		cap -= s.areas[n-1].size // exclude the top reserve spare
	}
	if cap <= 0 {
		return 0
	}
	return (memUsedBytes(s) + sumAreasUsed(s.areas)) * 100 / cap
}

func areaFillPct(a area) int64 {
	if a.size <= 0 {
		return 0
	}
	return a.used * 100 / a.size
}

// swapFillPct is the system-wide swap utilization percent, used for logging.
func swapFillPct(s snapshot) int64 {
	if s.swapTotal <= 0 {
		return 0
	}
	return (s.swapTotal - s.swapFree) * 100 / s.swapTotal
}

func sumAreas(areas []area) int64 {
	var t int64
	for _, a := range areas {
		t += a.size
	}
	return t
}

func pct(v, p int64) int64 { return v * p / 100 }

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func clampMin64(v, lo int64) int64 {
	if v < lo {
		return lo
	}
	return v
}
