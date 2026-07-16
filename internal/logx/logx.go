// Package logx is the Go port of core/Log.js for the server binary: module-
// prefixed log lines on stdout (Log) and stderr (Warn/Error), with the same
// %s substitution and sensitive-field redaction as Node.
//
// Line format matches Node byte-for-byte for string arguments: the module
// prefix "[A][B] " keeps its trailing space and arguments are joined with a
// single space (console.log semantics), so a line reads "[Config]  message"
// with two spaces after the bracket — exactly what production logs contain
// today and what the monitor's bracket parser expects. The watchdog adds the
// outer "[LOG][<ISO>]" prefix; this package never writes it.
//
// Deviations from Node (log content is human-facing, not contractual):
//   - Non-string arguments render via fmt ("%v") instead of util.inspect.
//   - Errors render as err.Error() (Go errors carry no stack).
//   - Node's CLI-mode suppression is not ported — the Go CLI has its own
//     output paths and never uses this package.
//   - String.prototype.replace's "$&"-style dollar patterns in %s
//     replacements are not interpreted (Go replaces literally).
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Stdout and Stderr are swappable for tests.
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// Logger writes lines prefixed with "[Module] " (or "[A][B] " for multiple
// module names), mirroring Log.init(...arg) in Node.
type Logger struct {
	prefix string
}

// New returns a logger for the given module name(s):
// New("Updater") → prefix "[Updater] ".
func New(modules ...string) *Logger {
	return &Logger{prefix: "[" + strings.Join(modules, "][") + "] "}
}

// Log writes to stdout. Zero arguments write nothing (Node parity). The
// first argument, when it is a string containing %s, consumes following
// arguments one per %s; leftover %s markers are stripped.
func (l *Logger) Log(args ...any) {
	if len(args) == 0 {
		return
	}
	rendered := renderAll(args)
	if strings.Contains(rendered[0], "%s") {
		msg, rest := rendered[0], rendered[1:]
		for strings.Contains(msg, "%s") && len(rest) > 0 {
			msg = strings.Replace(msg, "%s", rest[0], 1)
			rest = rest[1:]
		}
		msg = strings.ReplaceAll(msg, "%s", "")
		rendered = append([]string{msg}, rest...)
	}
	l.write(Stdout, rendered)
}

// Warn writes to stderr. No %s substitution (Node parity: only log() has it).
func (l *Logger) Warn(args ...any) {
	l.write(Stderr, renderAll(args))
}

// Error writes to stderr. No %s substitution.
func (l *Logger) Error(args ...any) {
	l.write(Stderr, renderAll(args))
}

func (l *Logger) write(w io.Writer, rendered []string) {
	// console.log(module, ...args): single-space join, module keeps its
	// trailing space. Write errors (e.g. EPIPE) are ignored — Node swallows
	// EPIPE on stdout/stderr too.
	fmt.Fprintln(w, strings.Join(append([]string{l.prefix}, rendered...), " "))
}

func renderAll(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = render(Sanitize(a))
	}
	return out
}

func render(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case error:
		return t.Error()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Sanitize is the port of Log.#sanitize: shallow-copies maps, replaces any
// "env" value with "{ ...redacted... }", masks values whose key contains
// token/password/secret/key/auth (case-insensitive) with "***", and recurses
// into nested maps and slices. Non-container values pass through unchanged
// (a bare string is never masked — Node parity).
func Sanitize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		cp := make(map[string]any, len(t))
		for k, val := range t {
			cp[k] = val
		}
		if _, ok := cp["env"]; ok {
			cp["env"] = "{ ...redacted... }"
		}
		for k, val := range cp {
			if k == "env" {
				continue
			}
			if sensitiveKey(k) {
				cp[k] = "***"
			} else {
				switch val.(type) {
				case map[string]any, []any:
					cp[k] = Sanitize(val)
				}
			}
		}
		return cp
	case []any:
		cp := make([]any, len(t))
		for i, item := range t {
			cp[i] = Sanitize(item)
		}
		return cp
	default:
		return v
	}
}

var sensitiveParts = []string{"token", "password", "secret", "key", "auth"}

func sensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range sensitiveParts {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}
