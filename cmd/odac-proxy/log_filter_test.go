package main

import (
	"bytes"
	"testing"
)

func TestLogFilterDropsOnlyIgnoredLines(t *testing.T) {
	var out bytes.Buffer
	f := NewLogFilter(&out, []string{"TLS handshake error"})

	for _, line := range []string{
		"2026/11/11 http: TLS handshake error from 203.0.113.9:4431: EOF\n",
		"2026/11/11 http: panic serving 203.0.113.9: boom\n",
	} {
		if n, err := f.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = %d, %v", line, n, err)
		}
	}
	if got := out.String(); got != "2026/11/11 http: panic serving 203.0.113.9: boom\n" {
		t.Fatalf("passed through %q", got)
	}
}
