package settings

import (
	"path"
	"strings"
	"unicode"
)

// A disk target is a host directory that a helper mounts writable, so
// naming one is close to host root: the action is the operator's alone,
// and directories whose contents the host or ODAC depends on are refused.
// The check is lexical, because ODAC's container cannot see the host's
// file system; a symlink on the host that leads into one of these can
// only have been made by someone who is root there already.

// systemTrees are refused with everything inside them.
var systemTrees = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/proc", "/run", "/sbin", "/sys", "/usr", "/var/run", "/var/lock",
	"/var/lib/docker", "/var/lib/containerd",
}

// volatileTrees are emptied by a reboot.
var volatileTrees = []string{"/tmp", "/var/tmp", "/dev/shm"}

// systemRoots are refused themselves; a directory below them is a normal
// place for backups.
var systemRoots = []string{"/", "/home", "/mnt", "/media", "/opt", "/root", "/srv", "/var", "/var/lib"}

// ProtectedPaths returns the host paths of ODAC's own data that no target
// may be at or inside: the base directory as this process sees it, and the
// host directory its container's /app comes from.
func ProtectedPaths(baseDir, hostRoot string) []string {
	var out []string
	for _, p := range []string{baseDir, hostRoot} {
		if path.IsAbs(p) {
			out = append(out, path.Clean(p))
		}
	}
	return out
}

func within(p, tree string) bool {
	return p == tree || tree == "/" || strings.HasPrefix(p, tree+"/")
}

// checkHostPath returns p clean, or why it cannot be a target.
func checkHostPath(p string, protected []string) (string, error) {
	if !path.IsAbs(p) {
		return "", invalid("A disk target is an absolute path on the host.")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "", invalid("A disk target path cannot contain control characters.")
		}
	}
	p = path.Clean(p)
	for _, root := range systemRoots {
		if p == root {
			return "", invalid("%s is not a backup target; use a directory inside it.", p)
		}
	}
	for _, tree := range systemTrees {
		if within(p, tree) {
			return "", invalid("%s belongs to the operating system and cannot be a backup target.", p)
		}
	}
	for _, tree := range volatileTrees {
		if within(p, tree) {
			return "", invalid("%s is emptied when the server restarts and cannot be a backup target.", p)
		}
	}
	for _, own := range protected {
		if within(p, own) {
			return "", invalid("%s holds ODAC's own data and cannot be a backup target.", p)
		}
	}
	return p, nil
}
