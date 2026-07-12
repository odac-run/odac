package main

import (
	"encoding/json"
	"net"
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
	// ODAC_API_ADDR :0 keeps parallel test runs (and a dev machine's real
	// server) off the contract port 1453.
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "ODAC_API_ADDR=127.0.0.1:0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	serverJSON := filepath.Join(home, ".odac", "config", "server.json")
	// Generous: under a fully parallel ./odac/... run the binary competes
	// with several concurrent go-build invocations for CPU.
	deadline := time.Now().Add(15 * time.Second)
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
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"ODAC_BIN_DIR="+modDir, "ODAC_API_ADDR=127.0.0.1:0")
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

// TestServerAPIRoundTrip verifies the 3.3 wiring end to end: the real server
// binary answers contract-0.1 requests over its TCP socket — dns.list with
// seeded zones using root auth from the config file, plus the unauthorized
// and unknown_action error paths.
func TestServerAPIRoundTrip(t *testing.T) {
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

	// Short HOME: api.sock must fit the unix socket path limit.
	home, err := os.MkdirTemp("", "oh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })

	// Seed a DNS zone before start; dns.list must serve it back.
	cfgDir := filepath.Join(home, ".odac", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"dns":{"seeded.test":{"soa":{"serial":2026010101},"records":[` +
		`{"id":"r1","name":"seeded.test","ttl":3600,"type":"A","value":"9.9.9.9"}]}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "dns.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// A just-freed port for the API listener.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiAddr := probe.Addr().String()
	probe.Close()

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "ODAC_API_ADDR="+apiAddr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// Root auth lands in api.json via autosave shortly after Init.
	var auth string
	deadline := time.Now().Add(10 * time.Second)
	for auth == "" {
		if raw, err := os.ReadFile(filepath.Join(cfgDir, "api.json")); err == nil {
			var parsed map[string]map[string]any
			if json.Unmarshal(raw, &parsed) == nil {
				auth, _ = parsed["api"]["auth"].(string)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("api.json never gained an auth token")
		}
		time.Sleep(20 * time.Millisecond)
	}

	call := func(payload string) map[string]json.RawMessage {
		t.Helper()
		var conn net.Conn
		deadline := time.Now().Add(10 * time.Second)
		for {
			var err error
			conn, err = net.DialTimeout("tcp", apiAddr, time.Second)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("API port never opened: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64*1024)
		n, _ := conn.Read(buf)
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(buf[:n], &resp); err != nil {
			t.Fatalf("bad response %q: %v", buf[:n], err)
		}
		return resp
	}

	resp := call(`{"auth":"` + auth + `","action":"dns.list","data":[]}`)
	if string(resp["result"]) != "true" {
		t.Fatalf("dns.list result = %s", resp["result"])
	}
	var zones map[string]map[string]any
	if err := json.Unmarshal(resp["data"], &zones); err != nil || zones["seeded.test"] == nil {
		t.Errorf("dns.list data = %s", resp["data"])
	}

	// app.list is registered since 3.4e; an empty install answers [].
	resp = call(`{"auth":"` + auth + `","action":"app.list","data":[]}`)
	if string(resp["result"]) != "true" || string(resp["data"]) != "[]" {
		t.Errorf("app.list = result %s data %s", resp["result"], resp["data"])
	}

	// ssl.renew stays unregistered until task 3.5.
	resp = call(`{"auth":"` + auth + `","action":"ssl.renew","data":[]}`)
	if string(resp["message"]) != `"unknown_action"` {
		t.Errorf("unregistered action = %s", resp["message"])
	}

	resp = call(`{"auth":"nope","action":"dns.list","data":[]}`)
	if string(resp["message"]) != `"unauthorized"` {
		t.Errorf("bad auth = %s", resp["message"])
	}

	// The unix socket serves the same protocol (skipped where the path
	// would exceed the sun_path limit — the short HOME should fit).
	if runtime.GOOS != "windows" {
		sock := filepath.Join(home, ".odac", "run", "api.sock")
		if _, err := os.Stat(sock); err == nil {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatalf("unix dial: %v", err)
			}
			defer conn.Close()
			conn.Write([]byte(`{"auth":"` + auth + `","action":"dns.list","data":[]}`))
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 64*1024)
			n, _ := conn.Read(buf)
			if !strings.Contains(string(buf[:n]), `"result":true`) {
				t.Errorf("unix socket reply = %s", buf[:n])
			}
		} else {
			t.Errorf("api.sock missing: %v", err)
		}
	}
}
