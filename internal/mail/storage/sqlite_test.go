package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	want := []AccountEntry{
		{Domain: "example.com", Email: "a@example.com"},
		{Domain: "example.com", Email: "b@example.com"},
	}
	if !reflect.DeepEqual(accounts, want) {
		t.Errorf("AccountList = %v, want %v", accounts, want)
	}

	all, err := store.AccountListAll(ctx)
	if err != nil {
		t.Fatalf("AccountListAll failed: %v", err)
	}
	want = append(want, AccountEntry{Domain: "other.com", Email: "c@other.com"})
	if !reflect.DeepEqual(all, want) {
		t.Errorf("AccountListAll = %v, want %v", all, want)
	}

	if empty, _ := store.AccountList(ctx, "none.test"); !reflect.DeepEqual(empty, []AccountEntry{}) {
		t.Errorf("AccountList empty = %v", empty)
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

func TestMessageStoreFlags(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Two messages get UIDs 1 and 2; a third UID 3 is a control that must stay empty.
	for i := 0; i < 3; i++ {
		if err := store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")}); err != nil {
			t.Fatalf("MessageStore failed: %v", err)
		}
	}

	flagsFor := func(uid int64) string {
		msgs, err := store.MessageFetch(ctx, "u@e.com", "INBOX", uid, uid)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("fetch uid %d failed: %v (%d rows)", uid, err, len(msgs))
		}
		return msgs[0].Flags.String
	}

	// add: applies only to the targeted UIDs, leaving UID 3 untouched.
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1, 2}, "add", []string{"seen"}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	for _, uid := range []int64{1, 2} {
		if !strings.Contains(flagsFor(uid), "seen") {
			t.Errorf("uid %d should have 'seen', got %q", uid, flagsFor(uid))
		}
	}
	if strings.Contains(flagsFor(3), "seen") {
		t.Errorf("uid 3 must be untouched, got %q", flagsFor(3))
	}

	// remove: drops the flag from UID 1 only.
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1}, "remove", []string{"seen"}); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	if strings.Contains(flagsFor(1), "seen") {
		t.Errorf("uid 1 should no longer have 'seen', got %q", flagsFor(1))
	}
	if !strings.Contains(flagsFor(2), "seen") {
		t.Errorf("uid 2 should still have 'seen', got %q", flagsFor(2))
	}

	// set: replaces the whole flag list, and a value with a quote must not break JSON.
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{2}, "set", []string{`weird"flag`, "deleted"}); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	got := flagsFor(2)
	if !strings.Contains(got, "deleted") || !strings.Contains(got, `weird`) {
		t.Errorf("uid 2 flags after set = %q", got)
	}
	// Stored value must be valid JSON (quote correctly escaped by json.Marshal).
	var parsed []string
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Errorf("stored flags are not valid JSON: %q (%v)", got, err)
	}
}

func TestMessageStoreFlags_EmptySetClearsFlags(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	if err := store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")}); err != nil {
		t.Fatalf("MessageStore failed: %v", err)
	}
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1}, "add", []string{"seen", "flagged"}); err != nil {
		t.Fatalf("add failed: %v", err)
	}

	// STORE 1 FLAGS () arrives here as action "set" with no flags and must
	// clear the list, not silently do nothing.
	if err := store.MessageStoreFlags(ctx, "u@e.com", []int64{1}, "set", nil); err != nil {
		t.Fatalf("set with empty flags failed: %v", err)
	}

	msgs, err := store.MessageFetch(ctx, "u@e.com", "INBOX", 1, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch failed: %v (%d rows)", err, len(msgs))
	}
	if got := msgs[0].Flags.String; got != "[]" {
		t.Errorf("flags after FLAGS () = %q, want %q", got, "[]")
	}
}

func TestMessageStoreFlags_LargeUIDSet(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const messages = 3
	for i := 0; i < messages; i++ {
		if err := store.MessageStore(ctx, &MessageRow{Email: "u@e.com", Mailbox: "INBOX", Flags: ns("[]")}); err != nil {
			t.Fatalf("MessageStore failed: %v", err)
		}
	}

	// More UIDs than SQLITE_MAX_VARIABLE_NUMBER (32766): a single IN clause
	// would fail with "too many SQL variables", so the update must be batched.
	uids := make([]int64, 40000)
	for i := range uids {
		uids[i] = int64(i + 1)
	}
	if err := store.MessageStoreFlags(ctx, "u@e.com", uids, "add", []string{"seen"}); err != nil {
		t.Fatalf("add over %d UIDs failed: %v", len(uids), err)
	}

	for uid := int64(1); uid <= messages; uid++ {
		msgs, err := store.MessageFetch(ctx, "u@e.com", "INBOX", uid, uid)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("fetch uid %d failed: %v (%d rows)", uid, err, len(msgs))
		}
		if !strings.Contains(msgs[0].Flags.String, "seen") {
			t.Errorf("uid %d should have 'seen', got %q", uid, msgs[0].Flags.String)
		}
	}
}

func TestMessageStoreFlags_UnknownAction(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	if err := store.MessageStoreFlags(context.Background(), "u@e.com", []int64{1}, "toggle", []string{"seen"}); err == nil {
		t.Error("unknown action should return an error, got nil")
	}
}
