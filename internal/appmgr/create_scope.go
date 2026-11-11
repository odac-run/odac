package appmgr

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"odac/internal/api"
)

// appBindRefusal decides whether a container that an app, not the operator,
// creates through app.create keeps its bind mounts inside its own app
// directory, and returns the refusal message when it does not. The operator
// may bind any host path; an app holding app.create would otherwise reach
// host root through the container it creates (`/`, the Docker socket).
//
// Three rules, all needed. A source must be a local relative path (a named
// volume), since an absolute one or one climbing out with ".." is the host.
// Sources may not nest: a container writing through `data` can replace
// `data/x` with a symlink, and Docker and fixVolumePermissions both follow it
// on the next start. And no component that already exists may be a symlink,
// because a directory left behind by an earlier app of that name may carry
// one.
func appBindRefusal(caller api.Caller, appDir string, volumes []map[string]any, configs any) string {
	if caller.Root {
		return ""
	}

	sources := make([]string, 0, len(volumes)+1)
	for _, vol := range volumes {
		host, _ := vol["host"].(string)
		if !filepath.IsLocal(host) {
			return __("Volume %s is outside the app's directory. An app may only create named volumes.", jsString(host))
		}
		sources = append(sources, filepath.Clean(host))
	}

	walk := append([]string(nil), sources...)
	if list, _ := configs.([]any); len(list) > 0 {
		sources = append(sources, "configs")
		for _, raw := range list {
			cfg, _ := raw.(map[string]any)
			target, _ := cfg["path"].(string)
			if target = path.Clean(target); path.IsAbs(target) {
				walk = append(walk, filepath.Join("configs", filepath.FromSlash(target)))
			}
		}
		walk = append(walk, "configs")
	}

	for _, inner := range sources {
		for _, outer := range sources {
			if inner != outer && nestedIn(outer, inner) {
				return __("Volumes %s and %s overlap. An app may only create separate named volumes.", outer, inner)
			}
		}
	}
	for _, rel := range walk {
		if symlinkOnPath(appDir, rel) {
			return __("Volume %s passes through a symbolic link.", rel)
		}
	}
	return ""
}

// nestedIn reports whether the cleaned relative path inner lies strictly
// inside outer.
func nestedIn(outer, inner string) bool {
	if outer == "." {
		return inner != "."
	}
	return strings.HasPrefix(inner, outer+string(filepath.Separator))
}

// symlinkOnPath reports whether dir, or any existing component of rel below
// it, is a symbolic link. Components that do not exist yet are created later
// by ODAC or Docker as plain directories, so the walk stops at the first one.
func symlinkOnPath(dir, rel string) bool {
	cur := dir
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			cur = filepath.Join(cur, parts[i])
		}
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if err != nil {
			return true // unreadable: refuse rather than guess
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
