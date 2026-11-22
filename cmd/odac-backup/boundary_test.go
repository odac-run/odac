package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
)

// engineOnlyModules may be linked into odac-backup alone. Every other
// binary runs for the life of the server (or on every CLI call), and the
// backup engine is a large tree that has no business in any of them.
var engineOnlyModules = []string{"github.com/kopia/", "github.com/studio-b12/gowebdav", "github.com/minio/"}

func TestBackupEngineStaysInOdacBackup(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	// The server links the manager and the service and through them proto,
	// thin, runner, consistency and settings; checking them directly catches a slip
	// before the server starts importing them.
	for _, bin := range []string{"cmd/odac", "cmd/odac-server", "cmd/odac-watchdog", "cmd/odac-proxy", "cmd/odac-dns", "cmd/odac-mail", "internal/backup", "internal/backup/proto", "internal/backup/thin", "internal/backup/runner", "internal/backup/consistency", "internal/backup/settings", "internal/backup/service"} {
		cmd := exec.Command(goBin, "list", "-deps", "odac/"+bin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", bin, err, stderr.String())
		}
		pkgs := strings.Fields(string(out))
		if len(pkgs) < 2 {
			t.Fatalf("go list %s returned %d packages; the check would pass vacuously", bin, len(pkgs))
		}
		for _, pkg := range pkgs {
			for _, prefix := range engineOnlyModules {
				if strings.HasPrefix(pkg, prefix) {
					t.Errorf("%s depends on %s; the backup engine belongs to odac-backup only", bin, pkg)
				}
			}
		}
	}
}

func TestRun(t *testing.T) {
	var out, errOut bytes.Buffer
	ctx := context.Background()
	if code := run(ctx, []string{"version"}, nil, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) == "" {
		t.Fatalf("version: code %d, out %q", code, out.String())
	}
	out.Reset()
	if code := run(ctx, []string{"bogus"}, nil, &out, &errOut); code == 0 {
		t.Fatal("unknown command exited 0")
	}
}
