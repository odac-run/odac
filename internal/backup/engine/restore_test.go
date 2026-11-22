package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/snapshot/restore"
)

func TestRestoreGuardKeepsEntriesInside(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.Mkdir(filepath.Join(dir, "real"), 0o700)
	os.Symlink(outside, filepath.Join(dir, "link"))
	os.WriteFile(filepath.Join(dir, "real", "done"), nil, 0o600)
	g := &guardedOutput{FilesystemOutput: &restore.FilesystemOutput{TargetPath: dir}}
	for rel, leaf := range map[string]bool{"": true, "real": false, "real/new": true, "new": true} {
		if err := g.check(rel, leaf); err != nil {
			t.Errorf("%q: %v", rel, err)
		}
	}
	for _, rel := range []string{"..", "../x", "real/../../x", "/etc/passwd", "link/passwd", "real//x", "real/done", "link", "missing/x"} {
		if err := g.check(rel, true); !errors.Is(err, ErrUnsafeEntry) {
			t.Errorf("%q passed the guard: %v", rel, err)
		}
	}
}
