//go:build linux

package sysinfo

import "testing"

// TestParseMeminfoKB covers the used-memory fix: available comes from
// MemAvailable when present (it is the kernel's own estimate, not a sum we
// recompute), and falls back to MemFree+Buffers+Cached only on old kernels.
func TestParseMeminfoKB(t *testing.T) {
	// Modern kernel: MemAvailable is authoritative and deliberately less than
	// free+buffers+cached (the kernel discounts unreclaimable cache).
	modern := []byte("MemTotal:        4000000 kB\n" +
		"MemFree:          500000 kB\n" +
		"MemAvailable:    2000000 kB\n" +
		"Buffers:          100000 kB\n" +
		"Cached:          1200000 kB\n" +
		"SwapCached:            0 kB\n")
	total, free, avail := parseMeminfoKB(modern)
	if total != 4000000 || free != 500000 || avail != 2000000 {
		t.Fatalf("modern = total %d free %d avail %d", total, free, avail)
	}
	// The reported "used" (total-available) must match free's accounting, not
	// the inflated total-free.
	if used := total - avail; used != 2000000 {
		t.Errorf("used = %d, want 2000000 (total-free would wrongly give %d)",
			used, total-free)
	}

	// Pre-3.14 kernel: no MemAvailable → free+buffers+cached fallback.
	old := []byte("MemTotal:        4000000 kB\n" +
		"MemFree:          500000 kB\n" +
		"Buffers:          100000 kB\n" +
		"Cached:          1200000 kB\n")
	if _, _, a := parseMeminfoKB(old); a != 500000+100000+1200000 {
		t.Errorf("fallback available = %d, want 1800000", a)
	}

	// SwapCached must not be counted as Cached in the fallback path.
	if _, _, a := parseMeminfoKB([]byte("MemFree: 100 kB\nSwapCached: 999 kB\n")); a != 100 {
		t.Errorf("SwapCached leaked into available: %d, want 100", a)
	}
}
