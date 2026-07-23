package swap

import "testing"

// defaultCfg mirrors the config defaults so tests exercise real thresholds.
func defaultCfg() Config {
	return Config{
		AutoManage:    true,
		Persist:       true,
		MaxDiskPct:    25,
		MaxIncrements: 8,
		AllowShrink:   true,
	}
}

// calm host: RAM comfortable, no pressure. big freeDisk so grows aren't clamped.
func calmSnap() snapshot {
	return snapshot{
		memTotal: 8 * gib,
		memAvail: 6 * gib, // 25% used
		freeDisk: 100 * gib,
		ok:       true,
	}
}

// growN drives decide until it emits a grow (or fails the streak), returning the
// decision on the acting tick.
func growN(s snapshot, st *counters) decision {
	var d decision
	for i := 0; i < growStreakNeeded; i++ {
		d = decide(s, defaultCfg(), st)
	}
	return d
}

func TestDecideDisabledOrNoSnapshot(t *testing.T) {
	st := &counters{growStreak: 2, shrinkStreak: 5}
	if d := decide(calmSnap(), Config{AutoManage: false}, st); d.action != hold {
		t.Errorf("auto-manage off should hold, got %v", d)
	}
	if st.growStreak != 0 || st.shrinkStreak != 0 {
		t.Error("streaks must reset when disabled")
	}

	bad := calmSnap()
	bad.ok = false
	if d := decide(bad, defaultCfg(), &counters{}); d.action != hold {
		t.Errorf("!ok snapshot should hold, got %v", d)
	}
}

func TestDecideProvisionsBaseline(t *testing.T) {
	// A calm host with no swap still wants one baseline increment (want=1).
	s := calmSnap() // no areas
	st := &counters{}
	if d := decide(s, defaultCfg(), st); d.action != hold {
		t.Fatalf("first tick should confirm, got %v", d)
	}
	d := decide(s, defaultCfg(), st)
	if d.action != grow || d.reason != "provision baseline swap" {
		t.Fatalf("baseline = %+v, want grow baseline", d)
	}
	if d.size != 4*gib { // clamp(8/2,1,16)
		t.Errorf("baseline size = %d, want 4 GiB", d.size)
	}
}

func TestDecideRAMHotOpensSpareEvenIfSwapEmpty(t *testing.T) {
	// Interpretation (b): RAM > 80% opens a second increment even though the
	// baseline swap is still empty — a fresh landing pad ahead of RAM.
	s := calmSnap()
	s.memAvail = pct(8*gib, 13) // 87% used → RAM hot
	s.areas = []area{{path: "/swapfile.odac.1", size: 2 * gib, used: 0}}

	d := growN(s, &counters{})
	if d.action != grow {
		t.Fatalf("RAM hot with empty baseline should grow a spare, got %v", d)
	}
}

func TestDecideSwapFillingOpensNext(t *testing.T) {
	// RAM hot AND swap1 >80% full, swap2 empty → want=3, open swap3.
	s := calmSnap()
	s.memAvail = pct(8*gib, 13) // RAM hot
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 2 * gib, used: pct(2*gib, 85)}, // hot
		{path: "/swapfile.odac.2", size: 2 * gib, used: 0},              // spare
	}
	d := growN(s, &counters{})
	if d.action != grow {
		t.Fatalf("filling swap1 should open a third increment, got %v", d)
	}
}

func TestDecideSteadyHoldsBaseline(t *testing.T) {
	// Calm host with exactly its one baseline spare: nothing to do, and the
	// baseline is never reclaimed.
	s := calmSnap()
	s.areas = []area{{path: "/swapfile.odac.1", size: 2 * gib, used: 0}}
	st := &counters{}
	for i := 0; i < shrinkStreakNeeded+5; i++ {
		if d := decide(s, defaultCfg(), st); d.action != hold {
			t.Fatalf("steady baseline should hold, got %v", d)
		}
	}
}

