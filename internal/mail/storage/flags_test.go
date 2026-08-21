package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestCanonicalFlags(t *testing.T) {
	got := CanonicalFlags([]string{`\Seen`, `\Draft`, `\SEEN`, ``, `$Label1`})
	want := []string{"seen", "draft", "$label1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestEncodeFlagsAlwaysProducesValidJSON(t *testing.T) {
	cases := [][]string{
		nil,
		{`\Seen`},
		{`weird"quote`},
		{`back\slash`},
	}
	for _, in := range cases {
		out := EncodeFlags(CanonicalFlags(in))
		var decoded []string
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("EncodeFlags(%v) = %q, not valid JSON: %v", in, out, err)
		}
	}
}

// A mailbox holding one row with a malformed flags value must still select.
// This is the "SELECT failed" a client reported after APPEND wrote `[\Seen`.
func TestMailboxSelectSurvivesMalformedFlags(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.MessageStore(ctx, &MessageRow{
		Email: "u@e.com", Mailbox: "Drafts", Flags: ns("[]"),
	}); err != nil {
		t.Fatal(err)
	}
	corruptFlags(t, store, `[\Seen`)

	stats, err := store.MailboxSelect(ctx, "u@e.com", "Drafts")
	if err != nil {
		t.Fatalf("MailboxSelect failed on malformed flags: %v", err)
	}
	if stats.Exists != 1 {
		t.Fatalf("Exists = %d, want 1", stats.Exists)
	}
}

func TestExpungeSurvivesMalformedFlags(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.MessageStore(ctx, &MessageRow{
		Email: "u@e.com", Mailbox: "Drafts", Flags: ns("[]"),
	}); err != nil {
		t.Fatal(err)
	}
	corruptFlags(t, store, `[\Deleted`)

	if _, err := store.MessageExpunge(ctx, "u@e.com", "Drafts"); err != nil {
		t.Fatalf("MessageExpunge failed on malformed flags: %v", err)
	}
}

func TestStoreFlagsSurvivesMalformedFlags(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := store.MessageStore(ctx, &MessageRow{
		Email: "u@e.com", Mailbox: "Drafts", Flags: ns("[]"),
	}); err != nil {
		t.Fatal(err)
	}
	corruptFlags(t, store, `[\Seen`)

	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1}, "add", []string{"seen"}); err != nil {
		t.Fatalf("add flag failed on malformed flags: %v", err)
	}
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1}, "remove", []string{"seen"}); err != nil {
		t.Fatalf("remove flag failed on malformed flags: %v", err)
	}
}

// Reopening the store must salvage the readable flag names rather than leaving
// the row unreadable or dropping the flags entirely.
func TestMigrateRepairsMalformedFlags(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mail")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.MessageStore(ctx, &MessageRow{
		Email: "u@e.com", Mailbox: "Drafts", Flags: ns("[]"),
	}); err != nil {
		t.Fatal(err)
	}
	corruptFlags(t, store, `[\Seen \Draft`)
	store.Close()

	store, err = NewStore(dbPath)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer store.Close()

	rows, err := store.MessageFlags(ctx, "u@e.com", "Drafts")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Flags.String != `["seen","draft"]` {
		t.Fatalf("flags = %q, want [\"seen\",\"draft\"]", rows[0].Flags.String)
	}
}

func corruptFlags(t *testing.T, store *Store, raw string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE mail_received SET flags = ?`, raw); err != nil {
		t.Fatal(err)
	}
}
