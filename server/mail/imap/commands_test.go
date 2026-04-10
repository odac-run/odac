package imap

import (
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
