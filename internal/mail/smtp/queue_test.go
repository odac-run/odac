package smtp

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/mail/auth"
	"odac/internal/mail/blob"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/storage"
)

// fakeSender answers each attempt with the next scripted result.
type fakeSender struct {
	mu      sync.Mutex
	results []error
	calls   []string
	block   bool // wait for the context instead of answering
}

func (f *fakeSender) Send(ctx context.Context, from, to string, _ []byte) error {
	f.mu.Lock()
	f.calls = append(f.calls, from+"->"+to)
	block := f.block
	var err error
	if len(f.results) > 0 {
		err, f.results = f.results[0], f.results[1:]
	}
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type queueFixture struct {
	q      *Queue
	store  *storage.Store
	blobs  *blob.Store
	sender *fakeSender
	now    time.Time
}

func newQueueFixture(t *testing.T) *queueFixture {
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
	f := &queueFixture{store: store, blobs: blobs, sender: &fakeSender{}, now: time.Now()}
	f.q = NewQueue(store, blobs, f.sender)
	f.q.now = func() time.Time { return f.now }
	return f
}

// round runs one dispatch and waits for its deliveries to finish.
func (f *queueFixture) round(t *testing.T) {
	t.Helper()
	f.q.dispatch(context.Background())
	f.q.wg.Wait()
}

// queued lists every row, leased or not, by claiming far in the future.
func (f *queueFixture) queued(t *testing.T) []storage.OutboundRow {
	t.Helper()
	rows, err := f.store.OutboundClaim(context.Background(), f.now.Add(365*24*time.Hour), time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *queueFixture) inbox(t *testing.T) []storage.MessageRow {
	t.Helper()
	rows, err := f.store.MessageFetch(context.Background(), "ali@emre.red", "INBOX", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

var greylisted = &replyError{code: 451, host: "mx.test", msg: "4.7.1 greylisted, try later", stage: stageRcpt}

// Greylisting answered 4xx and the old client gave up after about 6 seconds,
// losing the message. A temporary failure keeps the row and schedules it
// later; the next attempt delivers it.
func TestQueueRetriesTemporaryFailure(t *testing.T) {
	f := newQueueFixture(t)
	f.sender.results = []error{greylisted, nil}
	if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{"bob@remote.test"}, []byte(testBody)); err != nil {
		t.Fatal(err)
	}

	f.round(t)
	rows := f.queued(t)
	if len(rows) != 1 || rows[0].Attempts != 1 || !strings.Contains(rows[0].LastError, "greylisted") {
		t.Fatalf("after a 4xx the queue holds %+v, want one row with attempts=1", rows)
	}
	if !rows[0].NextAttempt.After(f.now) {
		t.Errorf("retry scheduled at %v, not after now", rows[0].NextAttempt)
	}

	// Not due yet: nothing is attempted.
	f.now = rows[0].NextAttempt.Add(-time.Second)
	f.store.OutboundRetry(context.Background(), rows[0].ID, rows[0].Attempts, rows[0].NextAttempt, rows[0].LastError)
	f.round(t)
	if n := f.sender.callCount(); n != 1 {
		t.Fatalf("attempted %d times before the retry was due", n)
	}

	f.now = rows[0].NextAttempt
	f.round(t)
	if n := f.sender.callCount(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	if rows := f.queued(t); len(rows) != 0 {
		t.Errorf("delivered row still queued: %+v", rows)
	}
	if got := f.inbox(t); len(got) != 0 {
		t.Errorf("a delivered message produced %d bounces", len(got))
	}
}

// A permanent refusal ends the row and tells the sender, who used to see the
// message in Sent and hear nothing.
func TestQueuePermanentFailureBounces(t *testing.T) {
	f := newQueueFixture(t)
	f.sender.results = []error{&replyError{code: 550, host: "mx.test", msg: "5.1.1 no such user", stage: stageRcpt}}
	if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{"nobody@remote.test"}, []byte(testBody)); err != nil {
		t.Fatal(err)
	}

	f.round(t)
	if rows := f.queued(t); len(rows) != 0 {
		t.Fatalf("permanently refused row still queued: %+v", rows)
	}
	inbox := f.inbox(t)
	if len(inbox) != 1 {
		t.Fatalf("sender has %d INBOX messages, want one bounce", len(inbox))
	}
	if !strings.Contains(inbox[0].Subject.String, "Undelivered") {
		t.Errorf("bounce subject = %q", inbox[0].Subject.String)
	}
	raw, err := f.blobs.Get(inbox[0].RawRef.String)
	if err != nil {
		t.Fatalf("bounce has no raw message: %v", err)
	}
	for _, want := range []string{
		"multipart/report; report-type=delivery-status",
		"Final-Recipient: rfc822; nobody@remote.test",
		"Status: 5.1.1",
		"Subject: hi", // the original headers travel with the report
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("bounce lacks %q", want)
		}
	}
}

// Retrying stops once the message is older than the queue lifetime.
func TestQueueExpiredMessageBounces(t *testing.T) {
	f := newQueueFixture(t)
	f.sender.results = []error{greylisted}
	if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{"bob@remote.test"}, []byte(testBody)); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(queueLifetime)

	f.round(t)
	if rows := f.queued(t); len(rows) != 0 {
		t.Fatalf("expired row still queued: %+v", rows)
	}
	inbox := f.inbox(t)
	if len(inbox) != 1 {
		t.Fatalf("sender has %d INBOX messages, want one bounce", len(inbox))
	}
	raw, _ := f.blobs.Get(inbox[0].RawRef.String)
	if !strings.Contains(string(raw), "Status: 4.4.7") {
		t.Errorf("expiry bounce does not report 4.4.7:\n%s", raw)
	}
}

// A restart, which every ODAC update causes, used to drop whatever was in
// flight. Stopping now hands an interrupted delivery back unchanged, so the
// next process picks it up at once.
func TestQueueStopReleasesInFlightDelivery(t *testing.T) {
	f := newQueueFixture(t)
	f.sender.block = true
	f.q.now = time.Now
	if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{"bob@remote.test"}, []byte(testBody)); err != nil {
		t.Fatal(err)
	}

	f.q.Start()
	deadline := time.Now().Add(5 * time.Second)
	for f.sender.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("queue never attempted the delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.q.Stop()

	rows, err := f.store.OutboundClaim(context.Background(), time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Attempts != 0 {
		t.Fatalf("after Stop the next process can claim %+v, want the row with attempts=0", rows)
	}
}

// A submission is queued in the transaction that answers 250, next to the
// sender's Sent copy, and the queue is woken.
func TestSubmissionQueuesRemoteRecipients(t *testing.T) {
	f := newQueueFixture(t)
	outbox := &countingOutbox{}
	b := NewBackend(f.store, f.blobs, auth.NewFirewall(), func() config.Config { return config.Config{} },
		limits.New(limits.SMTPSubmissionProfile()), outbox, "test")
	s := &Session{backend: b, ip: "203.0.113.7", user: "ali@emre.red", from: "ali@emre.red",
		recipients: []string{"bob@remote.test", "carol@remote.test"}}

	if err := s.Data(newBody(testBody)); err != nil {
		t.Fatalf("Data = %v", err)
	}
	rows := f.queued(t)
	if len(rows) != 2 {
		t.Fatalf("queued %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Sender != "ali@emre.red" || !f.blobs.Has(row.RawRef) {
			t.Errorf("row %+v has no readable message", row)
		}
	}
	if outbox.n == 0 {
		t.Error("the queue was not woken")
	}
	sent, _ := f.store.MessageFetch(context.Background(), "ali@emre.red", "Sent", 0, 0)
	if len(sent) != 1 {
		t.Errorf("Sent holds %d copies, want 1", len(sent))
	}
}

// A queued message must outlive the blob sweeper's grace period.
func TestQueuedMessageIsReferenced(t *testing.T) {
	f := newQueueFixture(t)
	if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{"bob@remote.test"}, []byte(testBody)); err != nil {
		t.Fatal(err)
	}
	ref := f.queued(t)[0].RawRef
	if ok, err := f.store.RawRefExists(context.Background(), ref); err != nil || !ok {
		t.Errorf("RawRefExists(queued) = %v, %v, want true", ok, err)
	}
}

func TestEnqueueRejectsInvalidAddresses(t *testing.T) {
	f := newQueueFixture(t)
	for _, to := range []string{"no-at-sign", "bob@remote.test\r\nRCPT TO:<x@y.z>"} {
		if err := f.q.Enqueue(context.Background(), "ali@emre.red", []string{to}, []byte(testBody)); err == nil {
			t.Errorf("Enqueue(%q) accepted an invalid recipient", to)
		}
	}
	if rows := f.queued(t); len(rows) != 0 {
		t.Errorf("invalid recipients were queued: %+v", rows)
	}
}

type countingOutbox struct{ n int }

func (c *countingOutbox) Wake() { c.n++ }
