package appmgr

import (
	"errors"
	"io"
	"io/fs"
	"os"
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
// on the next start. And the app directory must be new: one an earlier app of
// that name left behind (a delete that kept its data, a failed create) holds
// that app's data and may carry symlinks, and the directory is named after
// the app, so any caller can pick the name.
func appBindRefusal(caller api.Caller, appDir string, volumes []map[string]any, configs any) string {
	if caller.Root {
		return ""
	}
	if leftBehind(appDir) {
		return __("The directory of an earlier app named %s is still on disk. An app may only create an app under a name that left nothing behind.", filepath.Base(appDir))
	}

	sources := make([]string, 0, len(volumes)+1)
	for _, vol := range volumes {
		host, _ := vol["host"].(string)
		if !filepath.IsLocal(host) {
			return __("Volume %s is outside the app's directory. An app may only create named volumes.", jsString(host))
		}
		sources = append(sources, filepath.Clean(host))
	}

	if list, _ := configs.([]any); len(list) > 0 {
		sources = append(sources, "configs")
	}

	for _, inner := range sources {
		for _, outer := range sources {
			if inner != outer && nestedIn(outer, inner) {
				return __("Volumes %s and %s overlap. An app may only create separate named volumes.", outer, inner)
			}
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

// leftBehind reports whether dir already exists as anything but an empty
// directory. One that cannot be read counts as left behind.
func leftBehind(dir string) bool {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil || !info.IsDir() {
		return true
	}
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return !errors.Is(err, io.EOF)
}
