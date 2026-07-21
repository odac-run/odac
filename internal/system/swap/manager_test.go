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

type testErr struct{}

func (testErr) Error() string { return "boom" }

var errTest = testErr{}
