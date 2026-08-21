package blob

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// fakeRefs answers reference lookups from a fixed set, and can be made to fail.
type fakeRefs struct {
	live map[string]bool
	err  error
}

func (f *fakeRefs) RawRefExists(_ context.Context, ref string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.live[ref], nil
}

// age backdates a blob so the grace period no longer protects it.
func age(t *testing.T, s *Store, ref string, d time.Duration) {
	t.Helper()
	p, err := s.path(ref)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	old := time.Now().Add(-d)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestSweepRemovesOnlyUnreferencedBlobs(t *testing.T) {
	s := newTestStore(t)
	live, _ := s.Put([]byte("still referenced"))
	orphan, _ := s.Put([]byte("nobody points here"))
	age(t, s, live, 48*time.Hour)
	age(t, s, orphan, 48*time.Hour)

	sw := &Sweeper{store: s, refs: &fakeRefs{live: map[string]bool{live: true}}, grace: defaultGrace}
	deleted, freed := sw.Sweep(context.Background())

	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if freed != int64(len("nobody points here")) {
		t.Errorf("freed = %d", freed)
	}
	if !s.Has(live) {
		t.Error("referenced blob was deleted")
	}
	if s.Has(orphan) {
		t.Error("orphan survived the sweep")
	}
}

// A blob written moments ago may belong to a delivery whose row is not
// committed yet, so the grace period must protect it.
func TestSweepSpareBlobsInsideGracePeriod(t *testing.T) {
	s := newTestStore(t)
	fresh, _ := s.Put([]byte("just delivered"))

	sw := &Sweeper{store: s, refs: &fakeRefs{live: map[string]bool{}}, grace: defaultGrace}
	if deleted, _ := sw.Sweep(context.Background()); deleted != 0 {
		t.Errorf("deleted = %d, want 0 inside the grace period", deleted)
	}
	if !s.Has(fresh) {
		t.Error("a blob inside the grace period was deleted")
	}
}

// A failed lookup must never be read as "unreferenced"; that would delete mail.
func TestSweepKeepsBlobsWhenLookupFails(t *testing.T) {
	s := newTestStore(t)
	ref, _ := s.Put([]byte("database is down"))
	age(t, s, ref, 48*time.Hour)

	sw := &Sweeper{store: s, refs: &fakeRefs{err: errors.New("database unavailable")}, grace: defaultGrace}
	if deleted, _ := sw.Sweep(context.Background()); deleted != 0 {
		t.Errorf("deleted = %d, want 0 when the reference check fails", deleted)
	}
	if !s.Has(ref) {
		t.Error("blob deleted despite a failed reference check")
	}
}

func TestSweepStopsOnCancelledContext(t *testing.T) {
	s := newTestStore(t)
	ref, _ := s.Put([]byte("orphan"))
	age(t, s, ref, 48*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sw := &Sweeper{store: s, refs: &fakeRefs{live: map[string]bool{}}, grace: defaultGrace}
	if deleted, _ := sw.Sweep(ctx); deleted != 0 {
		t.Errorf("deleted = %d, want 0 for a cancelled sweep", deleted)
	}
	if !s.Has(ref) {
		t.Error("cancelled sweep still deleted a blob")
	}
}

func TestSweeperStartStopIsClean(t *testing.T) {
	s := newTestStore(t)
	sw := NewSweeper(s, &fakeRefs{live: map[string]bool{}})
	sw.interval = 10 * time.Millisecond

	sw.Start()
	sw.Start() // second Start must not spawn a second goroutine
	time.Sleep(30 * time.Millisecond)
	sw.Stop()
	sw.Stop() // Stop must be safe to call twice
}

func TestSweeperDisabledByEnv(t *testing.T) {
	t.Setenv("ODAC_MAIL_SWEEP_HOURS", "0")
	sw := NewSweeper(newTestStore(t), &fakeRefs{})
	if sw.interval != 0 {
		t.Fatalf("interval = %s, want 0", sw.interval)
	}
	sw.Start()
	defer sw.Stop()
	if sw.cancel != nil {
		t.Error("a disabled sweeper must not start a goroutine")
	}
}
