package rundir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlIsNotInsideApp(t *testing.T) {
	base := t.TempDir()
	rel, err := filepath.Rel(App(base), Control(base))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("Control %q is inside App %q", Control(base), App(base))
	}
}

func TestPrepareTightensExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %o, want 700", got)
	}
}
