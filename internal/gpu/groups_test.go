package gpu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeDev points the device scan at a temp tree; the files there are owned
// by the test process's own group, which is what the assertions expect.
func fakeDev(t *testing.T, names ...string) int {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dri"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o660); err != nil {
			t.Fatal(err)
		}
	}
	previous := devRoot
	devRoot = root
	t.Cleanup(func() { devRoot = previous })

	gid, ok := deviceGID(filepath.Join(root, names[0]))
	if !ok {
		t.Skip("device ownership is not observable on this platform")
	}
	return gid
}

// noGroupFile makes the fallback resolve to nothing.
func noGroupFile(t *testing.T) {
	t.Helper()
	previous := groupFileCandidates
	groupFileCandidates = []string{filepath.Join(t.TempDir(), "absent")}
	t.Cleanup(func() { groupFileCandidates = previous })
}

// The node's own owner is the authoritative answer: it is the gid the kernel
// checks, whatever the group happens to be called.
func TestRenderGroupsFromDeviceNodes(t *testing.T) {
	gid := fakeDev(t, "kfd", "dri/renderD128", "dri/card0")
	noGroupFile(t)

	got := RenderGroups()
	if len(got) != 1 || got[0] != strconv.Itoa(gid) {
		t.Errorf("RenderGroups() = %v, want [%d]", got, gid)
	}
}

// ODAC usually runs containerised, where /dev shows nothing of the host.
func TestRenderGroupsFallsBackToGroupFile(t *testing.T) {
	previous := devRoot
	devRoot = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { devRoot = previous })

	path := filepath.Join(t.TempDir(), "group")
	if err := os.WriteFile(path, []byte(
		"root:x:0:\nvideo:x:44:emre\nrender:x:993:\nplugdev:x:46:\nbroken:x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previousCandidates := groupFileCandidates
	groupFileCandidates = []string{filepath.Join(t.TempDir(), "absent"), path}
	t.Cleanup(func() { groupFileCandidates = previousCandidates })

	got := RenderGroups()
	if len(got) != 2 || got[0] != "44" || got[1] != "993" {
		t.Errorf("RenderGroups() = %v, want [44 993]", got)
	}
}

// Nothing resolvable is not an error: a root container opens the nodes
// anyway, and inventing a gid would be worse than adding none.
func TestRenderGroupsEmpty(t *testing.T) {
	previous := devRoot
	devRoot = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { devRoot = previous })
	noGroupFile(t)

	if got := RenderGroups(); len(got) != 0 {
		t.Errorf("RenderGroups() = %v, want none", got)
	}
}
