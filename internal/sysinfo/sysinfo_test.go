package sysinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odac/internal/jscanon"
)

// TestGetShape pins the field set and Node's literal key order.
func TestGetShape(t *testing.T) {
	info := New(func() bool { return true }).Get()

	wantOrder := []string{
		"arch", "container_engine", "cpu", "hostname", "load", "memory",
		"node", "platform", "release", "uptime", "version",
	}
	if len(info) < len(wantOrder) {
		t.Fatalf("info has %d fields: %v", len(info), info)
	}
	for i, key := range wantOrder {
		if info[i].K != key {
			t.Errorf("field %d = %s, want %s", i, info[i].K, key)
		}
	}
	// The optional distro field can only trail the fixed set.
	if len(info) > len(wantOrder) && info[len(info)-1].K != "distro" {
		t.Errorf("unexpected trailing field %s", info[len(info)-1].K)
	}

	// The payload must be jscanon-encodable (it feeds the signed
	// system.info message and the auth body).
	raw, err := jscanon.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("info not valid JSON: %v\n%s", err, raw)
	}
	if parsed["container_engine"] != true || parsed["version"] != Version {
		t.Errorf("payload values: %s", raw)
	}
	if cpu, _ := parsed["cpu"].(map[string]any); cpu["count"] == float64(0) {
		t.Errorf("cpu count missing: %s", raw)
	}
	if load, _ := parsed["load"].([]any); len(load) != 3 {
		t.Errorf("load shape: %s", raw)
	}
}

func TestParseOSRelease(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	ubuntu := write("ubuntu", "NAME=\"Ubuntu\"\nVERSION_ID=\"20.04\"\nID=ubuntu\n")
	got := parseOSRelease([]string{ubuntu})
	if got["name"] != "Ubuntu" || got["version"] != "20.04" || got["id"] != "ubuntu" {
		t.Fatalf("ubuntu = %v", got)
	}

	// VERSION fallback + missing ID.
	debianish := write("debianish", "NAME=Debian\nVERSION=\"12 (bookworm)\"\n")
	got = parseOSRelease([]string{debianish})
	if got["name"] != "Debian" || got["version"] != "12 (bookworm)" || got["id"] != "unknown" {
		t.Fatalf("debianish = %v", got)
	}

	// ID-only file: name falls back to id.
	idOnly := write("idonly", "ID=alpine\n")
	got = parseOSRelease([]string{idOnly})
	if got["name"] != "alpine" || got["version"] != "Unknown" {
		t.Fatalf("id-only = %v", got)
	}

	// Empty/garbage candidates walk to the next; all-failing → nil.
	empty := write("empty", "\n# nothing\n")
	if got = parseOSRelease([]string{empty, ubuntu}); got["id"] != "ubuntu" {
		t.Fatalf("candidate walk = %v", got)
	}
	if got = parseOSRelease([]string{empty, filepath.Join(dir, "missing")}); got != nil {
		t.Fatalf("all-failing = %v", got)
	}
}

func TestHostPlatformVocabulary(t *testing.T) {
	// On any dev/CI host the answer must be Node os.platform() vocabulary.
	switch got := HostPlatform(); got {
	case "linux", "darwin", "win32", "freebsd":
	default:
		t.Errorf("HostPlatform() = %q, not Node vocabulary", got)
	}

	if nodeOS("windows") != "win32" || nodeOS("linux") != "linux" {
		t.Error("nodeOS mapping broken")
	}
	if nodeArch("amd64") != "x64" || nodeArch("386") != "ia32" || nodeArch("arm64") != "arm64" {
		t.Error("nodeArch mapping broken")
	}
}

// TestStatsShape pins the system.stats field set/order and confirms the
// payload is jscanon-encodable with numeric leaves.
func TestStatsShape(t *testing.T) {
	stats := New(nil).Stats()

	wantOrder := []string{"cpu", "disk", "memory", "network"}
	if len(stats) != len(wantOrder) {
		t.Fatalf("stats has %d fields: %v", len(stats), stats)
	}
	for i, key := range wantOrder {
		if stats[i].K != key {
			t.Errorf("field %d = %s, want %s", i, stats[i].K, key)
		}
	}

	raw, err := jscanon.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("stats not valid JSON: %v\n%s", err, raw)
	}
	if _, ok := parsed["cpu"].(float64); !ok {
		t.Errorf("cpu not numeric: %s", raw)
	}
	for _, group := range []string{"disk", "memory"} {
		m, ok := parsed[group].(map[string]any)
		if !ok {
			t.Fatalf("%s missing object: %s", group, raw)
		}
		if _, ok := m["used"].(float64); !ok {
			t.Errorf("%s.used not numeric: %s", group, raw)
		}
		if _, ok := m["total"].(float64); !ok {
			t.Errorf("%s.total not numeric: %s", group, raw)
		}
	}
	net, ok := parsed["network"].(map[string]any)
	if !ok {
		t.Fatalf("network missing object: %s", raw)
	}
	if _, ok := net["download"].(float64); !ok {
		t.Errorf("network.download not numeric: %s", raw)
	}
	if _, ok := net["upload"].(float64); !ok {
		t.Errorf("network.upload not numeric: %s", raw)
	}
}

