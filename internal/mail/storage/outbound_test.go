package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Two odac-mail processes drain the queue side by side during an update. A
// claimed row must stay with its claimant until the lease runs out, or both
// deliver it.
func TestOutboundClaimLeasesRows(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	row := OutboundRow{Sender: "a@x.test", Recipient: "b@y.test", RawRef: "ref", Created: now}
	if err := s.Deliver(ctx, nil, []OutboundRow{row}); err != nil {
		t.Fatal(err)
	}

	first, err := s.OutboundClaim(ctx, now, time.Minute, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %v, %v", first, err)
	}
	if second, _ := s.OutboundClaim(ctx, now.Add(30*time.Second), time.Minute, 10); len(second) != 0 {
		t.Fatalf("a leased row was claimed again: %+v", second)
	}
	if late, _ := s.OutboundClaim(ctx, now.Add(2*time.Minute), time.Minute, 10); len(late) != 1 {
		t.Fatalf("an expired lease was not reclaimable: %+v", late)
	}

	got := first[0]
	if got.Sender != row.Sender || got.Recipient != row.Recipient || got.RawRef != row.RawRef || !got.Created.Equal(now) {
		t.Errorf("claimed %+v, want the stored row", got)
	}

	if err := s.OutboundRetry(ctx, got.ID, 3, now.Add(time.Hour), "451 later"); err != nil {
		t.Fatal(err)
	}
	if early, _ := s.OutboundClaim(ctx, now.Add(59*time.Minute), time.Minute, 10); len(early) != 0 {
		t.Fatalf("a rescheduled row was claimed before it was due: %+v", early)
	}
	due, _ := s.OutboundClaim(ctx, now.Add(time.Hour), time.Minute, 10)
	if len(due) != 1 || due[0].Attempts != 3 || due[0].LastError != "451 later" {
		t.Fatalf("rescheduled row = %+v", due)
	}

	if err := s.OutboundDelete(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.OutboundClaim(ctx, now.Add(48*time.Hour), time.Minute, 10); len(left) != 0 {
		t.Fatalf("deleted row still queued: %+v", left)
	}
}

// Every row of a transaction commits or none does, queue rows included.
func TestDeliverIsAtomicAcrossMailboxAndQueue(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	msg := &MessageRow{Email: "a@x.test", Mailbox: "Sent"}
	bad := OutboundRow{Sender: "a@x.test", Recipient: "b@y.test"} // no message: refused
	if err := s.Deliver(ctx, []*MessageRow{msg}, []OutboundRow{bad}); err == nil {
		t.Fatal("Deliver accepted a queue row with no message")
	}
	if uids, _ := s.MessageUIDs(ctx, "a@x.test", "Sent"); len(uids) != 0 {
		t.Errorf("the Sent copy committed without its queue row (%d rows)", len(uids))
	}
}
