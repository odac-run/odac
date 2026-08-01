package swap

import "testing"

func TestParseMeminfoBytes(t *testing.T) {
	raw := []byte("MemTotal:        4000000 kB\n" +
		"MemFree:          500000 kB\n" +
		"MemAvailable:    2000000 kB\n" +
		"Buffers:          100000 kB\n" +
		"Cached:          1200000 kB\n" +
		"SwapTotal:       1000000 kB\n" +
		"SwapFree:         600000 kB\n")
	total, avail, swapTotal, swapFree := parseMeminfoBytes(raw)
	if total != 4000000*kib || avail != 2000000*kib {
		t.Errorf("mem = total %d avail %d", total, avail)
	}
	if swapTotal != 1000000*kib || swapFree != 600000*kib {
		t.Errorf("swap = total %d free %d", swapTotal, swapFree)
	}

	// Pre-3.14: no MemAvailable → Free+Buffers+Cached, and SwapCached is not
	// mistaken for Cached.
	old := []byte("MemTotal: 4000000 kB\nMemFree: 500000 kB\n" +
		"Buffers: 100000 kB\nCached: 1200000 kB\nSwapCached: 999 kB\n")
	if _, a, _, _ := parseMeminfoBytes(old); a != (500000+100000+1200000)*kib {
		t.Errorf("fallback available = %d", a)
	}
}

func TestParsePressureSomeAvg10(t *testing.T) {
	raw := []byte("some avg10=12.34 avg60=5.00 avg300=1.00 total=123456\n" +
		"full avg10=8.00 avg60=2.00 avg300=0.50 total=6789\n")
	if got := parsePressureSomeAvg10(raw); got != 12.34 {
		t.Errorf("some avg10 = %v, want 12.34", got)
	}
	// Absent / disabled PSI → 0.
	if got := parsePressureSomeAvg10([]byte("")); got != 0 {
		t.Errorf("empty PSI = %v, want 0", got)
	}
}

func TestParseSwaps(t *testing.T) {
	// Header + a user partition + a user swapfile + two odac increments (out of
	// order to exercise the LIFO sort). Only the odac rows survive.
	raw := []byte("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
		"/dev/sda2                               partition\t2000000\t0\t-2\n" +
		"/home/user/myswap                       file\t1000000\t100\t-3\n" +
		"/swapfile.odac.2                        file\t2097152\t524288\t-4\n" +
		"/swapfile.odac.1                        file\t1048576\t0\t-5\n")
	areas := parseSwaps(raw, "/swapfile.odac.")
	if len(areas) != 2 {
		t.Fatalf("got %d areas, want 2 (odac only): %+v", len(areas), areas)
	}
	// Ascending by index: .1 then .2.
	if areas[0].path != "/swapfile.odac.1" || areas[1].path != "/swapfile.odac.2" {
		t.Errorf("not sorted by index: %+v", areas)
	}
	if areas[0].size != 1048576*kib || areas[1].used != 524288*kib {
		t.Errorf("KB→bytes conversion wrong: %+v", areas)
	}
}

func TestParseSwapsContainerPathRewrite(t *testing.T) {
	// Inside a container, /proc/swaps renders our bind-mounted swapfile under the
	// mount's source root ("/.odac/swap/...") instead of the path we created it at
	// ("/app/.odac/swap/..."). Ownership must still be recognized by basename, and
	// the area path rebuilt to the real, accessible path under our dir — otherwise
	// the manager never sees its own baseline and loops recreating it (busy).
	raw := []byte("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
		"/.odac/swap/swapfile.odac.1             file\t1948572\t948480\t-2\n")
	prefix := "/app/.odac/swap/swapfile.odac."
	areas := parseSwaps(raw, prefix)
	if len(areas) != 1 {
		t.Fatalf("got %d areas, want 1 despite path rewrite: %+v", len(areas), areas)
	}
	if areas[0].path != "/app/.odac/swap/swapfile.odac.1" {
		t.Errorf("path not rebuilt under dir: %q", areas[0].path)
	}
	if areas[0].size != 1948572*kib || areas[0].used != 948480*kib {
		t.Errorf("size/used wrong: %+v", areas[0])
	}
}

func TestIncrementIndex(t *testing.T) {
	if idx, ok := incrementIndex("swapfile.odac.3"); !ok || idx != "3" {
		t.Errorf("swapfile.odac.3 = %q %v, want 3 true", idx, ok)
	}
	// Nothing downstream may adopt or delete these.
	for _, name := range []string{"swapfile.odac.bak", "swapfile.odac.", "myswap", "swapfile"} {
		if _, ok := incrementIndex(name); ok {
			t.Errorf("%q must not be recognized as an increment", name)
		}
	}
}

func TestIndexOf(t *testing.T) {
	if indexOf("/swapfile.odac.7", "/swapfile.odac.") != 7 {
		t.Error("indexOf .7")
	}
	if indexOf("/swapfile.odac.bogus", "/swapfile.odac.") != -1 {
		t.Error("non-numeric suffix should be -1")
	}
}

func TestSwapPrefix(t *testing.T) {
	cases := map[string]string{
		"/app/.odac/swap":  "/app/.odac/swap/swapfile.odac.",
		"/app/.odac/swap/": "/app/.odac/swap/swapfile.odac.",
		"":                 "/swapfile.odac.",
	}
	for dir, want := range cases {
		if got := swapPrefix(dir); got != want {
			t.Errorf("swapPrefix(%q) = %q, want %q", dir, got, want)
		}
	}
}
