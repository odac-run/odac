package smtp

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	gosmtp "github.com/emersion/go-smtp"

	"odac/internal/mail/auth"
	"odac/internal/mail/blob"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/spam"
	"odac/internal/mail/storage"
)

// fakeScreener answers every message with one verdict built by a real Filter
// in tag mode (so Stamp has something to write), then overrides the action.
type fakeScreener struct {
	action spam.Action
	calls  []spam.Envelope
}

func (f *fakeScreener) Check(ctx context.Context, env spam.Envelope, raw []byte) spam.Verdict {
	f.calls = append(f.calls, env)
	v := spam.New(func() config.Config { return config.Config{Hostname: "mx.test"} }, spam.ModeTag,
		spam.WithResolver(nullDNS{})).Check(ctx, env, raw)
	v.Action = f.action
	if f.action == spam.Reject {
		v.Reason = "rejected per policy"
	}
	return v
}

func newScreenedSession(t *testing.T, action spam.Action) (*Session, *storage.Store, *blob.Store, *fakeScreener) {
	t.Helper()
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AccountCreate(context.Background(), "ali@emre.red", "x", "emre.red"); err != nil {
		t.Fatal(err)
	}
	b := NewBackend(store, blobs, auth.NewFirewall(), func() config.Config { return config.Config{} },
		limits.New(limits.SMTPInboundProfile()), nil, "test")
	fs := &fakeScreener{action: action}
	b.screener = fs
	s := &Session{backend: b, ip: "203.0.113.7", helo: "mail.sender.example", from: "x@sender.example",
		recipients: []string{"ali@emre.red"}}
	return s, store, blobs, fs
}

const screenedBody = "From: x@sender.example\r\nSubject: hi\r\n\r\nbody\r\n"

func uids(t *testing.T, store *storage.Store, mailbox string) []int64 {
	t.Helper()
	u, err := store.MessageUIDs(context.Background(), "ali@emre.red", mailbox)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestData_SpamVerdictPlacesMessage(t *testing.T) {
	for _, tc := range []struct {
		action spam.Action
		box    string
	}{{spam.Inbox, "INBOX"}, {spam.Junk, "Junk"}} {
		s, store, blobs, fs := newScreenedSession(t, tc.action)
		if err := s.Data(newBody(screenedBody)); err != nil {
			t.Fatalf("%s: Data failed: %v", tc.action, err)
		}
		if len(fs.calls) != 1 || fs.calls[0].Helo != "mail.sender.example" || fs.calls[0].MailFrom != "x@sender.example" || fs.calls[0].IP.String() != "203.0.113.7" {
			t.Fatalf("%s: screener saw %+v", tc.action, fs.calls)
		}
		if n := len(uids(t, store, tc.box)); n != 1 {
			t.Fatalf("%s: %s holds %d rows, want 1", tc.action, tc.box, n)
		}
		rows, err := store.MessageFetch(context.Background(), "ali@emre.red", tc.box, 1, 1<<40)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: fetch = %v, %v", tc.action, rows, err)
		}
		raw, err := blobs.Get(rows[0].RawRef.String)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(raw), "Authentication-Results: mx.test;") || !strings.HasSuffix(string(raw), screenedBody) {
			t.Errorf("%s: stored blob is not the stamped original:\n%s", tc.action, raw)
		}
	}
}

// A refusal answers 550 and stores nothing, so the sending server, not this
// one, tells the sender.
func TestData_SpamRejectAnswers550(t *testing.T) {
	s, store, _, _ := newScreenedSession(t, spam.Reject)
	err := s.Data(newBody(screenedBody))
	var smtpErr *gosmtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 550 || smtpErr.EnhancedCode != (gosmtp.EnhancedCode{5, 7, 1}) {
		t.Fatalf("Data = %v, want 550 5.7.1", err)
	}
	if n := len(uids(t, store, "INBOX")) + len(uids(t, store, "Junk")); n != 0 {
		t.Errorf("stored %d rows for a refused message", n)
	}
}

