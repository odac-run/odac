package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OutboundRow is one remote recipient waiting in the delivery queue. The
// message lives in the blob store under RawRef, so a message to several
// recipients is stored once.
type OutboundRow struct {
	Attempts    int
	Created     time.Time
	ID          int64
	LastError   string
	NextAttempt time.Time
	RawRef      string
	Recipient   string
	Sender      string
}

func insertOutbound(ctx context.Context, tx *sql.Tx, row OutboundRow) error {
	if row.RawRef == "" {
		return fmt.Errorf("outbound row for %s has no message", row.Recipient)
	}
	created := row.Created
	if created.IsZero() {
		created = time.Now()
	}
	next := row.NextAttempt
	if next.IsZero() {
		next = created
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO mail_outbound (sender, recipient, rawRef, attempts, created, nextAttempt)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.Sender, row.Recipient, row.RawRef, row.Attempts, created.Unix(), next.Unix())
	if err != nil {
		return fmt.Errorf("outbound insert failed: %w", err)
	}
	return nil
}

// OutboundClaim leases up to limit due rows to the caller until now+lease.
//
// The lease lives in the database, not in memory, because two odac-mail
// processes run side by side during an update and both drain the queue: the
// single UPDATE is what keeps them from delivering one row twice. A process
// that dies mid-delivery leaves its rows to be claimed again once the lease
// runs out.
func (s *Store) OutboundClaim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboundRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`UPDATE mail_outbound SET lockedUntil = ?
		WHERE id IN (
			SELECT id FROM mail_outbound
			WHERE nextAttempt <= ? AND lockedUntil <= ?
			ORDER BY nextAttempt, id LIMIT ?)
		RETURNING id, sender, recipient, rawRef, attempts, created, nextAttempt, lastError`,
		now.Add(lease).Unix(), now.Unix(), now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("outbound claim failed: %w", err)
	}
	defer rows.Close()

	var out []OutboundRow
	for rows.Next() {
		var (
			row           OutboundRow
			created, next int64
			lastErr       sql.NullString
		)
		if err := rows.Scan(&row.ID, &row.Sender, &row.Recipient, &row.RawRef,
			&row.Attempts, &created, &next, &lastErr); err != nil {
			return nil, fmt.Errorf("outbound scan failed: %w", err)
		}
		row.Created = time.Unix(created, 0)
		row.NextAttempt = time.Unix(next, 0)
		row.LastError = lastErr.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// OutboundDelete removes a row once it was delivered or given up on.
func (s *Store) OutboundDelete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx, "DELETE FROM mail_outbound WHERE id = ?", id); err != nil {
		return fmt.Errorf("outbound delete failed: %w", err)
	}
	return nil
}

// OutboundRetry releases a row's lease and schedules its next attempt.
func (s *Store) OutboundRetry(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE mail_outbound SET attempts = ?, nextAttempt = ?, lockedUntil = 0, lastError = ?
		WHERE id = ?`,
		attempts, next.Unix(), lastErr, id)
	if err != nil {
		return fmt.Errorf("outbound reschedule failed: %w", err)
	}
	return nil
}
