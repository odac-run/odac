package smtp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac/internal/mail/address"
	"odac/internal/mail/blob"
	"odac/internal/mail/message"
	"odac/internal/mail/storage"
)

const (
	queueWorkers  = 8
	queuePoll     = 10 * time.Second
	queueLease    = 30 * time.Minute
	queueLifetime = 5 * 24 * time.Hour
	// queueBookkeeping bounds the database write that closes an attempt. It
	// runs on its own context so a shutdown still records the outcome.
	queueBookkeeping = 10 * time.Second
)

// queueBackoff is the wait before each retry, the last entry repeating until
// the message expires. RFC 5321 section 4.5.4.1 asks for retries spread over
// at least four to five days, starting no sooner than a few minutes apart.
var queueBackoff = []time.Duration{
	time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 4 * time.Hour,
}

// Sender is one delivery attempt to one remote recipient.
type Sender interface {
	Send(ctx context.Context, from, to string, body []byte) error
}

// Queue is the durable outbound queue. Accepting a message and delivering it
// are separate steps: the transaction that answers 250 writes the queue rows,
// and the queue retries temporary failures with backoff until the message
// expires, then reports a permanent failure to the sender. Without it a
// greylisting server, or an odac-mail restart, lost the message silently.
type Queue struct {
	blobs    *blob.Store
	lifetime time.Duration
	now      func() time.Time
	pruner   interface{ Prune() }
	sender   Sender
	store    *storage.Store
	wake     chan struct{}

	slots chan struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewQueue creates the queue over the mail store. ODAC_MAIL_QUEUE_HOURS
// overrides how long a message is retried before it bounces.
func NewQueue(store *storage.Store, blobs *blob.Store, sender Sender) *Queue {
	q := &Queue{
		blobs:    blobs,
		lifetime: queueLifetime,
		now:      time.Now,
		sender:   sender,
		store:    store,
		wake:     make(chan struct{}, 1),
		slots:    make(chan struct{}, queueWorkers),
	}
	if p, ok := sender.(interface{ Prune() }); ok {
		q.pruner = p
	}
	if raw := os.Getenv("ODAC_MAIL_QUEUE_HOURS"); raw != "" {
		if h, err := strconv.Atoi(raw); err == nil && h > 0 {
			q.lifetime = time.Duration(h) * time.Hour
		} else {
			log.Printf("[SMTP Queue] Ignoring invalid ODAC_MAIL_QUEUE_HOURS=%q", raw)
		}
	}
	return q
}

// Enqueue accepts one message for remote recipients: the message goes to the
// blob store and one queue row per recipient is committed before it returns.
func (q *Queue) Enqueue(ctx context.Context, from string, to []string, body []byte) error {
	if !address.Valid(from) {
		return fmt.Errorf("invalid sender address %q", from)
	}
	for _, rcpt := range to {
		if !address.Valid(rcpt) {
			return fmt.Errorf("invalid recipient address %q", rcpt)
		}
	}
	if q.blobs == nil {
		return errors.New("no message store configured")
	}
	ref, err := q.blobs.Put(body)
	if err != nil {
		return fmt.Errorf("message store failed: %w", err)
	}
	if err := q.store.Deliver(ctx, nil, OutboundRows(from, to, ref, q.now())); err != nil {
		return err
	}
	q.Wake()
	return nil
}

// OutboundRows builds the queue rows for one message, due immediately.
func OutboundRows(from string, to []string, rawRef string, now time.Time) []storage.OutboundRow {
	rows := make([]storage.OutboundRow, 0, len(to))
	for _, rcpt := range to {
		rows = append(rows, storage.OutboundRow{
			Created: now, NextAttempt: now, RawRef: rawRef, Recipient: rcpt, Sender: from,
		})
	}
	return rows
}

// Wake makes the worker look for due rows now instead of at its next poll.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start begins draining the queue on a tracked goroutine.
func (q *Queue) Start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.run(ctx)
	}()
}

// Stop ends the worker and every delivery in flight, then waits for them.
// An interrupted delivery keeps its row and is retried by the next process.
func (q *Queue) Stop() {
	q.mu.Lock()
	cancel := q.cancel
	q.cancel = nil
	q.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	q.wg.Wait()
}

func (q *Queue) run(ctx context.Context) {
	ticker := time.NewTicker(queuePoll)
	defer ticker.Stop()
	for {
		q.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
			if q.pruner != nil {
				q.pruner.Prune()
			}
		}
	}
}