// TestCpuUsageDelta covers Node's getCpuUsage guards: the first sample has no
// baseline (0), a rolled-back counter yields 0, and a normal window reports
// the clamped busy percentage.
func TestCpuUsageDelta(t *testing.T) {
	// Stub the platform collector with a scripted sequence.
	var ticks []struct{ idle, total int64 }
	orig := cpuTicksFn
	cpuTicksFn = func() (int64, int64, bool) {
		if len(ticks) == 0 {
			return 0, 0, false
		}
		s := ticks[0]
		ticks = ticks[1:]
		return s.idle, s.total, true
	}
	defer func() { cpuTicksFn = orig }()

	i := &Info{}

	// First sample: baseline only.
	ticks = []struct{ idle, total int64 }{{idle: 100, total: 200}}
	if got := i.cpuUsage(); got != 0 {
		t.Errorf("first sample = %d, want 0", got)
	}

	// Window: idle +10, total +100 → 90% busy.
	ticks = []struct{ idle, total int64 }{{idle: 110, total: 300}}
	if got := i.cpuUsage(); got != 90 {
		t.Errorf("busy window = %d, want 90", got)
	}

	// Rolled-back idle counter → 0 (guard), and re-baselines.
	ticks = []struct{ idle, total int64 }{{idle: 50, total: 400}}
	if got := i.cpuUsage(); got != 0 {
		t.Errorf("rollback = %d, want 0", got)
	}
}

// TestNetworkUsageDelta covers Node's getNetworkUsage guards: first sample is
// 0/0, a normal window divides the byte delta by elapsed seconds (rounded),
// and a rolled-back counter yields 0/0 while re-baselining.
func TestNetworkUsageDelta(t *testing.T) {
	var samples []struct{ recv, sent int64 }
	orig := netStatsFn
	netStatsFn = func() (int64, int64, bool) {
		if len(samples) == 0 {
			return 0, 0, false
		}
		s := samples[0]
		samples = samples[1:]
		return s.recv, s.sent, true
	}
	defer func() { netStatsFn = orig }()

	clock := time.Unix(1000, 0)
	i := &Info{now: func() time.Time { return clock }}

	dl := func(o jscanon.Obj) int64 { return o[0].V.(int64) }
	ul := func(o jscanon.Obj) int64 { return o[1].V.(int64) }

	// First sample: baseline only.
	samples = []struct{ recv, sent int64 }{{recv: 1000, sent: 500}}
	if o := i.networkUsage(); dl(o) != 0 || ul(o) != 0 {
		t.Errorf("first sample = %v, want 0/0", o)
	}

	// 4s window, +8000 recv / +2000 sent → 2000 down, 500 up.
	clock = clock.Add(4 * time.Second)
	samples = []struct{ recv, sent int64 }{{recv: 9000, sent: 2500}}
	if o := i.networkUsage(); dl(o) != 2000 || ul(o) != 500 {
		t.Errorf("window = %v, want 2000/500", o)
	}

	// Counter reset (interface reinit) → 0/0 guard.
	clock = clock.Add(2 * time.Second)
	samples = []struct{ recv, sent int64 }{{recv: 10, sent: 5}}
	if o := i.networkUsage(); dl(o) != 0 || ul(o) != 0 {
		t.Errorf("reset = %v, want 0/0", o)
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		s, word string
		want    bool
	}{
		{"none /mnt osxfs rw", "osxfs", true},
		{"fuse.osxfs rw", "osxfs", true}, // \b matches at the dot
		{"notosxfs rw", "osxfs", false},
		{"osxfsx rw", "osxfs", false},
		{"drvfs", "drvfs", true},
	}
	for _, tc := range cases {
		if got := containsWord(tc.s, tc.word); got != tc.want {
			t.Errorf("containsWord(%q, %q) = %v", tc.s, tc.word, got)
		}
	}
}
