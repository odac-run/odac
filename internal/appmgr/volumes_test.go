package appmgr

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"odac/internal/docker"
)

// materializes a single-file volume as a file, not a directory: the whole
// point of file-typed mounts (e.g. a persisted sessions.db).
func TestFixVolumePermissionsCreatesFile(t *testing.T) {
	fx := newFixture(t, []any{})
	host := filepath.Join(fx.m.appsPath(), "app1", "storage", "sessions.db")

	fx.m.fixVolumePermissions("app1", []docker.Mount{
		{Host: host, Container: "/app/storage/sessions.db"},
	})

	fi, err := os.Stat(host)
	if err != nil {
		t.Fatalf("expected file created at %s: %v", host, err)
	}
	if fi.IsDir() {
		t.Fatalf("expected a file, got a directory at %s", host)
	}
}

// a container path without an extension and no other signal is treated as a
// directory (backward-compatible default for named volumes like 'data').
func TestFixVolumePermissionsCreatesDir(t *testing.T) {
	fx := newFixture(t, []any{})
	host := filepath.Join(fx.m.appsPath(), "app1", "data")

	fx.m.fixVolumePermissions("app1", []docker.Mount{
		{Host: host, Container: "/app/data"},
	})

	fi, err := os.Stat(host)
	if err != nil {
		t.Fatalf("expected dir created at %s: %v", host, err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected a directory, got a file at %s", host)
	}
}

// a trailing slash on the container path forces a directory even when the
// basename carries an extension.
func TestFixVolumePermissionsTrailingSlashForcesDir(t *testing.T) {
	fx := newFixture(t, []any{})
	host := filepath.Join(fx.m.appsPath(), "app1", "conf.d")

	fx.m.fixVolumePermissions("app1", []docker.Mount{
		{Host: host, Container: "/app/conf.d/"},
	})

	fi, err := os.Stat(host)
	if err != nil {
		t.Fatalf("stat %s: %v", host, err)
	}
	if !fi.IsDir() {
		t.Fatalf("trailing slash should force a directory at %s", host)
	}
}

// mountIsFile classification order: an existing host path wins over the
// extension heuristic, and the live container wins over the heuristic when the
// host does not yet exist.
func TestMountIsFileClassification(t *testing.T) {
	fx := newFixture(t, []any{})
	base := filepath.Join(fx.m.appsPath(), "app1")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	// (2) existing host directory beats an extension-suggesting basename.
	existingDir := filepath.Join(base, "cache.d")
	if err := os.MkdirAll(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if fx.m.mountIsFile("app1", existingDir, "/app/cache.d") {
		t.Fatalf("existing directory should classify as directory")
	}

	// (3) live container reports a file at an extension-less path.
	fx.dock.statDirs = map[string]bool{"app1\x00/app/socketfile": false}
	if !fx.m.mountIsFile("app1", filepath.Join(base, "socketfile"), "/app/socketfile") {
		t.Fatalf("live container file should classify as file")
	}

	// (3) live container reports a directory at an extension-carrying path.
	fx.dock.statDirs["app1\x00/app/weird.db"] = true
	if fx.m.mountIsFile("app1", filepath.Join(base, "weird.db"), "/app/weird.db") {
		t.Fatalf("live container directory should classify as directory")
	}

	// (4) fallback extension heuristic when nothing else is known.
	if !fx.m.mountIsFile("app1", filepath.Join(base, "new.sqlite"), "/app/new.sqlite") {
		t.Fatalf("unknown path with extension should classify as file")
	}
	if fx.m.mountIsFile("app1", filepath.Join(base, "newdir"), "/app/newdir") {
		t.Fatalf("unknown path without extension should classify as directory")
	}

	// (4) a dotted basename is a hidden directory, not an extension: mounting
	// a file at /root/.ollama makes the ollama server fail to start.
	for _, cPath := range []string{"/root/.ollama", "/root/.ssh", "/home/node/.n8n"} {
		if fx.m.mountIsFile("app1", filepath.Join(base, path.Base(cPath)), cPath) {
			t.Fatalf("dotted basename %s should classify as directory", cPath)
		}
	}

	// ... unless it carries a real extension after the leading dot.
	if !fx.m.mountIsFile("app1", filepath.Join(base, ".env.local"), "/app/.env.local") {
		t.Fatalf("dotted basename with an extension should classify as file")
	}
}
