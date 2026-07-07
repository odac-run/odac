package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// TestServerSpawnsDataPlaneModules verifies the 3.2 wiring end to end: with
// stub module binaries in ODAC_BIN_DIR, the orchestrator must spawn the proxy
// immediately after ready and mail one second later, claiming their PID
// files. DNS is not asserted — its spawn waits for IP detection, whose
// duration depends on the network environment.
func TestServerSpawnsDataPlaneModules(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test builds a binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("stub modules are shell scripts")
	}

	bin := filepath.Join(t.TempDir(), "odac-server")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	modDir := t.TempDir()
	for _, name := range []string{"odac-proxy", "odac-dns", "odac-mail"} {
		script := []byte("#!/bin/sh\nsleep 300\n")
		if err := os.WriteFile(filepath.Join(modDir, name), script, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	home := t.TempDir()
	runDir := filepath.Join(home, ".odac", "run")
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "ODAC_BIN_DIR="+modDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// The modules are detached and survive the server — kill via PID files.
		for _, name := range []string{"proxy", "dns", "mail"} {
			raw, err := os.ReadFile(filepath.Join(runDir, name+"-default.pid"))
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill()
				}
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
	}()

	for _, name := range []string{"proxy", "mail"} {
		pidFile := filepath.Join(runDir, name+"-default.pid")
		deadline := time.Now().Add(10 * time.Second)
		for {
			if raw, err := os.ReadFile(pidFile); err == nil {
				if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s pid file never claimed", name)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
