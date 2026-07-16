package dataplane

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// JS-semantics helpers: config values arrive as decoded JSON (map[string]any,
// []any, string, float64, bool, nil) and the payload builders must reproduce
// Node's truthiness and coercion rules on them.

// truthy mirrors JavaScript truthiness: false, null/undefined, "", 0 and NaN
// are falsy; objects and arrays (even empty) are truthy.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	}
	return true
}

// jsParseInt mirrors parseInt(v): numbers truncate toward zero, strings parse
// an optional sign plus leading digits ("80/tcp" → 80), anything else is NaN,
// returned here as 0 — callers treat 0 as "no port" exactly like Node's
// !port check.
func jsParseInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		s := strings.TrimSpace(x)
		i := 0
		neg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			neg = s[i] == '-'
			i++
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return 0
		}
		n, err := strconv.Atoi(s[i:j])
		if err != nil {
			return 0
		}
		if neg {
			return -n
		}
		return n
	}
	return 0
}

// jsEqual mirrors === on decoded-JSON scalars, including the accidental
// Node semantics where two absent keys compare equal (undefined ===
// undefined). Non-scalars never compare equal (and would panic under Go ==).
func jsEqual(a, b any) bool {
	switch a.(type) {
	case nil, string, float64, bool:
	default:
		return false
	}
	switch b.(type) {
	case nil, string, float64, bool:
	default:
		return false
	}
	return a == b
}

// str returns the string form of a decoded-JSON scalar ("" for nil).
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
}

// jsString mirrors `${v}` / String(v) on decoded-JSON values: numbers render
// without a trailing ".0", arrays join with "," (null elements as "", like
// Array.prototype.join), objects become "[object Object]". Decoded JSON null
// arrives as Go nil and renders "null".
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				continue // join renders null/undefined as ""
			}
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return fmt.Sprintf("%v", v)
}

// orMap is JS `v || {}`.
func orMap(v any) any {
	if truthy(v) {
		return v
	}
	return map[string]any{}
}

// orList is JS `v || []`.
func orList(v any) any {
	if truthy(v) {
		return v
	}
	return []any{}
}
