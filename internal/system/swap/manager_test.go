package swap

import (
	"testing"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// fakeController records what the Manager asks it to do.
type fakeController struct {
	opened  []string
	sizes   []int64
	removed []string
	fstab   [][]string
	openErr error

	onDisk      []diskFile // what scan reports
	activated   []string
	discarded   []string
	activateErr error
}

func (f *fakeController) open(path string, size int64) error {
	if f.openErr != nil {
		return f.openErr
	}
	f.opened = append(f.opened, path)
	f.sizes = append(f.sizes, size)
	return nil
}
func (f *fakeController) remove(path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *fakeController) syncFstab(paths []string) error {
	f.fstab = append(f.fstab, append([]string(nil), paths...))
	return nil
}
func (f *fakeController) scan(string) ([]diskFile, error) { return f.onDisk, nil }
func (f *fakeController) activate(path string) error {
	if f.activateErr != nil {
		return f.activateErr
	}
	f.activated = append(f.activated, path)
	return nil
}
func (f *fakeController) discard(path string) (int64, error) {
	f.discarded = append(f.discarded, path)
	return gib, nil
}

func newTestManager(t *testing.T, ctl controller) *Manager {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{
		cfg:  cfg,
		ctl:  ctl,
		log:  logx.New("SwapTest"),
		now:  time.Now,
		gate: checkGate,
	}
}

func TestManagerSelfGate(t *testing.T) {
	f := &fakeController{}
	m := newTestManager(t, f)

	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }

	// enact a grow directly to prove the gate — but here we drive Check with a
	// controlled clock. First Check runs (lastRun zero).
	m.Check()
	// A Check 5s later must be gated out (no second run). We can't easily
	// observe "ran" without a snapshot, so assert lastRun advanced only once.
	first := m.lastRun
	clock = clock.Add(5 * time.Second)
	m.Check()
	if m.lastRun != first {
		t.Errorf("Check within gate should be a no-op, lastRun moved to %v", m.lastRun)
	}
	// Past the gate it runs again.
	clock = clock.Add(checkGate)
	m.Check()
	if m.lastRun == first {
		t.Error("Check past the gate should have run")
	}
}

func TestManagerEnactGrowPersists(t *testing.T) {
	f := &fakeController{}
	m := newTestManager(t, f)

	s := snapshot{
		areas: []area{{path: "/swapfile.odac.1", size: 1 * gib}},
	}
	m.enact(decision{action: grow, size: 2 * gib, reason: "test"}, s, defaultCfg(), "")

	if len(f.opened) != 1 || f.opened[0] != "/swapfile.odac.2" || f.sizes[0] != 2*gib {
		t.Fatalf("grow should open .2 at 2 GiB: %+v %+v", f.opened, f.sizes)
	}
	// fstab synced to the full set including the new increment.
	if len(f.fstab) != 1 || len(f.fstab[0]) != 2 ||
		f.fstab[0][1] != "/swapfile.odac.2" {
		t.Errorf("fstab not synced to {.1,.2}: %+v", f.fstab)
	}
}

func TestManagerEnactShrinkPersists(t *testing.T) {
	f := &fakeController{}
	m := newTestManager(t, f)

	s := snapshot{areas: []area{
		{path: "/swapfile.odac.1"}, {path: "/swapfile.odac.2"},
	}}
	m.enact(decision{action: shrink, target: "/swapfile.odac.2", reason: "test"}, s, defaultCfg(), "")

	if len(f.removed) != 1 || f.removed[0] != "/swapfile.odac.2" {
		t.Fatalf("shrink should remove .2: %+v", f.removed)
	}
	if len(f.fstab) != 1 || len(f.fstab[0]) != 1 || f.fstab[0][0] != "/swapfile.odac.1" {
		t.Errorf("fstab not synced to {.1}: %+v", f.fstab)
	}
}

func TestManagerGrowFailureNoPersist(t *testing.T) {
	f := &fakeController{openErr: errTest}
	m := newTestManager(t, f)
	m.enact(decision{action: grow, size: gib}, snapshot{}, defaultCfg(), "")
	if len(f.fstab) != 0 {
		t.Error("failed grow must not touch fstab")
	}
}

