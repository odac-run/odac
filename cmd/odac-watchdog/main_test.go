package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serverCommand picks the Go orchestrator only when an executable odac-server
// sits next to this binary — the single difference between the production
// Node image and a 3.8 staging image.
func TestServerCommandPrefersSiblingGoServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probe is unix-only")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// serverCommand resolves symlinks (darwin: /var → /private/var).
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	sibling := filepath.Join(filepath.Dir(exe), "odac-server")

	// No sibling: the Node fallback holds.
	os.Remove(sibling)
	if cmd := serverCommand(); cmd[0] != "node" {
		t.Fatalf("without a sibling binary serverCommand = %v, want node fallback", cmd)
	}

	// Executable sibling: it wins.
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(sibling) })
	if cmd := serverCommand(); cmd[0] != sibling {
		t.Fatalf("with a sibling binary serverCommand = %v, want %q", cmd, sibling)
	}

	// A non-executable file must not be supervised.
	if err := os.Chmod(sibling, 0o644); err != nil {
		t.Fatal(err)
	}
	if cmd := serverCommand(); cmd[0] != "node" {
		t.Fatalf("with a non-executable sibling serverCommand = %v, want node fallback", cmd)
	}

	// ODAC_SERVER_SCRIPT forces a Node entrypoint over everything.
	os.Chmod(sibling, 0o755)
	t.Setenv("ODAC_SERVER_SCRIPT", "/srv/index.js")
	cmd := serverCommand()
	if cmd[0] != "node" || !strings.HasSuffix(cmd[1], "/srv/index.js") {
		t.Fatalf("with ODAC_SERVER_SCRIPT serverCommand = %v, want node /srv/index.js", cmd)
	}
}
