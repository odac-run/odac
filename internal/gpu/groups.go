package gpu

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// devRoot is a test seam over the device tree.
var devRoot = "/dev"

// groupFileCandidates mirrors the layering sysinfo uses for os-release: an
// explicit host bind mount, the pid-host path, then this container's own
// file. ODAC usually runs containerised, where /etc/group is the image's and
// says nothing about the host's render group.
var groupFileCandidates = []string{"/host/etc/group", "/proc/1/root/etc/group", "/etc/group"}

// renderGroupNames are the conventional owners of the DRM nodes, consulted
// only when the nodes themselves are not visible from in here.
var renderGroupNames = []string{"render", "video"}

// RenderGroups returns the supplementary group ids a container needs to open
// /dev/kfd and /dev/dri/*, as decimal strings for HostConfig.GroupAdd.
//
// Numeric ids, not names: --group-add resolves a name against the
// container's /etc/group, where the host's render group may be absent or
// carry a different id, while the kernel checks the numeric owner of the
// device inode. Empty when nothing resolves — an image running as root opens
// the nodes regardless, and adding a wrong group would be worse than none.
func RenderGroups() []string {
	seen := map[int]bool{}
	for _, path := range renderNodePaths() {
		if gid, ok := deviceGID(path); ok && gid > 0 {
			seen[gid] = true
		}
	}
	if len(seen) == 0 {
		for _, gid := range groupFileGIDs() {
			seen[gid] = true
		}
	}

	gids := make([]int, 0, len(seen))
	for gid := range seen {
		gids = append(gids, gid)
	}
	sort.Ints(gids)

	out := make([]string, 0, len(gids))
	for _, gid := range gids {
		out = append(out, strconv.Itoa(gid))
	}
	return out
}

// renderNodePaths lists the device nodes whose ownership matters: the ROCm
// compute interface and every DRM node.
func renderNodePaths() []string {
	paths := []string{filepath.Join(devRoot, "kfd")}
	dri := filepath.Join(devRoot, "dri")
	entries, err := os.ReadDir(dri)
	if err != nil {
		return paths
	}
	for _, entry := range entries {
		paths = append(paths, filepath.Join(dri, entry.Name()))
	}
	return paths
}

// groupFileGIDs resolves renderGroupNames against the first readable group
// file, the fallback for hosts whose /dev ODAC's own container cannot see.
func groupFileGIDs() []int {
	for _, candidate := range groupFileCandidates {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var gids []int
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) < 3 {
				continue
			}
			for _, want := range renderGroupNames {
				if fields[0] != want {
					continue
				}
				if gid, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil && gid > 0 {
					gids = append(gids, gid)
				}
			}
		}
		return gids
	}
	return nil
}