func TestManagerNoPersistWhenDisabled(t *testing.T) {
	f := &fakeController{}
	m := newTestManager(t, f)
	cfg := defaultCfg()
	cfg.Persist = false
	m.enact(decision{action: grow, size: gib}, snapshot{}, cfg, "")
	if len(f.opened) != 1 {
		t.Fatal("grow should still open")
	}
	if len(f.fstab) != 0 {
		t.Error("persist disabled: fstab must not be synced")
	}
}

// rebootSnapshot is the state right after a reboot: no swap active, but our
// swapfiles are still on disk.
func rebootSnapshot() snapshot {
	return snapshot{ok: true, memTotal: 2 * gib, memAvail: gib, freeDisk: 20 * gib}
}

func TestManagerReconcileAdoptsAfterReboot(t *testing.T) {
	f := &fakeController{onDisk: []diskFile{
		{path: "/swap/swapfile.odac.2", idx: 2, size: gib},
		{path: "/swap/swapfile.odac.1", idx: 1, size: gib},
	}}
	m := newTestManager(t, f)

	if !m.reconcile(defaultCfg(), "/swap", rebootSnapshot()) {
		t.Fatal("reconcile should report a change")
	}
	// Both come back, baseline first, and nothing is deleted.
	if len(f.activated) != 2 ||
		f.activated[0] != "/swap/swapfile.odac.1" || f.activated[1] != "/swap/swapfile.odac.2" {
		t.Errorf("both swapfiles should be adopted in index order: %+v", f.activated)
	}
	if len(f.discarded) != 0 {
		t.Errorf("adoptable swapfiles must never be deleted: %+v", f.discarded)
	}
}

func TestManagerReconcileLeavesActiveAlone(t *testing.T) {
	f := &fakeController{onDisk: []diskFile{{path: "/swap/swapfile.odac.1", idx: 1, size: gib}}}
	m := newTestManager(t, f)

	s := rebootSnapshot()
	s.areas = []area{{path: "/swap/swapfile.odac.1", size: gib}}

	if m.reconcile(defaultCfg(), "/swap", s) {
		t.Error("already-active swap needs no reconcile")
	}
	if len(f.activated) != 0 || len(f.discarded) != 0 {
		t.Errorf("live swap must not be touched: %+v %+v", f.activated, f.discarded)
	}
}

func TestManagerReconcileDiscardsCorrupt(t *testing.T) {
	f := &fakeController{
		onDisk:      []diskFile{{path: "/swap/swapfile.odac.1", idx: 1, size: gib}},
		activateErr: errTest,
	}
	m := newTestManager(t, f)

	// mkswap could not rescue it, so it is reclaimed instead of retried forever.
	if !m.reconcile(defaultCfg(), "/swap", rebootSnapshot()) {
		t.Fatal("a discard is still a change")
	}
	if len(f.discarded) != 1 || f.discarded[0] != "/swap/swapfile.odac.1" {
		t.Errorf("unusable swapfile should be discarded: %+v", f.discarded)
	}
}

func TestManagerReconcileSkippedWhenUnmanaged(t *testing.T) {
	f := &fakeController{onDisk: []diskFile{{path: "/swap/swapfile.odac.1", idx: 1, size: gib}}}
	m := newTestManager(t, f)

	cfg := defaultCfg()
	cfg.AutoManage = false
	if m.reconcile(cfg, "/swap", rebootSnapshot()) {
		t.Error("auto-manage off must reconcile nothing")
	}
	// An unreadable snapshot holds too, even with auto-manage on.
	if m.reconcile(defaultCfg(), "/swap", snapshot{ok: false}) {
		t.Error("no snapshot must reconcile nothing")
	}
	if len(f.activated) != 0 || len(f.discarded) != 0 {
		t.Errorf("hands off the host: %+v %+v", f.activated, f.discarded)
	}
}

type testErr struct{}

func (testErr) Error() string { return "boom" }

var errTest = testErr{}
