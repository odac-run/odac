package imap

import (
	"database/sql"
	"io"
	"net"
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
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			got := make(chan string, 1)
			go func() {
				buf := make([]byte, 4096)
				n, _ := client.Read(buf)
				got <- string(buf[:n])
				io.Copy(io.Discard, client)
			}()

			c := &Connection{conn: server}
			msg := &storage.MessageRow{Text: sql.NullString{String: body, Valid: true}}
			c.writeBodySection(tt.items, msg)
			server.Close()

			out := <-got
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
