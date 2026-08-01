package main

import (
	"strings"
	"testing"

	"odac/internal/lang"
)

// Tests in this package assert English output; pin the catalog so they don't
// depend on the host machine's LC_ALL/LANG (en-US is identity-mapped).
func init() { lang.SetLocale("en-US") }

// TestHelpTranslated exercises the display-time translation path end to end:
// with the Turkish catalog the help output localizes command descriptions
// ("List all available commands") and invalid-command errors substitute into
// the translated template.
func TestHelpTranslated(t *testing.T) {
	lang.SetLocale("tr-TR")
	defer lang.SetLocale("en-US")

	a, out, _ := testApp(t, "127.0.0.1:0")

	if code := a.dispatch([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(out.String(), "Tüm kullanılabilir komutları listele") {
		t.Fatalf("help not translated:\n%s", out.String())
	}

	out.Reset()
	if code := a.dispatch([]string{"bogus"}); code != 1 {
		t.Fatalf("bogus exit = %d", code)
	}
	if !strings.Contains(out.String(), "geçerli bir komut değil") {
		t.Fatalf("invalid-command error not translated:\n%s", out.String())
	}
}
