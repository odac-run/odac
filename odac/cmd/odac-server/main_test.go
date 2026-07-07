package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestServerSmoke builds the binary and runs it with an isolated HOME: it
// must write pid/started/os/arch into server.json via autosave and keep
// running (the skeleton has no exit path besides being killed).
func TestServerSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test builds a binary")
	}

	bin := filepath.Join(t.TempDir(), "odac-server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	home := t.TempDir()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	serverJSON := filepath.Join(home, ".odac", "config", "server.json")
	deadline := time.Now().Add(5 * time.Second)
	var srv map[string]any
	for {
		raw, err := os.ReadFile(serverJSON)
		if err == nil {
			var parsed map[string]map[string]any
			if json.Unmarshal(raw, &parsed) == nil && parsed["server"]["pid"] != nil {
				srv = parsed["server"]
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server.json never autosaved with a pid (last err: %v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if pid, _ := srv["pid"].(float64); int(pid) != cmd.Process.Pid {
		t.Errorf("server.pid = %v, want %d", srv["pid"], cmd.Process.Pid)
	}
	if started, _ := srv["started"].(float64); started <= 0 {
		t.Errorf("server.started = %v, want ms epoch", srv["started"])
	}
	if srv["os"] == "windows" || srv["arch"] == "amd64" {
		t.Errorf("server.os/arch not in Node vocabulary: %v/%v", srv["os"], srv["arch"])
	}

	// Still alive after startup settles (no early exit).
	time.Sleep(300 * time.Millisecond)
	if cmd.ProcessState != nil {
		t.Fatal("server exited prematurely")
	}
}
