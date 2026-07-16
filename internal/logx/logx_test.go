package logx

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func capture(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := Stdout, Stderr
	Stdout, Stderr = out, errBuf
	t.Cleanup(func() { Stdout, Stderr = oldOut, oldErr })
	return out, errBuf
}

func TestLogLineFormat(t *testing.T) {
	out, errBuf := capture(t)
	l := New("Config")

	l.Log("Loading modular configuration...")
	// Node: console.log('[Config] ', msg) → trailing prefix space + join space.
	if got, want := out.String(), "[Config]  Loading modular configuration...\n"; got != want {
		t.Errorf("Log line = %q, want %q", got, want)
	}
	if errBuf.Len() != 0 {
		t.Errorf("Log wrote to stderr: %q", errBuf.String())
	}
}

func TestMultiModulePrefix(t *testing.T) {
	out, _ := capture(t)
	New("Hub", "WS").Log("connected")
	if got, want := out.String(), "[Hub][WS]  connected\n"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

func TestLogZeroArgsWritesNothing(t *testing.T) {
	out, _ := capture(t)
	New("X").Log()
	if out.Len() != 0 {
		t.Errorf("Log() wrote %q, want nothing", out.String())
	}
}

func TestWarnErrorGoToStderr(t *testing.T) {
	out, errBuf := capture(t)
	l := New("Updater")
	l.Warn("careful")
	l.Error("boom")
	if out.Len() != 0 {
		t.Errorf("stdout got %q", out.String())
	}
	if got, want := errBuf.String(), "[Updater]  careful\n[Updater]  boom\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestPercentSSubstitution(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"single", []any{"Handshake failed: %s", "timeout"}, "[U]  Handshake failed: timeout\n"},
		{"multiple", []any{"%s -> %s", "a", "b"}, "[U]  a -> b\n"},
		{"excess markers stripped", []any{"a %s b %s c", "X"}, "[U]  a X b  c\n"},
		{"excess args appended", []any{"v=%s", "1", "extra"}, "[U]  v=1 extra\n"},
		{"non-string first arg untouched", []any{42, "x"}, "[U]  42 x\n"},
		{"number substitution", []any{"pid %s", 123}, "[U]  pid 123\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := capture(t)
			New("U").Log(tc.args...)
			if got := out.String(); got != tc.want {
				t.Errorf("Log(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestErrorHasNoPercentSSubstitution(t *testing.T) {
	_, errBuf := capture(t)
	New("U").Error("fail: %s", "reason")
	if got, want := errBuf.String(), "[U]  fail: %s reason\n"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
}

func TestRenderSpecials(t *testing.T) {
	out, errBuf := capture(t)
	New("X").Log("val:", nil)
	New("X").Error(errors.New("disk full"))
	if got, want := out.String(), "[X]  val: null\n"; got != want {
		t.Errorf("nil render = %q, want %q", got, want)
	}
	if got, want := errBuf.String(), "[X]  disk full\n"; got != want {
		t.Errorf("error render = %q, want %q", got, want)
	}
}

func TestSanitize(t *testing.T) {
	in := map[string]any{
		"name":     "app1",
		"token":    "tok-123",
		"Password": "hunter2",
		"apiKey":   "k",
		"AUTHcode": "c",
		"env":      map[string]any{"SECRET_URL": "x"},
		"nested": map[string]any{
			"secret": "s",
			"plain":  "ok",
		},
		"list": []any{map[string]any{"token": "t2"}, "str"},
		"port": 8080,
	}
	got := Sanitize(in).(map[string]any)

	want := map[string]any{
		"name":     "app1",
		"token":    "***",
		"Password": "***",
		"apiKey":   "***",
		"AUTHcode": "***",
		"env":      "{ ...redacted... }",
		"nested": map[string]any{
			"secret": "***",
			"plain":  "ok",
		},
		"list": []any{map[string]any{"token": "***"}, "str"},
		"port": 8080,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sanitize = %#v, want %#v", got, want)
	}

	// Original must not be mutated (Node shallow-copies).
	if in["token"] != "tok-123" || in["nested"].(map[string]any)["secret"] != "s" {
		t.Error("Sanitize mutated its input")
	}
}

func TestSanitizePassesScalars(t *testing.T) {
	for _, v := range []any{"a-token-string", 5, true, nil} {
		if got := Sanitize(v); !reflect.DeepEqual(got, v) {
			t.Errorf("Sanitize(%v) = %v, want unchanged", v, got)
		}
	}
}
