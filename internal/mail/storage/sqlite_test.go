package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_mail")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cleanup := func() {
		store.Close()
		os.RemoveAll(dir)
	}
	return store, cleanup
}

func TestNewStore_CreatesDatabase(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	if store == nil {
		t.Fatal("store should not be nil")
	}
}

func TestAccountCreate_And_Exists(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := store.AccountCreate(ctx, "test@example.com", "scrypt$abc$def", "example.com")
	if err != nil {
		t.Fatalf("AccountCreate failed: %v", err)
	}

	account, err := store.AccountExists(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("AccountExists failed: %v", err)
	}
	if account == nil {
		t.Fatal("account should exist")
	}
	if account.Email != "test@example.com" {
		t.Errorf("email mismatch: got %s", account.Email)
	}
	if account.Domain != "example.com" {
		t.Errorf("domain mismatch: got %s", account.Domain)
	}
}

func TestAccountExists_NotFound(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	account, err := store.AccountExists(ctx, "nonexistent@example.com")
	if err != nil {
		t.Fatalf("AccountExists failed: %v", err)
	}
	if account != nil {
		t.Error("non-existent account should return nil")
	}
}

func TestAccountDelete(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.AccountCreate(ctx, "delete@example.com", "hash", "example.com")

	err := store.AccountDelete(ctx, "delete@example.com")
	if err != nil {
		t.Fatalf("AccountDelete failed: %v", err)
	}

	account, _ := store.AccountExists(ctx, "delete@example.com")
	if account != nil {
		t.Error("deleted account should not exist")
	}
}

func TestAccountUpdatePassword(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.AccountCreate(ctx, "update@example.com", "old_hash", "example.com")

	err := store.AccountUpdatePassword(ctx, "update@example.com", "new_hash")
	if err != nil {
		t.Fatalf("AccountUpdatePassword failed: %v", err)
	}

	account, _ := store.AccountExists(ctx, "update@example.com")
	if account.Password != "new_hash" {
		t.Errorf("password should be updated, got: %s", account.Password)
	}
}

func TestAccountList(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.AccountCreate(ctx, "a@example.com", "h", "example.com")
	store.AccountCreate(ctx, "b@example.com", "h", "example.com")
	store.AccountCreate(ctx, "c@other.com", "h", "other.com")

	accounts, err := store.AccountList(ctx, "example.com")
	if err != nil {
		t.Fatalf("AccountList failed: %v", err)
	}
	if len(accounts) != 2 {
		t.Errorf("expected 2 accounts for example.com, got %d", len(accounts))
	}
}

func TestMailboxList_DefaultINBOX(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	boxes, err := store.MailboxList(ctx, "user@example.com")
	if err != nil {
		t.Fatalf("MailboxList failed: %v", err)
	}
	if len(boxes) != 1 || boxes[0] != "INBOX" {
		t.Errorf("default mailbox list should be [INBOX], got %v", boxes)
	}
}

func TestMailboxCreate_And_List(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.MailboxCreate(ctx, "user@example.com", "Sent")
	store.MailboxCreate(ctx, "user@example.com", "Drafts")

	boxes, err := store.MailboxList(ctx, "user@example.com")
	if err != nil {
		t.Fatalf("MailboxList failed: %v", err)
	}
	if len(boxes) != 3 {
		t.Errorf("expected 3 mailboxes (INBOX + 2), got %d: %v", len(boxes), boxes)
	}
}

func TestMailboxRename(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.MailboxCreate(ctx, "user@example.com", "OldName")
	store.MailboxRename(ctx, "user@example.com", "OldName", "NewName")

	boxes, _ := store.MailboxList(ctx, "user@example.com")
	found := false
	for _, b := range boxes {
		if b == "NewName" {
			found = true
		}
		if b == "OldName" {
			t.Error("old mailbox name should not exist after rename")
		}
	}
	if !found {
		t.Error("renamed mailbox should exist")
	}
}

func TestMailboxDelete(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.MailboxCreate(ctx, "user@example.com", "Trash")
	store.MailboxDelete(ctx, "user@example.com", "Trash")

	boxes, _ := store.MailboxList(ctx, "user@example.com")
	for _, b := range boxes {
		if b == "Trash" {
			t.Error("deleted mailbox should not exist")
		}
	}
}

func TestMessageStore_And_Fetch(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	msg := &MessageRow{
		Email:   "user@example.com",
		Mailbox: "INBOX",
		Subject: ns("Test Subject"),
		Text:    ns("Hello World"),
		Flags:   ns("[]"),
	}

	err := store.MessageStore(ctx, msg)
	if err != nil {
		t.Fatalf("MessageStore failed: %v", err)
	}

	messages, err := store.MessageFetch(ctx, "user@example.com", "INBOX", 0, 0)
	if err != nil {
		t.Fatalf("MessageFetch failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].UID != 1 {
		t.Errorf("first message UID should be 1, got %d", messages[0].UID)
	}
}

func TestMessageStore_AutoIncrementUID(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		store.MessageStore(ctx, &MessageRow{
			Email:   "user@example.com",
			Mailbox: "INBOX",
			Flags:   ns("[]"),
		})
	}

	messages, _ := store.MessageFetch(ctx, "user@example.com", "INBOX", 0, 0)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	// Messages are ordered ASC by uid, so UIDs should be 1, 2, 3
	expectedUIDs := []int64{1, 2, 3}
	for i, m := range messages {
		if m.UID != expectedUIDs[i] {
			t.Errorf("message %d: expected UID %d, got %d", i, expectedUIDs[i], m.UID)
		}
	}
}

func TestMailboxSelect(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	// Store 3 messages, mark 1 as seen
	store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns(`["seen"]`)})
	store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")})
	store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")})

	stats, err := store.MailboxSelect(ctx, "u@e.com", "INBOX")
	if err != nil {
		t.Fatalf("MailboxSelect failed: %v", err)
	}
	if stats.Exists != 3 {
		t.Errorf("expected 3 messages, got %d", stats.Exists)
	}
	if stats.Unseen != 2 {
		t.Errorf("expected 2 unseen, got %d", stats.Unseen)
	}
}

func TestMessageExpunge(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns(`["deleted"]`)})
	store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")})

	uids, err := store.MessageExpunge(ctx, "u@e.com", "INBOX")
	if err != nil {
		t.Fatalf("MessageExpunge failed: %v", err)
	}
	if len(uids) != 1 {
		t.Errorf("expected 1 expunged UID, got %d", len(uids))
	}

	remaining, _ := store.MessageFetch(ctx, "u@e.com", "INBOX", 0, 0)
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining message, got %d", len(remaining))
	}
}

// ns is a test helper for creating sql.NullString values.
func ns(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}
