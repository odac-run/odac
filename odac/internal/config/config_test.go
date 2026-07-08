package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAutoSavePersistsDirtyModules(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	stop := s.AutoSave(10*time.Millisecond, func(err error) { t.Errorf("autosave error: %v", err) })
	defer stop()

	srv := s.Map("server")
	srv["pid"] = 4242
	s.Touch("server")

	file := filepath.Join(dir, "config", "server.json")
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(file)
		if err == nil {
			var parsed map[string]map[string]any
			if json.Unmarshal(raw, &parsed) == nil && parsed["server"]["pid"] == float64(4242) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server.json not autosaved with pid; last content: %s", raw)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop must halt further saves: dirty a module, stop, verify no write.
	stop()
	stop() // idempotent
	srv["pid"] = 9999
	s.Touch("server")
	time.Sleep(50 * time.Millisecond)
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "9999") {
		t.Error("autosave kept writing after stop()")
	}
}

// TestViewMutateSerializeWithSave hammers deep-value mutation against
// SaveDirty's marshal — the exact race the 3.3 API handlers introduce.
// Meaningful under -race: without the value lock this reports a data race
// or a concurrent-map panic inside encoding/json.
func TestViewMutateSerializeWithSave(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("dns", map[string]any{"zone": map[string]any{"records": []any{}}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.Mutate(func() {
				zone := s.Map("dns")["zone"].(map[string]any)
				records := zone["records"].([]any)
				zone["records"] = append(records, map[string]any{"i": i})
				s.Touch("dns")
			})
		}
	}()

	for i := 0; i < 200; i++ {
		if err := s.SaveDirty(); err != nil {
			t.Fatal(err)
		}
		s.View(func() {
			zone := s.Map("dns")["zone"].(map[string]any)
			_ = len(zone["records"].([]any))
		})
	}
	<-done

	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	zone, _ := s.Map("dns")["zone"].(map[string]any)
	records, _ := zone["records"].([]any)
	if len(records) != 200 {
		t.Errorf("persisted records = %d, want 200", len(records))
	}
}
