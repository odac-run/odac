// Package lang ports core/Lang.js: user-facing strings are looked up in a
// per-locale JSON catalog keyed by the English source string, with printf-like
// %s / %s1..%sN argument substitution.
//
// Two deliberate deviations from Node (recorded in the migration STATE):
//
//   - Catalogs are embedded at build time and the lookup is READ-ONLY. Node
//     self-populates the repo's locale/*.json at runtime (missing file →
//     created as {}, missing key → appended with the English string). A
//     compiled binary has no writable __dirname, and mutating shipped files
//     from a CLI run is a side effect we don't want; a missing key simply
//     falls back to the English source string, which is what Node renders for
//     a fresh key anyway.
//
//   - The locale comes from LC_ALL → LC_MESSAGES → LANG (first non-empty),
//     normalized to ll-CC. Node asks ICU via Intl.DateTimeFormat, which on
//     POSIX resolves from the same variables. C/POSIX or nothing set (the
//     usual Windows case) means en-US.
//
// The files under locale/ are copies of the repo-root locale/ directory (the
// source of truth until the Phase 4 repo restructure); TestCatalogsMatchRepo
// fails when they drift.
package lang

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
)

//go:embed locale/*.json
var catalogFS embed.FS

var (
	mu      sync.Mutex
	catalog map[string]string // nil until first T/SetLocale; never nil after
)

// T ports Lang.get: translate key, then substitute arguments — each argument
// replaces its positional %sN marker when the text has one, otherwise the
// first remaining plain %s (both single replacement, like Node's
// String.replace with a string pattern).
func T(key string, args ...any) string {
	mu.Lock()
	if catalog == nil {
		catalog = load(Detect())
	}
	c := catalog
	mu.Unlock()

	if key == "Odac" {
		return "Odac"
	}
	text, ok := c[key]
	if !ok {
		text = key
	}
	for i, arg := range args {
		val := fmt.Sprint(arg)
		if marker := "%s" + strconv.Itoa(i+1); strings.Contains(text, marker) {
			text = strings.Replace(text, marker, val, 1)
		} else {
			text = strings.Replace(text, "%s", val, 1)
		}
	}
	return text
}

// SetLocale pins the catalog to a specific tag (tests; overrides detection).
func SetLocale(tag string) {
	mu.Lock()
	catalog = load(tag)
	mu.Unlock()
}

// Detect resolves the locale tag from LC_ALL, LC_MESSAGES, LANG (first
// non-empty wins, matching POSIX/ICU precedence). "tr_TR.UTF-8" → "tr-TR";
// a bare language ("tr") matches the first embedded catalog for it.
func Detect() string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(name); v != "" {
			return normalize(v)
		}
	}
	return "en-US"
}

func normalize(v string) string {
	if i := strings.IndexAny(v, ".@"); i >= 0 {
		v = v[:i]
	}
	v = strings.ReplaceAll(v, "_", "-")
	if v == "" || v == "C" || v == "POSIX" {
		return "en-US"
	}
	language, region, hasRegion := strings.Cut(v, "-")
	language = strings.ToLower(language)
	if hasRegion {
		return language + "-" + strings.ToUpper(region)
	}
	// Bare language: pick the first embedded catalog with that language.
	entries, _ := fs.Glob(catalogFS, "locale/"+language+"-*.json")
	if len(entries) > 0 {
		return strings.TrimSuffix(strings.TrimPrefix(entries[0], "locale/"), ".json")
	}
	return language
}

// load reads the embedded catalog for tag; an unknown tag or a broken file
// yields an empty catalog, so every key passes through as English.
func load(tag string) map[string]string {
	m := map[string]string{}
	raw, err := catalogFS.ReadFile("locale/" + tag + ".json")
	if err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}
