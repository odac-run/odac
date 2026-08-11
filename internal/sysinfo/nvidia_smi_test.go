//go:build !windows

package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeHostRoot builds a stand-in for the host filesystem seen from inside
// ODAC's container and points the lookup at it.
func fakeHostRoot(t *testing.T, smiPath string) string {
	t.Helper()
	root := t.TempDir()
	if smiPath != "" {
		full := filepath.Join(root, smiPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous, previousLookPath := hostRootCandidates, lookPath
	hostRootCandidates = []string{filepath.Join(t.TempDir(), "absent"), root}
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { hostRootCandidates, lookPath = previous, previousLookPath })
	return root
}

// ODAC's Alpine image ships no nvidia-smi, so the host's binary is used —
// under a chroot, otherwise it would be loaded against the wrong userland.
func TestNvidiaSMICommandUsesHostBinary(t *testing.T) {
	root := fakeHostRoot(t, "usr/bin/nvidia-smi")

	cmd, ok := nvidiaSMICommand(context.Background(), "--query-gpu=memory.total")
	if !ok {
		t.Fatal("no command built")
	}
	if cmd.Path != "/usr/bin/nvidia-smi" {
		t.Errorf("path = %q, want the host-absolute path", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Chroot != root {
		t.Fatalf("chroot = %+v, want %q", cmd.SysProcAttr, root)
	}
	if cmd.Dir != "/" {
		t.Errorf("dir = %q, want / (the pre-chroot cwd is gone)", cmd.Dir)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != "--query-gpu=memory.total" {
		t.Errorf("args = %v", cmd.Args)
	}
}

// A host without the driver installed yields nothing, and the callers keep
// degrading to zero figures instead of erroring.
func TestNvidiaSMICommandAbsent(t *testing.T) {
	fakeHostRoot(t, "")

	if cmd, ok := nvidiaSMICommand(context.Background()); ok {
		t.Fatalf("built %v for a host with no nvidia-smi", cmd.Args)
	}
	if entries := runNvidiaSMI(); entries != nil {
		t.Errorf("runNvidiaSMI() = %+v, want nil", entries)
	}
	if entries := runNvidiaSMIStats(); entries != nil {
		t.Errorf("runNvidiaSMIStats() = %+v, want nil", entries)
	}
}

// The alternate install paths are probed in order.
func TestNvidiaSMICommandAlternatePath(t *testing.T) {
	fakeHostRoot(t, "usr/local/bin/nvidia-smi")

	cmd, ok := nvidiaSMICommand(context.Background())
	if !ok {
		t.Fatal("no command built")
	}
	if cmd.Path != "/usr/local/bin/nvidia-smi" {
		t.Errorf("path = %q", cmd.Path)
	}
}
