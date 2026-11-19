package smtp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"odac/internal/mail/auth"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/spam"
	"odac/internal/mail/storage"
	tt "odac/internal/mail/tnef/tneftest"
	"odac/internal/mail/winmail"
)

func winmailBody(from string) string {
	stream := tt.New().Class("IPM.Note").File("rapor.pdf", "application/pdf", []byte("%PDF rapor")).Bytes()
	return "From: " + from + "\r\nSubject: rapor\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nekte\r\n" +
		"--b\r\nContent-Type: application/ms-tnef\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(stream) + "\r\n--b--\r\n"
}

func onlyRow(t *testing.T, store *storage.Store, email, mailbox string) storage.MessageRow {
	t.Helper()
	rows, err := store.MessageFetch(context.Background(), email, mailbox, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%s %s holds %d rows (%v), want 1", email, mailbox, len(rows), err)
	}
	return rows[0]
}

// The mailbox keeps the converted copy, screened headers included, and the
// message as received stays in the blob store beside it.
func TestDataExpandsWinmail(t *testing.T) {
	s, store, blobs, _ := newScreenedSession(t, spam.Inbox)
	s.backend.rewrite = winmail.Expand
	if err := s.Data(newBody(winmailBody("x@sender.example"))); err != nil {
		t.Fatal(err)
	}
	row := onlyRow(t, store, "ali@emre.red", "INBOX")
	converted, err := blobs.Get(row.RawRef.String)
	if err != nil {
		t.Fatal(err)
	}
	original, err := blobs.Get(row.OriginalRef.String)
	if err != nil {
		t.Fatalf("original not stored: %v", err)
	}
	if !bytes.HasPrefix(converted, []byte(winmail.Header+": converted\r\nAuthentication-Results: ")) {
		t.Errorf("converted copy:\n%s", converted)
	}
	if !bytes.HasPrefix(original, []byte("Authentication-Results: ")) || !bytes.Contains(original, []byte("application/ms-tnef")) {
		t.Errorf("original copy:\n%s", original)
	}
	if !strings.Contains(row.Attachments.String, `"rapor.pdf"`) || strings.Contains(row.Attachments.String, "ms-tnef") {
		t.Errorf("attachments index = %s", row.Attachments.String)
	}
	if ok, err := store.RawRefExists(context.Background(), row.OriginalRef.String); !ok || err != nil {
		t.Errorf("original is not referenced: %v, %v", ok, err)
	}
}

func TestDataDeliversUnconvertedOnFailure(t *testing.T) {
	for name, rewrite := range map[string]Rewrite{
		"error":     func([]byte) ([]byte, error) { return nil, errors.New("broken stream") },
		"no change": func([]byte) ([]byte, error) { return nil, nil },
	} {
		s, store, blobs, _ := newScreenedSession(t, spam.Inbox)
		s.backend.rewrite = rewrite
		if err := s.Data(newBody(winmailBody("x@sender.example"))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		row := onlyRow(t, store, "ali@emre.red", "INBOX")
		raw, _ := blobs.Get(row.RawRef.String)
		if row.OriginalRef.Valid || !bytes.Contains(raw, []byte("application/ms-tnef")) {
			t.Errorf("%s: row %+v stores %q", name, row, raw)
		}
	}
}

// What the sender wrote is what the queue sends and what Sent keeps; only
// the local recipient's copy is converted.
func TestSubmissionConvertsOnlyLocalCopies(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	if err := f.store.AccountCreate(ctx, "veli@emre.red", "x", "emre.red"); err != nil {
		t.Fatal(err)
	}
	b := NewBackend(f.store, f.blobs, auth.NewFirewall(), func() config.Config { return config.Config{} },
		limits.New(limits.SMTPSubmissionProfile()), nil, "test")
	b.rewrite = winmail.Expand
	s := &Session{backend: b, ip: "203.0.113.7", user: "ali@emre.red", from: "ali@emre.red",
		recipients: []string{"veli@emre.red", "bob@remote.test"}}
	body := winmailBody("ali@emre.red")
	if err := s.Data(newBody(body)); err != nil {
		t.Fatal(err)
	}

	sent := onlyRow(t, f.store, "ali@emre.red", "Sent")
	queued := f.queued(t)
	if len(queued) != 1 || queued[0].RawRef != sent.RawRef.String || sent.OriginalRef.Valid {
		t.Errorf("sent %+v, queued %+v", sent, queued)
	}
	if raw, _ := f.blobs.Get(sent.RawRef.String); string(raw) != body {
		t.Errorf("Sent copy changed:\n%s", raw)
	}
	if !strings.Contains(sent.Attachments.String, "ms-tnef") {
		t.Errorf("Sent attachments index = %s", sent.Attachments.String)
	}

	inbox := onlyRow(t, f.store, "veli@emre.red", "INBOX")
	if inbox.OriginalRef.String != sent.RawRef.String || inbox.RawRef == sent.RawRef {
		t.Errorf("local copy %+v not converted from %s", inbox, sent.RawRef.String)
	}
}

// Without the blob store there is nowhere to keep the original, so nothing is
// converted.
func TestDataNeedsTheOriginalStored(t *testing.T) {
	s, store, _, _ := newScreenedSession(t, spam.Inbox)
	s.backend.blobs = nil
	called := false
	s.backend.rewrite = func([]byte) ([]byte, error) { called = true; return []byte("x"), nil }
	if err := s.Data(newBody(winmailBody("x@sender.example"))); err != nil {
		t.Fatal(err)
	}
	if row := onlyRow(t, store, "ali@emre.red", "INBOX"); called || row.OriginalRef.Valid {
		t.Errorf("rewrote with no original kept: called=%v row=%+v", called, row)
	}
}
