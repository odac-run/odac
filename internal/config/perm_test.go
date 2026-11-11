package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %#o, want %#o", path, got, want)
	}
}

// TestSecretsArePrivateToTheServer pins the modes of everything the store
// writes: api.json carries the root token, hub.json the Hub secret.
func TestSecretsArePrivateToTheServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s.Set("api", map[string]any{"auth": "root-token"})
		if err := s.SaveDirty(); err != nil {
			t.Fatal(err)
		}
	}
	assertMode(t, filepath.Join(dir, "config"), dirMode)
	assertMode(t, filepath.Join(dir, "config", "api.json"), fileMode)
	assertMode(t, filepath.Join(dir, ".bak"), dirMode)
	assertMode(t, filepath.Join(dir, ".bak", "api.json.bak"), fileMode)
}

// TestOpenNarrowsAnExistingInstall covers a data directory written before
// the modes were tightened: its secrets sit at 0644 in 0755 directories.
func TestOpenNarrowsAnExistingInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	bak := filepath.Join(dir, ".bak")
	for _, d := range []string{cfgDir, bak} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	api := filepath.Join(cfgDir, "api.json")
	if err := os.WriteFile(api, []byte(`{"api":{"auth":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, cfgDir, dirMode)
	assertMode(t, bak, dirMode)
	if err := s.ForceSave(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, api, fileMode)
}

// TestRecoveredModuleIsPrivate covers the restore path, which rewrites the
// main file from its backup outside writeModule.
func TestRecoveredModuleIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1", "v2"} {
		s.Set("hub", map[string]any{"secret": v})
		if err := s.SaveDirty(); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(dir, "config", "hub.json")
	if err := os.WriteFile(main, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(main, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, main, fileMode)
	assertMode(t, main+".corrupted", fileMode)
}

// TestFailedWriteStaysDirty pins the retry contract now that the dirty flag
// is cleared before the write: a module whose write fails must be retried.
func TestFailedWriteStaysDirty(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("hub", map[string]any{"secret": "v1"})
	// A non-empty directory where the temp file belongs makes the write
	// fail and survives the cleanup of the failed temp file.
	tmp := filepath.Join(dir, "config", "hub.json.tmp")
	if err := os.MkdirAll(filepath.Join(tmp, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDirty(); err == nil {
		t.Fatal("SaveDirty succeeded with an unwritable temp file")
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDirty(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "hub.json")); err != nil {
		t.Errorf("hub.json not written on retry: %v", err)
	}
}