func TestDecideGrowNeedsConfirmStreak(t *testing.T) {
	s := calmSnap() // wants baseline
	st := &counters{}
	for i := 0; i < growStreakNeeded-1; i++ {
		if d := decide(s, defaultCfg(), st); d.action != hold {
			t.Fatalf("tick %d should hold while confirming, got %v", i, d)
		}
	}
	if d := decide(s, defaultCfg(), st); d.action != grow {
		t.Fatalf("confirmed tick should grow, got %v", d)
	}
	if st.growStreak != 0 {
		t.Error("grow streak should reset after acting")
	}
}

func TestDecideShrinkReclaimsSurplusAfterCooldown(t *testing.T) {
	// Calm host carrying two idle spares → reclaim the top after the cooldown,
	// down to the baseline (never below).
	s := calmSnap()
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 2 * gib, used: 0},
		{path: "/swapfile.odac.2", size: 2 * gib, used: 0},
	}
	st := &counters{}
	for i := 0; i < shrinkStreakNeeded-1; i++ {
		if d := decide(s, defaultCfg(), st); d.action != hold {
			t.Fatalf("tick %d should cool down, got %v", i, d)
		}
	}
	d := decide(s, defaultCfg(), st)
	if d.action != shrink || d.target != "/swapfile.odac.2" {
		t.Fatalf("after cooldown = %+v, want shrink of surplus .2", d)
	}
}

func TestDecideNeverShrinksBaseline(t *testing.T) {
	// One idle increment on a calm host is the baseline — keep it forever.
	s := calmSnap()
	s.areas = []area{{path: "/swapfile.odac.1", size: 2 * gib, used: 0}}
	st := &counters{shrinkStreak: shrinkStreakNeeded}
	if d := decide(s, defaultCfg(), st); d.action != hold {
		t.Errorf("baseline must never be reclaimed, got %v", d)
	}
}

func TestDecideHysteresisBandHolds(t *testing.T) {
	// Combined pool fill sits in the band between coolPct (40) and growHotPct
	// (80): with two empty spares and ~50% working-pool fill, the grow math wants
	// no more and the cool math does not see a surplus → steady, no flapping.
	s := calmSnap()
	s.memAvail = 3 * gib // 5 GiB used on 8 GiB → mid-band footprint
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 2 * gib, used: 0},
		{path: "/swapfile.odac.2", size: 2 * gib, used: 0},
	}
	st := &counters{}
	for i := 0; i < shrinkStreakNeeded+3; i++ {
		if d := decide(s, defaultCfg(), st); d.action != hold {
			t.Fatalf("hysteresis band should hold, got %v", d)
		}
	}
}

func TestDecideSplitPoolGrowsWhereTiersWouldMiss(t *testing.T) {
	// The real bug this metric fixes: the kernel offloads to swap, so RAM's
	// memAvail recovers and BOTH RAM (~80%) and the baseline swap (~50%) read
	// below the 80% threshold individually — the old per-tier logic saw two cool
	// tiers and held. The combined footprint (6.4G RAM + 2G swap = 8.4G on an
	// 8G+4G working pool) is genuinely tight, so the pool metric grows a spare.
	s := calmSnap()
	s.memAvail = 8*gib - pct(8*gib, 80) // 80% RAM used, not > 80 individually
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 4 * gib, used: pct(4*gib, 50)}, // 50% swap
	}
	// Sanity: neither tier is individually hot, yet the pool must be.
	if ramUsedPct(s) > growHotPct || areaFillPct(s.areas[0]) > growHotPct {
		t.Fatal("test setup wrong: a tier is individually hot")
	}
	d := growN(s, &counters{})
	if d.action != grow {
		t.Fatalf("split-but-full pool should grow a spare, got %v", d)
	}
}

func TestDecideShrinkDisabled(t *testing.T) {
	cfg := defaultCfg()
	cfg.AllowShrink = false
	s := calmSnap()
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 2 * gib, used: 0},
		{path: "/swapfile.odac.2", size: 2 * gib, used: 0},
	}
	st := &counters{shrinkStreak: shrinkStreakNeeded}
	if d := decide(s, cfg, st); d.action != hold {
		t.Errorf("allowShrink=false must never shrink, got %v", d)
	}
}

