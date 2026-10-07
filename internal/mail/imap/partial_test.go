package imap

import (
	"database/sql"
	"strings"
	"testing"

	"odac/internal/mail/storage"
)

// TestWriteBodySectionPartialRange covers the BODY[...]<origin.count> parser.
// Negative or malformed ranges used to reach the slice expression directly and
// panic the connection goroutine.
func TestWriteBodySectionPartialRange(t *testing.T) {
	body := "0123456789"

	tests := []struct {
		name  string
		items string
		want  string
	}{
		{"full section", "BODY[1]", body},
		{"valid range", "BODY[1]<2.3>", "234"},
		{"count past end", "BODY[1]<8.99>", "89"},
		{"origin past end", "BODY[1]<50.5>", ""},
		{"negative origin", "BODY[1]<-5.3>", body},
		{"negative count", "BODY[1]<0.-1>", body},
		{"non-numeric", "BODY[1]<a.b>", body},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &storage.MessageRow{Text: sql.NullString{String: body, Valid: true}}
			out := capture(t, nil, func(c *Connection) {
				c.writeBodySection(tt.items, msg, newRawView(c, msg))
			})

			// Response is "BODY[...]{n}\r\n<payload> "; the payload follows the literal.
			idx := strings.Index(out, "}\r\n")
			if idx < 0 {
				t.Fatalf("malformed response: %q", out)
			}
			if payload := strings.TrimSuffix(out[idx+3:], " "); payload != tt.want {
				t.Errorf("payload = %q, want %q", payload, tt.want)
			}
		})
	}
}