// dispatch claims as many due rows as there are free workers and starts them.
// A finished delivery wakes the loop, so a backlog drains at full width.
func (q *Queue) dispatch(ctx context.Context) {
	free := cap(q.slots) - len(q.slots)
	if free == 0 || ctx.Err() != nil {
		return
	}
	rows, err := q.store.OutboundClaim(ctx, q.now(), queueLease, free)
	if err != nil {
		log.Printf("[SMTP Queue] Claim failed: %v", err)
		return
	}
	for _, row := range rows {
		q.slots <- struct{}{}
		q.wg.Add(1)
		go func(row storage.OutboundRow) {
			defer q.wg.Done()
			defer func() {
				<-q.slots
				q.Wake()
			}()
			q.attempt(ctx, row)
		}(row)
	}
}

// attempt makes one delivery attempt for row and records what happened.
func (q *Queue) attempt(ctx context.Context, row storage.OutboundRow) {
	if q.blobs == nil {
		log.Printf("[SMTP Queue] No message store configured, %s -> %s stays queued", row.Sender, row.Recipient)
		return
	}
	body, err := q.blobs.Get(row.RawRef)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			q.fail(row, &permanentError{status: "5.3.0", msg: "message content is missing from the store"})
			return
		}
		err = fmt.Errorf("message read failed: %w", err)
	} else {
		err = q.sender.Send(ctx, row.Sender, row.Recipient, body)
	}

	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueBookkeeping)
	defer cancel()

	switch {
	case err == nil:
		if derr := q.store.OutboundDelete(done, row.ID); derr != nil {
			log.Printf("[SMTP Queue] Delivered %s -> %s but could not dequeue it: %v", row.Sender, row.Recipient, derr)
		}
	case ctx.Err() != nil:
		// Shutdown, not a failure: hand the row back untouched.
		if rerr := q.store.OutboundRetry(done, row.ID, row.Attempts, q.now(), row.LastError); rerr != nil {
			log.Printf("[SMTP Queue] Could not release %s -> %s: %v", row.Sender, row.Recipient, rerr)
		}
	case IsPermanent(err):
		q.fail(row, err)
	case q.now().Sub(row.Created) >= q.lifetime:
		q.fail(row, &permanentError{status: "4.4.7",
			msg: fmt.Sprintf("delivery time expired after %d attempts, last error: %v", row.Attempts+1, err)})
	default:
		attempts := row.Attempts + 1
		next := q.now().Add(queueBackoff[min(attempts, len(queueBackoff))-1])
		log.Printf("[SMTP Queue] Deferred %s -> %s (attempt %d, next %s): %v",
			row.Sender, row.Recipient, attempts, next.Format(time.RFC3339), err)
		if rerr := q.store.OutboundRetry(done, row.ID, attempts, next, err.Error()); rerr != nil {
			log.Printf("[SMTP Queue] Could not reschedule %s -> %s: %v", row.Sender, row.Recipient, rerr)
		}
	}
}

// fail gives up on row: the sender gets a bounce, then the row is removed.
func (q *Queue) fail(row storage.OutboundRow, cause error) {
	log.Printf("[SMTP Queue] Failed %s -> %s: %v", row.Sender, row.Recipient, cause)
	ctx, cancel := context.WithTimeout(context.Background(), queueBookkeeping)
	defer cancel()

	if err := q.bounce(ctx, row, cause); err != nil {
		log.Printf("[SMTP Queue] Bounce to %s not stored: %v", row.Sender, err)
	}
	if err := q.store.OutboundDelete(ctx, row.ID); err != nil {
		log.Printf("[SMTP Queue] Could not dequeue %s -> %s: %v", row.Sender, row.Recipient, err)
	}
}

// bounce stores a delivery status notification in the sender's INBOX. Every
// queued sender is ours, so the report never leaves the server; a sender with
// no mailbox (an app sending as noreply@) has nowhere to read one.
func (q *Queue) bounce(ctx context.Context, row storage.OutboundRow, cause error) error {
	account, err := q.store.AccountExists(ctx, row.Sender)
	if err != nil {
		return err
	}
	if account == nil {
		log.Printf("[SMTP Queue] No mailbox for %s, bounce dropped", row.Sender)
		return nil
	}

	var original []byte
	if q.blobs != nil {
		original, _ = q.blobs.Get(row.RawRef)
	}
	dsn := buildDSN(dsnReport{
		arrival:    row.Created,
		diagnostic: strings.ReplaceAll(cause.Error(), "\n", " "),
		now:        q.now(),
		original:   original,
		recipient:  row.Recipient,
		sender:     account.Email,
		status:     failureStatus(cause),
	})

	ref := ""
	if q.blobs != nil {
		if ref, err = q.blobs.Put(dsn); err != nil {
			log.Printf("[SMTP Queue] Bounce raw store failed for %s: %v", row.Sender, err)
			ref = ""
		}
	}
	msg := &storage.MessageRow{
		Email:   account.Email,
		Flags:   toNullString("[]"),
		Mailbox: "INBOX",
		RawRef:  toNullString(ref),
	}
	message.Parse(dsn).Apply(msg)
	return q.store.Deliver(ctx, []*storage.MessageRow{msg}, nil)
}
