package netmode

import "testing"

func TestParse(t *testing.T) {
	valid := []struct {
		in   any
		want string
	}{
		{nil, Bridge},
		{"", Bridge},
		{"bridge", Bridge},
		{"default", Bridge},
		{"  HOST  ", Host},
		{"Bridge", Bridge},
		{"host", Host},
	}
	for _, tc := range valid {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%#v) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := []any{"none", "container:foo", "macvlan", 42, true, []any{"host"}}
	for _, in := range invalid {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%#v) = %q, want an error", in, got)
		}
	}
}

func TestIsHost(t *testing.T) {
	if !IsHost("host") {
		t.Error("IsHost(host) = false")
	}
	// Anything unparseable must fall back to the isolated default, never host.
	for _, v := range []any{nil, "", "bridge", "default", "garbage", 7} {
		if IsHost(v) {
			t.Errorf("IsHost(%#v) = true, want false", v)
		}
	}
}
