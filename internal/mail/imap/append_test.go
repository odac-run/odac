package imap

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"odac/internal/mail/storage"
)

func TestAppendFlagGroupRejoinsSplitTokens(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"multiple flags", []string{`(\Seen`, `\Draft)`, `{12}`}, []string{`\Seen`, `\Draft`}},
		{"single flag", []string{`(\Draft)`, `{12}`}, []string{`\Draft`}},
		{"empty group", []string{`()`, `{12}`}, nil},
		{"no group", []string{`{12}`}, nil},
		{"unterminated", []string{`(\Seen`, `\Draft`}, []string{`\Seen`, `\Draft`}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendFlagGroup(tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Saving a draft used to write `[\Seen` into the flags column, after which
// every SELECT of that mailbox failed with "malformed JSON" and the client
// reported "SELECT failed". APPEND must leave the mailbox selectable.
func TestAppendKeepsMailboxSelectable(t *testing.T) {
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const draft = "Subject: Re: test att\r\n\r\nbody\r\n"
	out := runAppend(t, store, `"Drafts" (\Seen \Draft) {`+strconv.Itoa(len(draft))+"}", draft)

	if !strings.Contains(out, "OK APPEND completed") {
		t.Fatalf("APPEND rejected: %q", out)
	}

	ctx := context.Background()
	if _, err := store.MailboxSelect(ctx, "u@e.com", "Drafts"); err != nil {
		t.Fatalf("SELECT failed after APPEND: %v", err)
	}

	rows, err := store.MessageFlags(ctx, "u@e.com", "Drafts")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	var flags []string
	if err := json.Unmarshal([]byte(rows[0].Flags.String), &flags); err != nil {
		t.Fatalf("flags %q is not valid JSON: %v", rows[0].Flags.String, err)
	}
	if strings.Join(flags, ",") != "seen,draft" {
		t.Fatalf("flags = %v, want [seen draft]", flags)
	}
}

// runAppend drives cmdAppend over an in-memory pipe and returns the server's
// output. The literal body is fed through the connection reader, exactly as a
// client would send it after the continuation response.
func runAppend(t *testing.T, store *storage.Store, args, literal string) string {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(client)
		done <- string(data)
	}()

	c := &Connection{
		conn:   server,
		store:  store,
		auth:   "u@e.com",
		reader: bufio.NewReader(strings.NewReader(literal)),
	}
	c.cmdAppend("A1", args)
	server.Close()
	return <-done
}
