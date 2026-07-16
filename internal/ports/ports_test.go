package ports

import (
	"reflect"
	"testing"
)

// Mirrors test/server/Ports.test.js. Entries are decoded-JSON shapes:
// map[string]any with float64 numbers.

func e(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestIsProxy(t *testing.T) {
	cases := []struct {
		name  string
		entry map[string]any
		want  bool
	}{
		{"explicit sentinel", e("host", "proxy", "container", 3000.0), true},
		{"legacy no host", e("container", 3000.0), true},
		{"empty host", e("host", "", "container", 3000.0), true},
		{"published numeric host", e("host", 8080.0, "container", 3000.0), false},
		{"published numeric string host", e("host", "8080", "container", 3000.0), false},
		{"nil entry", nil, false},
	}
	for _, c := range cases {
		if got := IsProxy(c.entry); got != c.want {
			t.Errorf("IsProxy(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsPublished(t *testing.T) {
	if !IsPublished(e("host", 8080.0, "container", 3000.0)) {
		t.Error("numeric host should be published")
	}
	if IsPublished(e("host", "proxy", "container", 3000.0)) {
		t.Error("proxy sentinel should not be published")
	}
	if IsPublished(e("container", 3000.0)) {
		t.Error("legacy no-host entry should not be published")
	}
	if IsPublished(nil) {
		t.Error("nil entry should not be published")
	}
}

func TestIsPublic(t *testing.T) {
	cases := []struct {
		name  string
		entry map[string]any
		want  bool
	}{
		{"published opting in", e("host", 8080.0, "public", true), true},
		{"published public:false", e("host", 8080.0, "public", false), false},
		{"published no flag", e("host", 8080.0), false},
		{"proxy claiming public", e("host", "proxy", "public", true), false},
		{"legacy claiming public", e("container", 3000.0, "public", true), false},
		{"truthy string not accepted", e("host", 8080.0, "public", "true"), false},
		{"truthy number not accepted", e("host", 8080.0, "public", 1.0), false},
		{"nil entry", nil, false},
	}
	for _, c := range cases {
		if got := IsPublic(c.entry); got != c.want {
			t.Errorf("IsPublic(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBindIP(t *testing.T) {
	if got := BindIP(e("host", 8080.0)); got != "127.0.0.1" {
		t.Errorf("default bind = %q, want loopback", got)
	}
	if got := BindIP(e("host", 8080.0, "public", false)); got != "127.0.0.1" {
		t.Errorf("public:false bind = %q, want loopback", got)
	}
	// Empty is Docker's own spelling of "0.0.0.0 and [::]"; naming the
	// families would break hosts without IPv6.
	if got := BindIP(e("host", 8080.0, "public", true)); got != "" {
		t.Errorf("public bind = %q, want empty", got)
	}
	if got := BindIP(e("host", "proxy", "public", true)); got != "127.0.0.1" {
		t.Errorf("proxy wrongly claiming public bind = %q, want loopback", got)
	}
}

func TestParsePublic(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		want   bool
		wantOK bool
	}{
		{"true through", true, true, true},
		{"false through", false, false, true},
		{"absent (nil)", nil, false, true},
		{"empty string", "", false, true},
		{"string true", "true", true, true},
		{"string false", "false", false, true},
		{"rejects yes", "yes", false, false},
		{"rejects 1", 1.0, false, false},
		{"rejects 0", 0.0, false, false},
		{"rejects object", map[string]any{}, false, false},
	}
	for _, c := range cases {
		got, ok := ParsePublic(c.value)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParsePublic(%s) = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestIsAuto(t *testing.T) {
	if !IsAuto(e("host", "proxy", "auto", true)) {
		t.Error("auto:true should be auto")
	}
	if IsAuto(e("host", "proxy")) {
		t.Error("absent auto should not be auto")
	}
	if IsAuto(e("host", "proxy", "auto", false)) {
		t.Error("auto:false should not be auto")
	}
	if IsAuto(e("host", "proxy", "auto", "true")) {
		t.Error("truthy non-boolean should not be auto")
	}
	if IsAuto(nil) {
		t.Error("nil entry should not be auto")
	}
}

func TestPrimary(t *testing.T) {
	published := e("host", 9000.0, "container", 9000.0)
	proxy := e("host", "proxy", "container", 3000.0)

	if got := Primary([]any{proxy, published}); !same(got, proxy) {
		t.Error("proxy-first: primary should be the proxy entry")
	}
	if got := Primary([]any{published, proxy}); !same(got, proxy) {
		t.Error("proxy-last: primary should still be the proxy entry")
	}

	legacy := e("container", 3000.0)
	if got := Primary([]any{published, legacy}); !same(got, legacy) {
		t.Error("legacy no-host entry should win as the proxy entry")
	}

	first := e("host", 8080.0, "container", 3000.0)
	if got := Primary([]any{first, e("host", 9090.0, "container", 3000.0)}); !same(got, first) {
		t.Error("nothing proxy-routed: primary should be the first entry")
	}

	if Primary([]any{}) != nil {
		t.Error("empty list should have no primary")
	}
	if Primary(nil) != nil {
		t.Error("nil list should have no primary")
	}
}

// same reports whether two entry maps are the identical map value (Node's
// toBe), using pointer identity via interchangeable writes being visible.
func same(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestDiscovered(t *testing.T) {
	got := Discovered(3000)
	want := map[string]any{"host": "proxy", "container": 3000.0, "auto": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discovered(3000) = %v, want %v", got, want)
	}

	entry := Discovered(8080)
	if !IsAuto(entry) || !IsProxy(entry) {
		t.Error("discovered entry should be auto and proxy-routed")
	}
	if got := Primary([]any{e("host", 9000.0, "container", 9000.0), entry}); !same(got, entry) {
		t.Error("discovered entry should be picked as primary")
	}
}

func TestNormalize(t *testing.T) {
	t.Run("stamps sentinel onto legacy entries in place", func(t *testing.T) {
		legacy := e("container", 3000.0)
		list := []any{legacy}
		returned := Normalize(list)
		if len(returned) != 1 || !same(returned[0].(map[string]any), legacy) {
			t.Fatal("Normalize should return the same slice/entries")
		}
		want := map[string]any{"host": "proxy", "container": 3000.0, "auto": true}
		if !reflect.DeepEqual(legacy, want) {
			t.Errorf("normalized entry = %v, want %v", legacy, want)
		}
	})

	t.Run("marks legacy proxy entries as guesses", func(t *testing.T) {
		list := []any{e("container", 3000.0), e("host", "", "container", 4000.0)}
		Normalize(list)
		for i, p := range list {
			if !IsAuto(p.(map[string]any)) {
				t.Errorf("entry %d should be marked auto", i)
			}
		}
	})

	t.Run("does not mark an explicit proxy entry", func(t *testing.T) {
		// Written by setPorts, so it is the user's choice and must survive restarts.
		explicit := e("host", "proxy", "container", 3000.0)
		Normalize([]any{explicit})
		if IsAuto(explicit) {
			t.Error("explicit proxy entry must not gain the auto marker")
		}
		if !reflect.DeepEqual(explicit, e("host", "proxy", "container", 3000.0)) {
			t.Errorf("explicit entry mutated: %v", explicit)
		}
	})

	t.Run("leaves published entries untouched", func(t *testing.T) {
		pub := e("host", 8080.0, "container", 3000.0)
		Normalize([]any{pub})
		if !reflect.DeepEqual(pub, e("host", 8080.0, "container", 3000.0)) {
			t.Errorf("published entry mutated: %v", pub)
		}
	})

	t.Run("normalizes every entry, not just the first", func(t *testing.T) {
		second := e("container", 9000.0)
		Normalize([]any{e("host", 8080.0, "container", 3000.0), second})
		if second["host"] != "proxy" {
			t.Errorf("second entry host = %v, want proxy", second["host"])
		}
	})

	t.Run("tolerates nil list and nil entries", func(t *testing.T) {
		if Normalize(nil) != nil {
			t.Error("nil list should come back nil")
		}
		legacy := e("container", 3000.0)
		Normalize([]any{nil, legacy})
		if legacy["host"] != "proxy" {
			t.Error("entries after a nil entry should still normalize")
		}
	})
}
