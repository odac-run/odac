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
	// Two increments, top at 50% fill (between coolPct 40 and growHotPct 80):
	// not hot enough to grow, not cool enough to reclaim → steady.
	s := calmSnap()
	s.areas = []area{
		{path: "/swapfile.odac.1", size: 2 * gib, used: 0},
		{path: "/swapfile.odac.2", size: 2 * gib, used: pct(2*gib, 50)},
	}
	st := &counters{}
	for i := 0; i < shrinkStreakNeeded+3; i++ {
		if d := decide(s, defaultCfg(), st); d.action != hold {
			t.Fatalf("hysteresis band should hold, got %v", d)
		}
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
	// Calm: no hot tier → want 1.
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 1 {
		t.Errorf("calm want = %d, want 1", got)
	}
	// RAM hot → want 2.
	s.memAvail = pct(8*gib, 13)
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 2 {
		t.Errorf("RAM hot want = %d, want 2", got)
	}
	// RAM hot + one hot swap → want 3.
	s.areas = []area{{path: "/swapfile.odac.1", size: gib, used: pct(gib, 90)}}
	if got := desiredBuffers(s, growHotPct, growPSIThreshold); got != 3 {
		t.Errorf("RAM+swap hot want = %d, want 3", got)
	}
	// PSI spike alone flags RAM hot even when memAvail looks fine.
	calm := calmSnap()
	calm.psiSomeAvg10 = 50
	if got := desiredBuffers(calm, growHotPct, growPSIThreshold); got != 2 {
		t.Errorf("PSI-hot want = %d, want 2", got)
	}
}
