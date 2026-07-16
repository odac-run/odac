package domains

import (
	"encoding/json"
	"fmt"
	"sort"
)

// JS-semantics helpers over decoded JSON values (the package-local subset of
// the pattern shared by dataplane/js.go and appmgr/helpers.go).

// truthy mirrors JS truthiness for the decoded-JSON value set.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	}
	return true
}

// str mirrors String(v) for the scalar values this package renders into
// messages (ids, appIds).
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	}
	return fmt.Sprintf("%v", v)
}

// listContains ports arr.includes(s) on a decoded JSON array.
func listContains(list any, s string) bool {
	arr, _ := list.([]any)
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

// hasKey reports key presence (JS `in`; distinguishes nil values).
func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// copyShallow snapshots a record map so reads escape the config lock.
func copyShallow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// rawJSON marks pre-serialized payload bytes (api encodes json.RawMessage
// verbatim, preserving Node's key order).
func rawJSON(b []byte) json.RawMessage { return json.RawMessage(b) }

// appendOrderedObject emits {"k":v,...} in the given field order, skipping
// fields whose include flag is false (JS undefined-key omission). Fields
// without an include entry are always emitted.
func appendOrderedObject(out []byte, fields [][2]any, include map[string]bool) []byte {
	out = append(out, '{')
	n := 0
	for _, f := range fields {
		name, _ := f[0].(string)
		if inc, ok := include[name]; ok && !inc {
			continue
		}
		if n > 0 {
			out = append(out, ',')
		}
		n++
		key, _ := json.Marshal(name)
		val, err := json.Marshal(f[1])
		if err != nil {
			val = []byte("null")
		}
		out = append(append(append(out, key...), ':'), val...)
	}
	return append(out, '}')
}
