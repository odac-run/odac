package sysinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"odac/internal/jscanon"
)

// TestVersionMatchesPackageJSON is the drift guard: the embedded Version
// must track the repo's package.json (same policy as the 2.4 locale
// catalogs).
func TestVersionMatchesPackageJSON(t *testing.T) {
	raw, err := os.ReadFile("../../../package.json")
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version != Version {
		t.Fatalf("sysinfo.Version = %s but package.json says %s — update version.go/sysinfo.go", Version, pkg.Version)
	}
}

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
