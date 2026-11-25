package consistency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Spec is a recipe's declaration of how its app's data is backed up, the
// `backup` member of a recipe, template sub-app or app record:
//
//	"backup": {"engine": "postgres"}
//	"backup": {"hook": {"dump": ["/usr/local/bin/export", "--stdout"],
//	                    "restore": ["/usr/local/bin/import"],
//	                    "file": "export.bin", "consistency": "consistent",
//	                    "covers": ["/var/lib/app"]}}
//
// The engine names one ODAC knows when the image name does not show it. A
// hook is for an engine ODAC does not know: its dump command writes the
// data to stdout and its restore command reads it back from stdin. A nil
// Spec is no declaration, which is almost every app.
type Spec struct {
	Engine Engine
	Hook   *Hook
}

// Hook is a recipe's own dump. Dump runs in the app's image, in its network
// namespace and with the app's environment, as the image's entrypoint
// would: the app's own processes see that environment, so the hook sees
// nothing they do not.
type Hook struct {
	Dump    []string
	Restore []string
	// File is the name the stream is stored under.
	File string
	// Consistency is what the recipe vouches for; without it the backup
	// says live, because ODAC cannot tell what the command reads.
	Consistency string
	// Covers are the container paths the dump stands in for, left out of
	// the file backup.
	Covers []string
}

const (
	maxHookArgs = 64
	maxHookArg  = 4096
	maxCovers   = 16
)

var filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Parse validates the `backup` member of an app definition. A definition
// without one yields (nil, nil); anything present but malformed is an
// error, since a declaration silently dropped is an app whose database is
// backed up as torn files.
func Parse(def any) (*Spec, error) {
	raw, ok := def.(map[string]any)
	if !ok || raw == nil {
		return nil, nil
	}
	v, ok := raw["backup"]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid backup: expected an object")
	}
	s := &Spec{}
	for k := range m {
		if k != "engine" && k != "hook" {
			return nil, fmt.Errorf("invalid backup: unknown member %q", k)
		}
	}
	if e, ok := m["engine"]; ok && e != nil {
		name, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("invalid backup.engine: expected a string")
		}
		switch Engine(name) {
		case Postgres, MySQL, MariaDB, Redis, Mongo, ClickHouse, SQLite, None:
			s.Engine = Engine(name)
		default:
			return nil, fmt.Errorf("invalid backup.engine %q", name)
		}
	}
	if h, ok := m["hook"]; ok && h != nil {
		hook, err := parseHook(h)
		if err != nil {
			return nil, err
		}
		s.Hook = hook
	}
	if s.Engine == "" && s.Hook == nil {
		return nil, nil
	}
	if s.Hook != nil && s.Engine != "" {
		return nil, fmt.Errorf("invalid backup: a hook replaces the engine, declare one or the other")
	}
	return s, nil
}

func parseHook(v any) (*Hook, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid backup.hook: expected an object")
	}
	h := &Hook{File: "dump", Consistency: Live}
	for k, val := range m {
		var err error
		switch k {
		case "dump":
			h.Dump, err = argv("dump", val)
		case "restore":
			h.Restore, err = argv("restore", val)
		case "file":
			f, _ := val.(string)
			if !filePattern.MatchString(f) {
				err = fmt.Errorf("invalid backup.hook.file %v", val)
			}
			h.File = f
		case "consistency":
			c, _ := val.(string)
			if c != Consistent && c != CrashConsistent && c != Live {
				err = fmt.Errorf("invalid backup.hook.consistency %v", val)
			}
			h.Consistency = c
		case "covers":
			h.Covers, err = covers(val)
		default:
			err = fmt.Errorf("invalid backup.hook: unknown member %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(h.Dump) == 0 {
		return nil, fmt.Errorf("invalid backup.hook: a dump command is required")
	}
	return h, nil
}

func argv(what string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 || len(list) > maxHookArgs {
		return nil, fmt.Errorf("invalid backup.hook.%s: expected 1 to %d arguments", what, maxHookArgs)
	}
	out := make([]string, len(list))
	for i, a := range list {
		s, ok := a.(string)
		if !ok || len(s) > maxHookArg || strings.ContainsRune(s, 0) || (i == 0 && strings.TrimSpace(s) == "") {
			return nil, fmt.Errorf("invalid backup.hook.%s argument %d", what, i)
		}
		out[i] = s
	}
	return out, nil
}

func covers(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok || len(list) > maxCovers {
		return nil, fmt.Errorf("invalid backup.hook.covers: expected up to %d paths", maxCovers)
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		s, ok := a.(string)
		if !ok || !path.IsAbs(s) || path.Clean(s) != s || s == "/" {
			return nil, fmt.Errorf("invalid backup.hook.covers path %v", a)
		}
		out = append(out, s)
	}
	return out, nil
}

// Map is the declaration as the app record stores it.
func (s *Spec) Map() map[string]any {
	out := map[string]any{}
	if s.Engine != "" {
		out["engine"] = string(s.Engine)
	}
	if h := s.Hook; h != nil {
		hm := map[string]any{"dump": anySlice(h.Dump), "file": h.File, "consistency": h.Consistency}
		if len(h.Restore) > 0 {
			hm["restore"] = anySlice(h.Restore)
		}
		if len(h.Covers) > 0 {
			hm["covers"] = anySlice(h.Covers)
		}
		out["hook"] = hm
	}
	return out
}

// Apply stamps the declaration onto an app record and clears the member
// when there is none. Absent means no declaration: the Cloud compares
// app.list rows by serialization, so an app that declared nothing must
// not grow an empty member.
func (s *Spec) Apply(app map[string]any) {
	if s == nil || (s.Engine == "" && s.Hook == nil) {
		delete(app, "backup")
		return
	}
	app["backup"] = s.Map()
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}
