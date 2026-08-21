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

	"odac/internal/mail/blob"
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
	out := runAppend(t, store, nil, `"Drafts" (\Seen \Draft) {`+strconv.Itoa(len(draft))+"}", draft)

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
func runAppend(t *testing.T, store *storage.Store, blobs *blob.Store, args, literal string) string {
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
		blobs:  blobs,
		auth:   "u@e.com",
		reader: bufio.NewReader(strings.NewReader(literal)),
	}
	c.cmdAppend("A1", args)
	server.Close()
	return <-done
}

// A draft saved with an attachment used to be dumped whole into the html
// column, which loses the attachment and leaves the message list with no
// subject to show. APPEND must store the verbatim bytes and derive the
// columns from them, exactly as an SMTP delivery does.
func TestAppendStoresRawMessageAndDerivesColumns(t *testing.T) {
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out := runAppend(t, store, blobs,
		`"Drafts" (\Draft) {`+strconv.Itoa(len(attachmentMsg))+"}", attachmentMsg)
	if !strings.Contains(out, "OK APPEND completed") {
		t.Fatalf("APPEND rejected: %q", out)
	}

	rows, err := store.MessageFetch(context.Background(), "u@e.com", "Drafts", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]

	if !row.RawRef.Valid || row.RawRef.String == "" {
		t.Fatal("RawRef is empty, the raw message was not stored")
	}
	raw, err := blobs.Get(row.RawRef.String)
	if err != nil {
		t.Fatalf("blob missing: %v", err)
	}
	if string(raw) != attachmentMsg {
		t.Fatal("stored blob is not byte-identical to the appended message")
	}

	if row.Subject.String != "Invoice" {
		t.Fatalf("Subject = %q, want \"Invoice\"", row.Subject.String)
	}
	if !strings.Contains(row.From.String, "s@example.com") {
		t.Fatalf("From = %q, want it to carry s@example.com", row.From.String)
	}
	if !strings.Contains(row.Attachments.String, "invoice.pdf") {
		t.Fatalf("Attachments = %q, want an index entry for invoice.pdf", row.Attachments.String)
	}
	if strings.Contains(row.HTML.String, "Content-Type: multipart") {
		t.Fatal("the raw message was dumped into the html column again")
	}
}

// A blob write failure must not lose the message: it still lands in the
// mailbox through the derived columns.
func TestAppendWithoutBlobStoreStillDelivers(t *testing.T) {
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	out := runAppend(t, store, nil,
		`"Drafts" (\Draft) {`+strconv.Itoa(len(attachmentMsg))+"}", attachmentMsg)
	if !strings.Contains(out, "OK APPEND completed") {
		t.Fatalf("APPEND rejected: %q", out)
	}

	rows, err := store.MessageFetch(context.Background(), "u@e.com", "Drafts", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Subject.String != "Invoice" {
		t.Fatalf("message not stored without a blob store: %+v", rows)
	}
}
