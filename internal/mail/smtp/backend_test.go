package smtp

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/mail/auth"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/storage"
)

func newTestSession(t *testing.T, cfg config.Config) (*Session, *storage.Store) {
	t.Helper()
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "test_mail"))
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	limiter := limits.New(limits.SMTPInboundProfile())
	b := NewBackend(store, nil, auth.NewFirewall(), func() config.Config { return cfg }, limiter, "test")
	return &Session{backend: b, ip: "203.0.113.7", from: "spam@evil.example"}, store
}

func newBody(raw string) io.Reader { return strings.NewReader(raw) }

func countRows(t *testing.T, store *storage.Store, email string) int {
	t.Helper()
	uids, err := store.MessageUIDs(context.Background(), email, "INBOX")
	if err != nil {
		t.Fatalf("MessageUIDs(%q) failed: %v", email, err)
	}
	return len(uids)
}

// A role address is only a valid recipient on a domain this server carries.
// Accepting postmaster@anything let any sender write rows under an address no
// session can log in as.
func TestRcpt_RoleAddressRequiresConfiguredDomain(t *testing.T) {
	cfg := config.Config{Domains: map[string]config.Domain{"emre.red": {}}}
	s, _ := newTestSession(t, cfg)

	accepted := []string{
		"postmaster@emre.red",
		"hostmaster@emre.red",
		"PostMaster@EMRE.RED",
		"postmaster@mail.emre.red", // walk-up to the configured parent
	}
	for _, to := range accepted {
		if err := s.Rcpt(to, nil); err != nil {
			t.Errorf("Rcpt(%q) = %v, want accepted", to, err)
		}
	}

	refused := []string{
		"postmaster@not-ours.com",
		"hostmaster@attacker.example",
		"postmaster@emre.red.evil.net",
	}
	for _, to := range refused {
		if err := s.Rcpt(to, nil); err == nil {
			t.Errorf("Rcpt(%q) = nil, want relay refusal", to)
		}
	}
}

func TestRcpt_RoleAddressRefusedWithNoDomainsConfigured(t *testing.T) {
	s, _ := newTestSession(t, config.Config{})
	if err := s.Rcpt("postmaster@emre.red", nil); err == nil {
		t.Error("a server carrying no domains must not accept a role address")
	}
}

// The Data path re-asks the same question: a domain check that held only at
// RCPT would still let a hand-built transaction store the row.
func TestData_RoleAddressWithNoMailboxStoresNothing(t *testing.T) {
	cfg := config.Config{Domains: map[string]config.Domain{"emre.red": {}}}
	s, store := newTestSession(t, cfg)

	if err := s.Rcpt("postmaster@emre.red", nil); err != nil {
		t.Fatalf("Rcpt failed: %v", err)
	}
	if err := s.Data(newBody("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("Data failed: %v", err)
	}

	if n := countRows(t, store, "postmaster@emre.red"); n != 0 {
		t.Errorf("stored %d rows for a role address with no mailbox, want 0", n)
	}
}

// An account behind the role address makes the mail readable, so it is kept.
func TestData_RoleAddressWithMailboxStoresMessage(t *testing.T) {
	cfg := config.Config{Domains: map[string]config.Domain{"emre.red": {}}}
	s, store := newTestSession(t, cfg)

	ctx := context.Background()
	if err := store.AccountCreate(ctx, "postmaster@emre.red", "x", "emre.red"); err != nil {
		t.Fatalf("AccountCreate failed: %v", err)
	}

	if err := s.Rcpt("postmaster@emre.red", nil); err != nil {
		t.Fatalf("Rcpt failed: %v", err)
	}
	if err := s.Data(newBody("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("Data failed: %v", err)
	}

	if n := countRows(t, store, "postmaster@emre.red"); n != 1 {
		t.Errorf("stored %d rows, want 1", n)
	}
}

// Envelope case must not fragment a mailbox: <Ali@x> and <ali@x> are one
// account, and the row is keyed to the account's stored spelling.
func TestRcptAndData_AreCaseInsensitive(t *testing.T) {
	s, store := newTestSession(t, config.Config{})

	ctx := context.Background()
	if err := store.AccountCreate(ctx, "ali@emre.red", "x", "emre.red"); err != nil {
		t.Fatalf("AccountCreate failed: %v", err)
	}

	if err := s.Rcpt("ALI@EMRE.RED", nil); err != nil {
		t.Fatalf("Rcpt with an upper-cased address = %v, want accepted", err)
	}
	if err := s.Data(newBody("Subject: hi\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("Data failed: %v", err)
	}

	if n := countRows(t, store, "ali@emre.red"); n != 1 {
		t.Errorf("canonical mailbox holds %d rows, want 1", n)
	}
	if n := countRows(t, store, "ALI@EMRE.RED"); n != 0 {
		t.Errorf("a second mailbox was created under the peer's spelling (%d rows)", n)
	}
}

func TestRcpt_UnknownLocalAccountStillRefused(t *testing.T) {
	cfg := config.Config{Domains: map[string]config.Domain{"emre.red": {}}}
	s, _ := newTestSession(t, cfg)
	if err := s.Rcpt("nobody@emre.red", nil); err == nil {
		t.Error("a configured domain must not make every address deliverable")
	}
}
