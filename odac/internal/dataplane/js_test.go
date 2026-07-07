package dataplane

import "testing"

func TestTruthy(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{nil, false},
		{false, false},
		{true, true},
		{"", false},
		{"x", true},
		{float64(0), false},
		{float64(3000), true},
		{map[string]any{}, true}, // JS: empty object is truthy
		{[]any{}, true},          // JS: empty array is truthy
	}
	for _, c := range cases {
		if got := truthy(c.v); got != c.want {
			t.Errorf("truthy(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestJSParseInt(t *testing.T) {
	cases := []struct {
		v    any
		want int
	}{
		{float64(3000), 3000},
		{float64(3000.7), 3000}, // parseInt truncates
		{"3000", 3000},
		{" 8080 ", 8080},
		{"80/tcp", 80}, // leading digits win
		{"-5x", -5},
		{"abc", 0}, // NaN → 0 → "no port"
		{"", 0},
		{nil, 0},
		{true, 0},
	}
	for _, c := range cases {
		if got := jsParseInt(c.v); got != c.want {
			t.Errorf("jsParseInt(%#v) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestJSEqual(t *testing.T) {
	cases := []struct {
		a, b any
		want bool
	}{
		{"app", "app", true},
		{"app", "other", false},
		{float64(1), float64(1), true},
		{nil, nil, true}, // undefined === undefined (Node's accidental match)
		{nil, "x", false},
		{map[string]any{}, map[string]any{}, false}, // non-scalars never equal
	}
	for _, c := range cases {
		if got := jsEqual(c.a, c.b); got != c.want {
			t.Errorf("jsEqual(%#v, %#v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