func TestDecideGrowDiskAndCountCaps(t *testing.T) {
	// Disk clamp: budget below a full step yields a smaller increment.
	s := calmSnap()
	s.memAvail = pct(8*gib, 13) // RAM hot → wants a spare
	s.freeDisk = 6 * gib        // 25% = 1.5 GiB budget, step would be 4 GiB
	d := growN(s, &counters{})
	if d.action != grow || d.size != pct(6*gib, 25) {
		t.Fatalf("disk-clamped grow = %+v, want grow 1.5 GiB", d)
	}

	// Increment count cap: at MaxIncrements, hold.
	s2 := calmSnap()
	s2.memAvail = pct(8*gib, 13)
	for i := 0; i < int(defaultCfg().MaxIncrements); i++ {
		s2.areas = append(s2.areas, area{path: "/swapfile.odac.x", size: 1 * gib, used: 1 * gib})
	}
	if d := growN(s2, &counters{}); d.action != hold {
		t.Errorf("at increment cap should hold, got %v", d)
	}
}

func TestPlannedShrinkTargetSafety(t *testing.T) {
	base := calmSnap()

	// Floor: a lone baseline is never a target.
	base.areas = []area{{path: "/swapfile.odac.1", size: gib, used: 0}}
	if _, ok := plannedShrinkTarget(base); ok {
		t.Error("baseline must not be a shrink target")
	}

	// Busy top: >emptyPct used is not an idle spare.
	base.areas = []area{
		{path: "/swapfile.odac.1", size: gib, used: 0},
		{path: "/swapfile.odac.2", size: gib, used: pct(gib, 50)},
	}
	if _, ok := plannedShrinkTarget(base); ok {
		t.Error("busy top must not be reclaimed")
	}

	// Won't fit back in RAM.
	base.memAvail = 1 * mib
	base.areas = []area{
		{path: "/swapfile.odac.1", size: gib, used: 0},
		{path: "/swapfile.odac.2", size: gib, used: 2 * mib},
	}
	if _, ok := plannedShrinkTarget(base); ok {
		t.Error("spare whose pages won't fit in RAM must not be reclaimed")
	}

	// Idle spare, fits → reclaim it.
	base.memAvail = 6 * gib
	base.areas = []area{
		{path: "/swapfile.odac.1", size: gib, used: 0},
		{path: "/swapfile.odac.2", size: gib, used: 0},
	}
	if target, ok := plannedShrinkTarget(base); !ok || target != "/swapfile.odac.2" {
		t.Errorf("idle spare should be reclaimed: %q %v", target, ok)
	}
}

func TestDesiredBuffersFormula(t *testing.T) {
	s := calmSnap()
	// Calm (2 GiB footprint on 8 GiB RAM): pool fits in RAM → want 1 baseline.
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 1 {
		t.Errorf("calm want = %d, want 1", got)
	}
	// RAM footprint alone crosses the pool threshold → want 2 (baseline + spare).
	s.memAvail = pct(8*gib, 13) // ~87% used
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 2 {
		t.Errorf("RAM hot want = %d, want 2", got)
	}
	// Footprint big enough to need two working increments plus the reserve → 3.
	s.areas = []area{{path: "/swapfile.odac.1", size: gib, used: pct(gib, 90)}}
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 3 {
		t.Errorf("RAM+swap hot want = %d, want 3", got)
	}
	// PSI spike forces one spare beyond what we hold even when the fill math is
	// calm: baseline present, calm footprint, high PSI → want 2.
	calm := calmSnap()
	calm.areas = []area{{path: "/swapfile.odac.1", size: 2 * gib, used: 0}}
	calm.psiSomeAvg10 = 50
	if got := desiredBuffers(calm, growHotPct, growPSIThreshold); got != 2 {
		t.Errorf("PSI-hot want = %d, want 2", got)
	}
}

