package imap

import (
	"strings"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{`"user@example.com" "password123"`, []string{`"user@example.com"`, `"password123"`}},
		{`INBOX`, []string{`INBOX`}},
		{`"Sent Items" "New Folder"`, []string{`"Sent Items"`, `"New Folder"`}},
		{`plain simple`, []string{`plain`, `simple`}},
		// Escaped quote must not close the quoted string / split the token.
		{`"user@example.com" "pa\"ss word"`, []string{`"user@example.com"`, `"pa\"ss word"`}},
		// Escaped backslash before the closing quote stays a single token.
		{`"back\\slash" INBOX`, []string{`"back\\slash"`, `INBOX`}},
	}

	for _, tt := range tests {
		got := splitArgs(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitArgs(%q) = %v (len %d), want %v (len %d)",
				tt.input, got, len(got), tt.want, len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitArgs(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestUnquote(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{`"INBOX"`, `INBOX`},
		{`"Sent Items"`, `Sent Items`},
		{`INBOX`, `INBOX`},
		{`""`, ``},
		{`"a"`, `a`},
		// Escaped characters are decoded per RFC 3501.
		{`"pa\"ss"`, `pa"ss`},
		{`"back\\slash"`, `back\slash`},
	}

	for _, tt := range tests {
		got := unquote(tt.input)
		if got != tt.want {
			t.Errorf("unquote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseJSONFlags(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{`[]`, 0},
		{`["seen"]`, 1},
		{`["seen","deleted"]`, 2},
		{``, 0},
		{`invalid`, 0},
	}

	for _, tt := range tests {
		got := parseJSONFlags(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseJSONFlags(%q) returned %d flags, want %d", tt.input, len(got), tt.want)
		}
	}

	// Verify flag formatting (should have backslash prefix and title case)
	flags := parseJSONFlags(`["seen","deleted"]`)
	if len(flags) == 2 {
		if flags[0] != "\\Seen" {
			t.Errorf("expected \\Seen, got %s", flags[0])
		}
		if flags[1] != "\\Deleted" {
			t.Errorf("expected \\Deleted, got %s", flags[1])
		}
	}
}

func TestExtractConnIP(t *testing.T) {
	// Test IPv4-mapped IPv6 stripping
	ip := "::ffff:192.168.1.1"
	got := extractConnIPString(ip)
	if got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}

	// Test plain IPv4
	got = extractConnIPString("10.0.0.1")
	if got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %s", got)
	}
}

// extractConnIPString is a test helper that applies the same logic as extractConnIP
// but works with raw strings instead of net.Conn.
func extractConnIPString(ip string) string {
	if len(ip) > 7 && ip[:7] == "::ffff:" {
		return ip[7:]
	}
	return ip
}

func TestUnquoteRejectsUnterminatedString(t *testing.T) {
	// The trailing quote is escaped, so the token is an unterminated quoted
	// string and must survive untouched instead of decoding to "a".
	for _, in := range []string{`"a\"`, `"a"b"`, `"`, `"\\\"`} {
		if got := unquote(in); got != in {
			t.Errorf("unquote(%q) = %q, want it returned unchanged", in, got)
		}
	}
}

func TestSplitArgsPreservesBoundaries(t *testing.T) {
	// Reference tokenizer: decodes escapes in one pass, so it is independent of
	// the splitArgs/unquote split. Boundaries must agree for every input,
	// well-formed or not.
	ref := func(s string) []string {
		var parts []string
		var cur strings.Builder
		has, inQuote := false, false
		r := []rune(s)
		for i := 0; i < len(r); i++ {
			switch {
			case inQuote && r[i] == '\\' && i+1 < len(r):
				cur.WriteRune(r[i+1])
				has = true
				i++
			case r[i] == '"':
				inQuote = !inQuote
				has = true
			case r[i] == ' ' && !inQuote:
				if has {
					parts = append(parts, cur.String())
					cur.Reset()
					has = false
				}
			default:
				cur.WriteRune(r[i])
				has = true
			}
		}
		if has {
			parts = append(parts, cur.String())
		}
		return parts
	}

	alphabet := []rune{'"', '\\', ' ', 'a'}
	var walk func(prefix []rune)
	walk = func(prefix []rune) {
		s := string(prefix)
		if got, want := len(splitArgs(s)), len(ref(s)); got != want {
			t.Errorf("splitArgs(%q) produced %d tokens, want %d", s, got, want)
		}
		if len(prefix) == 6 {
			return
		}
		for _, ch := range alphabet {
			walk(append(prefix, ch))
		}
	}
	walk(nil)
}

func TestQuoteString(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{`INBOX`, `"INBOX"`},
		{`Sent Items`, `"Sent Items"`},
		// A bare quote would otherwise close the response token early.
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		// Control characters are illegal inside a quoted-string.
		{"a\r\nb", `"ab"`},
	}
	for _, tt := range tests {
		if got := quoteString(tt.input); got != tt.want {
			t.Errorf("quoteString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}

	// Round-trip: whatever we emit must decode back to the original name.
	for _, name := range []string{`INBOX`, `a"b`, `a\b`, `a\"b`, `""`, `Sent Items`} {
		if got := unquote(quoteString(name)); got != name {
			t.Errorf("unquote(quoteString(%q)) = %q, want %q", name, got, name)
		}
	}
}

func TestValidMailboxName(t *testing.T) {
	valid := []string{"INBOX", "Sent Items", "Ärchiv", `a"b`, strings.Repeat("x", maxMailboxNameLen)}
	for _, name := range valid {
		if !validMailboxName(name) {
			t.Errorf("validMailboxName(%q) = false, want true", name)
		}
	}

	invalid := []string{"", "a\rb", "a\nb", "a\x00b", "a\x7fb", strings.Repeat("x", maxMailboxNameLen+1), "\xff\xfe"}
	for _, name := range invalid {
		if validMailboxName(name) {
			t.Errorf("validMailboxName(%q) = true, want false", name)
		}
	}
}
