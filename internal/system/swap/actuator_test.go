package swap

import "strings"

import "testing"

const userFstab = "UUID=abc / ext4 defaults 0 1\n" +
	"UUID=def /home ext4 defaults 0 2\n"

func TestUpdateFstabBlockAdds(t *testing.T) {
	got := updateFstabBlock(userFstab, []string{"/swapfile.odac.1", "/swapfile.odac.2"})

	// User lines are preserved verbatim.
	if !strings.HasPrefix(got, userFstab) {
		t.Fatalf("user lines not preserved:\n%s", got)
	}
	// Both swapfiles present with the nofail options and inside the markers.
	for _, want := range []string{
		fstabBegin,
		"/swapfile.odac.1 " + fstabOpts,
		"/swapfile.odac.2 " + fstabOpts,
		fstabEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "nofail") {
		t.Error("nofail safety option missing")
	}
}

func TestUpdateFstabBlockIsIdempotent(t *testing.T) {
	paths := []string{"/swapfile.odac.1"}
	once := updateFstabBlock(userFstab, paths)
	twice := updateFstabBlock(once, paths)
	if once != twice {
		t.Errorf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestUpdateFstabBlockReplacesOldSet(t *testing.T) {
	// Grow to two, then shrink back to one: the removed increment's line must
	// be gone and the block must contain only the current set.
	grown := updateFstabBlock(userFstab, []string{"/swapfile.odac.1", "/swapfile.odac.2"})
	shrunk := updateFstabBlock(grown, []string{"/swapfile.odac.1"})
	if strings.Contains(shrunk, "/swapfile.odac.2") {
		t.Errorf("stale increment .2 not removed:\n%s", shrunk)
	}
	if !strings.Contains(shrunk, "/swapfile.odac.1 "+fstabOpts) {
		t.Errorf("current increment .1 missing:\n%s", shrunk)
	}
}

func TestUpdateFstabBlockClearsToUserOnly(t *testing.T) {
	grown := updateFstabBlock(userFstab, []string{"/swapfile.odac.1"})
	cleared := updateFstabBlock(grown, nil)
	if strings.Contains(cleared, fstabBegin) || strings.Contains(cleared, "swapfile.odac") {
		t.Errorf("managed block not fully cleared:\n%s", cleared)
	}
	// The original user content survives, with no accumulated blank lines.
	if strings.TrimRight(cleared, "\n") != strings.TrimRight(userFstab, "\n") {
		t.Errorf("user content altered by clear:\n%q", cleared)
	}
}

func TestRemoveFstabBlockToleratesMissingEndMarker(t *testing.T) {
	corrupt := userFstab + fstabBegin + "\n/swapfile.odac.1 " + fstabOpts + "\n" // no end marker
	got := removeFstabBlock(corrupt)
	if strings.Contains(got, "swapfile.odac") || strings.Contains(got, fstabBegin) {
		t.Errorf("corrupt block (no end marker) not cleaned:\n%s", got)
	}
	if strings.TrimRight(got, "\n") != strings.TrimRight(userFstab, "\n") {
		t.Errorf("user content damaged:\n%q", got)
	}
}