func TestPlanReconcileAdoptsEverythingWithinCaps(t *testing.T) {
	// Post-reboot: nothing active, two swapfiles on disk, plenty of room.
	files := []diskFile{
		{path: "/s/swapfile.odac.2", idx: 2, size: gib},
		{path: "/s/swapfile.odac.1", idx: 1, size: gib},
	}
	adopt, discard := planReconcile(files, nil, defaultCfg())

	if len(discard) != 0 {
		t.Errorf("nothing should be discarded when the caps allow: %v", discard)
	}
	if len(adopt) != 2 || adopt[0] != "/s/swapfile.odac.1" || adopt[1] != "/s/swapfile.odac.2" {
		t.Fatalf("adopt = %v, want baseline first then .2", adopt)
	}
}

func TestPlanReconcileSortsNumericallyNotLexically(t *testing.T) {
	// A directory listing gives .10 before .2, but the tail is what gets
	// sacrificed when a cap bites, so the baseline must come first.
	files := []diskFile{
		{path: "/s/swapfile.odac.10", idx: 10, size: gib},
		{path: "/s/swapfile.odac.2", idx: 2, size: gib},
	}
	adopt, _ := planReconcile(files, nil, defaultCfg())
	if len(adopt) != 2 || adopt[0] != "/s/swapfile.odac.2" {
		t.Errorf("adopt = %v, want .2 before .10", adopt)
	}
}

func TestPlanReconcileHonorsIncrementCap(t *testing.T) {
	cfg := defaultCfg()
	cfg.MaxIncrements = 2
	files := []diskFile{
		{path: "/s/swapfile.odac.1", idx: 1, size: gib},
		{path: "/s/swapfile.odac.2", idx: 2, size: gib},
		{path: "/s/swapfile.odac.3", idx: 3, size: gib},
	}
	adopt, discard := planReconcile(files, nil, cfg)
	if len(adopt) != 2 {
		t.Fatalf("adopt = %v, want the first 2", adopt)
	}
	if len(discard) != 1 || discard[0] != "/s/swapfile.odac.3" {
		t.Errorf("the surplus increment should be discarded: %v", discard)
	}
}

func TestPlanReconcileIgnoresDiskCap(t *testing.T) {
	cfg := defaultCfg() // MaxDiskPct 25
	// The live area alone already exceeds a 25% budget, and the disk is nearly
	// full. It makes no difference: the leftover is allocated either way, so
	// swapon costs nothing and deleting it would only trade swap for space that
	// was never at stake. Shrink reclaims it later if it stays idle.
	active := []area{{path: "/s/swapfile.odac.1", size: 8 * gib}}
	files := []diskFile{
		{path: "/s/swapfile.odac.1", idx: 1, size: 8 * gib},
		{path: "/s/swapfile.odac.2", idx: 2, size: 8 * gib},
	}
	adopt, discard := planReconcile(files, active, cfg)
	if len(adopt) != 1 || adopt[0] != "/s/swapfile.odac.2" {
		t.Fatalf("adopt = %v, want the leftover regardless of the disk cap", adopt)
	}
	if len(discard) != 0 {
		t.Errorf("nothing should be discarded for disk reasons: %v", discard)
	}
}

func TestPlanReconcileCountsActiveTowardIncrementCap(t *testing.T) {
	cfg := defaultCfg()
	cfg.MaxIncrements = 2
	active := []area{{path: "/s/swapfile.odac.1", size: gib}}
	files := []diskFile{
		{path: "/s/swapfile.odac.1", idx: 1, size: gib},
		{path: "/s/swapfile.odac.2", idx: 2, size: gib},
		{path: "/s/swapfile.odac.3", idx: 3, size: gib},
	}
	adopt, discard := planReconcile(files, active, cfg)
	if len(adopt) != 1 || adopt[0] != "/s/swapfile.odac.2" {
		t.Fatalf("adopt = %v, want only .2 (the live .1 fills a slot)", adopt)
	}
	if len(discard) != 1 || discard[0] != "/s/swapfile.odac.3" {
		t.Errorf("the count surplus should be discarded: %v", discard)
	}
}
