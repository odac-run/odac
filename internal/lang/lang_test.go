package lang

import (
	"testing"
)

func setCatalog(t *testing.T, tag string) {
	t.Helper()
	SetLocale(tag)
	t.Cleanup(func() { SetLocale("en-US") })
}

func TestTTranslates(t *testing.T) {
	setCatalog(t, "tr-TR")
	if got := T("Commands:"); got != "Komutlar:" {
		t.Fatalf("T(Commands:) = %q", got)
	}
	if got := T("Online"); got != "Çevrimiçi" {
		t.Fatalf("T(Online) = %q", got)
	}
}

func TestTMissingKeyFallsThrough(t *testing.T) {
	setCatalog(t, "tr-TR")
	if got := T("No such key in any catalog"); got != "No such key in any catalog" {
		t.Fatalf("missing key = %q", got)
	}
}

func TestTOdacHardcoded(t *testing.T) {
	setCatalog(t, "tr-TR")
	if got := T("Odac"); got != "Odac" {
		t.Fatalf("T(Odac) = %q", got)
	}
}

func TestTUnknownLocalePassthrough(t *testing.T) {
	setCatalog(t, "xx-XX")
	if got := T("Commands:"); got != "Commands:" {
		t.Fatalf("unknown locale: %q", got)
	}
}

func TestTSubstitution(t *testing.T) {
	setCatalog(t, "xx-XX") // empty catalog: templates pass through untranslated
	tests := []struct {
		key  string
		args []any
		want string
	}{
		// sequential %s, one per argument, single replacement each
		{"a %s b %s", []any{"1", "2"}, "a 1 b 2"},
		// positional markers consumed by index
		{"x %s2 y %s1", []any{"A", "B"}, "x B y A"},
		// mixed: %s1 taken by the first arg, plain %s by the second
		{"%s1 and %s", []any{"one", "two"}, "one and two"},
		// more args than markers: extras are no-ops (Node: replace misses)
		{"just %s", []any{"a", "b"}, "just a"},
		// non-string args are stringified
		{"n=%s", []any{42}, "n=42"},
	}
	for _, tc := range tests {
		if got := T(tc.key, tc.args...); got != tc.want {
			t.Errorf("T(%q, %v) = %q, want %q", tc.key, tc.args, got, tc.want)
		}
	}
}

func TestTTranslatedTemplateSubstitution(t *testing.T) {
	setCatalog(t, "tr-TR")
	got := T("Login on %s to manage all your server operations.", "https://odac.run")
	want := "Tüm sunucu işlemlerinizi yönetmek için https://odac.run üzerinde giriş yapın."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		lcAll, lcMessages, lang string
		want                    string
	}{
		{"tr_TR.UTF-8", "", "", "tr-TR"},
		{"", "de_DE", "", "de-DE"},
		{"", "", "pt_BR.UTF-8", "pt-BR"},
		{"fr_FR", "de_DE", "en_US", "fr-FR"}, // LC_ALL wins
		{"", "", "", "en-US"},
		{"C", "", "", "en-US"},
		{"C.UTF-8", "", "", "en-US"},
		{"POSIX", "", "", "en-US"},
		{"tr", "", "", "tr-TR"}, // bare language matches a catalog
		{"xx", "", "", "xx"},    // bare language without a catalog
		{"ru_RU.KOI8-R", "", "", "ru-RU"},
		{"zh_CN.UTF-8@pinyin", "", "", "zh-CN"},
	}
	for _, tc := range tests {
		t.Setenv("LC_ALL", tc.lcAll)
		t.Setenv("LC_MESSAGES", tc.lcMessages)
		t.Setenv("LANG", tc.lang)
		if got := Detect(); got != tc.want {
			t.Errorf("Detect(LC_ALL=%q LC_MESSAGES=%q LANG=%q) = %q, want %q",
				tc.lcAll, tc.lcMessages, tc.lang, got, tc.want)
		}
	}
}
