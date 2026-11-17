package docker

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of app.list's imageMeta. They are a wire contract with the Cloud:
// a member past one is skipped, never truncated, because a cut version
// string is a different version. Lengths count bytes, which also bounds the
// value's length in characters and in UTF-16 units.
const (
	maxVersionMetaKeys  = 16
	maxVersionMetaKey   = 128
	maxVersionMetaValue = 128
)

// versionEnvRe is the only image env name imageMeta reports. The image's
// own Config.Env is read, never the container's, which also carries the
// operator's env and secrets.
var versionEnvRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}_VERSION$`)

// versionMetaLabels are the labels reported whatever their name: they tell
// the Cloud which image a generic version label describes (an Ubuntu base's
// org.opencontainers.image.version is inherited by images built on it).
var versionMetaLabels = map[string]bool{
	"org.opencontainers.image.title":    true,
	"org.opencontainers.image.ref.name": true,
}

// ImageVersionMeta is what an image declares about its own version, in its
// labels and in its config env. It exists for display only: a tag such as
// latest names no version and a digest the Cloud cannot match says nothing,
// so the Cloud picks a value out of this, while every decision (direction,
// auto-update) stays on the digest. Labels and Env are never nil once read,
// so an image that declares nothing is reported as empty, not as unknown.
type ImageVersionMeta struct {
	Labels map[string]string
	Env    map[string]string
}

// Row is the imageMeta member of an app.list row. It is a fresh copy, so a
// caller editing the row cannot reach the cache.
func (m ImageVersionMeta) Row() map[string]any {
	return map[string]any{"labels": stringRow(m.Labels), "env": stringRow(m.Env)}
}

func stringRow(src map[string]string) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// declaredVersionMeta filters an image config down to imageMeta: labels
// whose name mentions a version plus versionMetaLabels, env names matching
// versionEnvRe. Each side keeps at most maxVersionMetaKeys, the first by
// sorted key, so the same image always reports the same set.
func declaredVersionMeta(labels map[string]string, env []string) ImageVersionMeta {
	envVals := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if versionEnvRe.MatchString(name) {
			// A later entry wins, as it does in the engine.
			envVals[name] = value
		}
	}
	return ImageVersionMeta{
		Labels: pickVersionMeta(labels, func(k string) bool {
			return versionMetaLabels[k] || strings.Contains(strings.ToLower(k), "version")
		}),
		Env: pickVersionMeta(envVals, func(string) bool { return true }),
	}
}

func pickVersionMeta(src map[string]string, wanted func(string) bool) map[string]string {
	keys := make([]string, 0, len(src))
	for k, v := range src {
		if wanted(k) && reportableMeta(k, maxVersionMetaKey) && reportableMeta(v, maxVersionMetaValue) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) > maxVersionMetaKeys {
		keys = keys[:maxVersionMetaKeys]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = src[k]
	}
	return out
}

// reportableMeta refuses what the Cloud could not store as written: an
// oversized string, invalid UTF-8 (JSON would rewrite it), or any control
// character.
func reportableMeta(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
