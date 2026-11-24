package appmgr

import (
	"odac/internal/docker"
)

// runtimeOverrideKey is the app record member holding the language version
// an operator pinned for a git app's builds ("22", "3.12"). It is desired
// state rather than a report, so it outlives restarts and the rebuilds a
// caller starts without naming it, and app.list leaves it out: the reported
// runtime's source says whether a build applied it.
const runtimeOverrideKey = "runtimeOverride"

// parseRuntimeOverride reads the runtime member of an app.redeploy or git
// app.create payload. set reports whether the payload decides the override
// at all: an absent member keeps the stored one, a null or empty version
// clears it, and a string version replaces it. Anything that is not one of
// those shapes, or a version no language takes, is refused with
// docker.ErrUnsupportedRuntimeVersion rather than read as "clear".
func parseRuntimeOverride(v any, present bool) (version string, set bool, err error) {
	if !present {
		return "", false, nil
	}
	switch r := v.(type) {
	case nil:
		return "", true, nil
	case map[string]any:
		switch ver := r["version"].(type) {
		case nil:
			return "", true, nil
		case string:
			if ver == "" {
				return "", true, nil
			}
			if docker.ValidRuntimeOverride(ver) {
				return ver, true, nil
			}
		}
	}
	return "", true, docker.ErrUnsupportedRuntimeVersion
}

// storedRuntimeOverride reads the override off a record. ok is false for a
// value no write path produces (a hand-edited number or object), which the
// caller refuses instead of building as if there were none.
func storedRuntimeOverride(app map[string]any) (version string, ok bool) {
	v, present := app[runtimeOverrideKey]
	if !present {
		return "", true
	}
	version, ok = v.(string)
	return version, ok
}

// applyRuntimeOverrideLocked writes a parsed override onto a record. Caller
// holds cfg.Mutate.
func applyRuntimeOverrideLocked(app map[string]any, version string) {
	if version == "" {
		delete(app, runtimeOverrideKey)
		return
	}
	app[runtimeOverrideKey] = version
}