func TestData_SpamDeferAnswers451(t *testing.T) {
	s, store, _, _ := newScreenedSession(t, spam.Defer)
	err := s.Data(newBody(screenedBody))
	var smtpErr *gosmtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 451 {
		t.Fatalf("Data = %v, want 451", err)
	}
	if n := len(uids(t, store, "INBOX")) + len(uids(t, store, "Junk")); n != 0 {
		t.Errorf("stored %d rows for a deferred message", n)
	}
}

// Bounces and delivery reports arrive with MAIL FROM:<>; refusing it lost
// every DSN for mail a local user sent.
func TestMail_NullSender(t *testing.T) {
	s, store, _, fs := newScreenedSession(t, spam.Inbox)
	if err := s.Mail("", nil); err != nil {
		t.Fatalf("Mail(<>) = %v, want accepted", err)
	}
	if err := s.Data(newBody("From: MAILER-DAEMON@sender.example\r\nSubject: Undelivered\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("Data failed: %v", err)
	}
	if n := len(uids(t, store, "INBOX")); n != 1 {
		t.Errorf("INBOX holds %d rows, want the bounce", n)
	}
	if len(fs.calls) != 1 || fs.calls[0].MailFrom != "" {
		t.Errorf("screener saw %+v, want the null sender", fs.calls)
	}

	s.user = "ali@emre.red"
	if err := s.Mail("", nil); err == nil {
		t.Error("an authenticated session must name its own sender")
	}
}

// A logged-in sender is never screened, even when delivering locally.
func TestData_AuthenticatedSessionNotScreened(t *testing.T) {
	s, store, _, fs := newScreenedSession(t, spam.Reject)
	if err := store.AccountCreate(context.Background(), "veli@emre.red", "x", "emre.red"); err != nil {
		t.Fatal(err)
	}
	s.user, s.from = "veli@emre.red", "veli@emre.red"
	if err := s.Data(newBody(screenedBody)); err != nil {
		t.Fatalf("Data failed: %v", err)
	}
	if len(fs.calls) != 0 {
		t.Errorf("authenticated session was screened %d times", len(fs.calls))
	}
	if n := len(uids(t, store, "INBOX")); n != 1 {
		t.Errorf("INBOX holds %d rows, want 1", n)
	}
}

// nullDNS answers NXDOMAIN for everything.
type nullDNS struct{}

func nx(name string) error { return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true} }

func (nullDNS) LookupTXT(_ context.Context, n string) ([]string, error)        { return nil, nx(n) }
func (nullDNS) LookupMX(_ context.Context, n string) ([]*net.MX, error)        { return nil, nx(n) }
func (nullDNS) LookupIPAddr(_ context.Context, n string) ([]net.IPAddr, error) { return nil, nx(n) }
func (nullDNS) LookupAddr(_ context.Context, n string) ([]string, error)       { return nil, nx(n) }

// Mail to oneself lands in INBOX as well as Sent, and is not screened.
func TestData_SelfSendReachesInbox(t *testing.T) {
	s, store, _, fs := newScreenedSession(t, spam.Reject)
	s.user, s.from = "ali@emre.red", "ali@emre.red"
	if err := s.Data(newBody(screenedBody)); err != nil {
		t.Fatalf("Data failed: %v", err)
	}
	if n := len(uids(t, store, "INBOX")); n != 1 {
		t.Errorf("INBOX holds %d rows, want 1", n)
	}
	if n := len(uids(t, store, "Sent")); n != 1 {
		t.Errorf("Sent holds %d rows, want 1", n)
	}
	if len(fs.calls) != 0 {
		t.Error("an authenticated self-send must not be screened")
	}
}

// From outside, a self-send naming a local account is screened like any other
// message instead of being accepted and silently dropped.
func TestData_ForgedSelfSendIsScreened(t *testing.T) {
	s, _, _, fs := newScreenedSession(t, spam.Reject)
	s.from = "ali@emre.red"
	if err := s.Data(newBody(screenedBody)); err == nil {
		t.Fatal("the screener's refusal was not applied")
	}
	if len(fs.calls) != 1 {
		t.Errorf("screened %d times, want 1", len(fs.calls))
	}
}
