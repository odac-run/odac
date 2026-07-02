package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAppliesDefaults(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := s.Map("server")
	if srv == nil {
		t.Fatal("server default missing")
	}
	for _, k := range []string{"pid", "started", "watchdog"} {
		if v, ok := srv[k]; !ok || v != nil {
			t.Errorf("server.%s = %v, want present nil", k, v)
		}
	}

	if apps, ok := s.Get("apps").([]any); !ok || len(apps) != 0 {
		t.Errorf("apps default = %v, want empty array", s.Get("apps"))
	}

	fw := s.Map("firewall")
	if fw == nil || fw["enabled"] != true {
		t.Errorf("firewall default = %v, want enabled:true", fw)
	}
	rl, _ := fw["rateLimit"].(map[string]any)
	if rl == nil || rl["windowMs"] != 60000 || rl["max"] != 1000 {
		t.Errorf("firewall.rateLimit default = %v", rl)
	}
}

func TestSaveFormatAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.Map("server")
	srv["watchdog"] = 123
	srv["started"] = int64(1751433600000)
	s.Touch("server")
	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Contract 0.6: module files wrap their keys and use 4-space indent.
	if !strings.Contains(string(raw), "\n    \"server\": {") {
		t.Errorf("server.json not wrapped/indented as expected:\n%s", raw)
	}
	if strings.Contains(string(raw), "e+") || strings.Contains(string(raw), ".0") {
		t.Errorf("ms-epoch written in non-integer form:\n%s", raw)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Map("server")
	if v, _ := got["watchdog"].(float64); int(v) != 123 {
		t.Errorf("reloaded server.watchdog = %v, want 123", got["watchdog"])
	}
	if v, _ := got["started"].(float64); int64(v) != 1751433600000 {
		t.Errorf("reloaded server.started = %v, want 1751433600000", got["started"])
	}
}

func TestSaveDirtyOnlyWritesDirtyModules(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	s.Set("hub", map[string]any{"token": "t"})
	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "config", "hub.json")); err != nil {
		t.Errorf("hub.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "dns.json")); !os.IsNotExist(err) {
		t.Errorf("dns.json written although untouched")
	}
}

func TestCorruptionRecoveryFromBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// First save creates the file; second save (with the file existing)
	// snapshots version 1 into .bak before overwriting.
	s.Set("hub", map[string]any{"token": "v1"})
	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	s.Set("hub", map[string]any{"token": "v2"})
	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}

	mainFile := filepath.Join(dir, "config", "hub.json")
	if err := os.WriteFile(mainFile, []byte("{corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := recovered.Map("hub")
	if hub["token"] != "v1" {
		t.Errorf("recovered hub.token = %v, want v1 (from backup)", hub["token"])
	}
	if _, err := os.Stat(mainFile + ".corrupted"); err != nil {
		t.Errorf("forensic .corrupted copy missing: %v", err)
	}
	// Main file must have been restored from the backup.
	raw, _ := os.ReadFile(mainFile)
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		t.Errorf("main file not restored to valid JSON: %s", raw)
	}
}

func TestBothCorruptedFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "firewall.json"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fw := s.Map("firewall")
	if fw == nil || fw["enabled"] != true {
		t.Errorf("firewall not defaulted after double corruption: %v", fw)
	}
}
