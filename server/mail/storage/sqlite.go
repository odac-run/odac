package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides thread-safe SQLite access for the mail server.
// Uses WAL mode for concurrent read/write performance and connection
// pooling via database/sql. Backward-compatible with the existing
// Node.js SQLite database at ~/.odac/db/mail.
type Store struct {
	db   *sql.DB
	mu   sync.RWMutex
	path string
}

// NewStore creates a new Store and opens the SQLite database.
// Automatically creates the database directory and runs migrations.
// The database path defaults to ~/.odac/db/mail if not specified.
func NewStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
		dbPath = filepath.Join(home, ".odac", "db", "mail")
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("cannot create database directory: %w", err)
	}

	// Pure Go SQLite driver via modernc.org/sqlite
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("cannot open database: %w", err)
	}

	// Connection pool tuning for mail workload
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	s := &Store{db: db, path: dbPath}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	log.Printf("[Mail-DB] Database opened: %s (WAL mode)", dbPath)
	return s, nil
}

// migrate runs all schema migrations in a single transaction.
func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range migrations {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement failed: %w\nSQL: %s", err, stmt)
		}
	}

	return tx.Commit()
}

// Close gracefully shuts down the database connection pool.
func (s *Store) Close() error {
	if s.db != nil {
		log.Println("[Mail-DB] Closing database connection")
		return s.db.Close()
	}
	return nil
}

// --- Account Operations ---

// AccountExists checks if a mail account exists and returns its data.
// Returns nil if the account does not exist.
func (s *Store) AccountExists(ctx context.Context, email string) (*AccountRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		"SELECT id, email, password, domain FROM mail_account WHERE email = ?", email)

	var a AccountRow
	err := row.Scan(&a.ID, &a.Email, &a.Password, &a.Domain)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("account lookup failed: %w", err)
	}
	return &a, nil
}

// AccountCreate inserts a new mail account with a pre-hashed password.
func (s *Store) AccountCreate(ctx context.Context, email, hashedPassword, domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"INSERT INTO mail_account (email, password, domain) VALUES (?, ?, ?)",
		email, hashedPassword, domain)
	if err != nil {
		return fmt.Errorf("account creation failed: %w", err)
	}
	return nil
}

// AccountDelete removes a mail account by email address.
func (s *Store) AccountDelete(ctx context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"DELETE FROM mail_account WHERE email = ?", email)
	if err != nil {
		return fmt.Errorf("account deletion failed: %w", err)
	}
	return nil
}

// AccountUpdatePassword updates the password for an existing account.
func (s *Store) AccountUpdatePassword(ctx context.Context, email, hashedPassword string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"UPDATE mail_account SET password = ? WHERE email = ?",
		hashedPassword, email)
	if err != nil {
		return fmt.Errorf("password update failed: %w", err)
	}
	return nil
}

// AccountList returns all email addresses for a given domain.
func (s *Store) AccountList(ctx context.Context, domain string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		"SELECT email FROM mail_account WHERE domain = ?", domain)
	if err != nil {
		return nil, fmt.Errorf("account list query failed: %w", err)
	}
	defer rows.Close()

	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		emails = append(emails, email)
	}
	return emails, rows.Err()
}

// AccountRow represents a row from the mail_account table.
type AccountRow struct {
	Domain   string
	Email    string
	ID       int64
	Password string
}

// --- Message Operations ---

// MessageStore inserts a new email message into the mail_received table.
// Automatically assigns the next UID for the given email account.
func (s *Store) MessageStore(ctx context.Context, msg *MessageRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get next UID for this email account
	var nextUID int64
	err = tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(uid), 0) + 1 FROM mail_received WHERE email = ?",
		msg.Email).Scan(&nextUID)
	if err != nil {
		return fmt.Errorf("UID query failed: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO mail_received
			(uid, email, mailbox, attachments, headers, headerLines,
			 html, text, textAsHtml, subject, "to", "from", messageId, flags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nextUID, msg.Email, msg.Mailbox, msg.Attachments, msg.Headers,
		msg.HeaderLines, msg.HTML, msg.Text, msg.TextAsHTML, msg.Subject,
		msg.To, msg.From, msg.MessageID, msg.Flags)
	if err != nil {
		return fmt.Errorf("message insert failed: %w", err)
	}

	return tx.Commit()
}

// MessageFetch retrieves messages for a given email and mailbox with optional UID range.
func (s *Store) MessageFetch(ctx context.Context, email, mailbox string, uidMin, uidMax int64) ([]MessageRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, uid, email, mailbox, flags, attachments, headers,
		headerLines, html, text, textAsHtml, subject, date, "to", "from", messageId
		FROM mail_received WHERE email = ? AND mailbox = ?`
	args := []any{email, mailbox}

	if uidMin > 0 {
		query += " AND uid >= ?"
		args = append(args, uidMin)
	}
	if uidMax > 0 {
		query += " AND uid <= ?"
		args = append(args, uidMax)
	}
	query += " ORDER BY id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("message fetch failed: %w", err)
	}
	defer rows.Close()

	var messages []MessageRow
	for rows.Next() {
		var m MessageRow
		err := rows.Scan(&m.ID, &m.UID, &m.Email, &m.Mailbox, &m.Flags,
			&m.Attachments, &m.Headers, &m.HeaderLines, &m.HTML, &m.Text,
			&m.TextAsHTML, &m.Subject, &m.Date, &m.To, &m.From, &m.MessageID)
		if err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// MessageExpunge deletes messages marked with the 'deleted' flag.
// Returns the UIDs of deleted messages.
func (s *Store) MessageExpunge(ctx context.Context, email, mailbox string) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT uid FROM mail_received WHERE email = ? AND mailbox = ? AND flags LIKE '%deleted%'`,
		email, mailbox)
	if err != nil {
		return nil, fmt.Errorf("expunge query failed: %w", err)
	}

	var uids []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		uids = append(uids, uid)
	}
	rows.Close()

	if len(uids) > 0 {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM mail_received WHERE email = ? AND mailbox = ? AND flags LIKE '%deleted%'`,
			email, mailbox)
		if err != nil {
			return nil, fmt.Errorf("expunge delete failed: %w", err)
		}
	}

	return uids, tx.Commit()
}

// MailboxSelect returns mailbox statistics for IMAP SELECT command.
func (s *Store) MailboxSelect(ctx context.Context, email, mailbox string) (*MailboxStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) AS "exists",
			COALESCE(SUM(IIF(flags LIKE '%seen%', 0, 1)), 0) AS unseen,
			COALESCE(MAX(uid) + 1, 1) AS uidnext,
			COALESCE(MAX(uid), 0) AS uidvalidity
		FROM mail_received WHERE email = ? AND mailbox = ?`,
		email, mailbox)

	var stats MailboxStats
	err := row.Scan(&stats.Exists, &stats.Unseen, &stats.UIDNext, &stats.UIDValidity)
	if err != nil {
		return nil, fmt.Errorf("mailbox select failed: %w", err)
	}
	return &stats, nil
}

// MailboxList returns all mailbox names for an account, always including INBOX.
func (s *Store) MailboxList(ctx context.Context, email string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		"SELECT title FROM mail_box WHERE email = ?", email)
	if err != nil {
		return nil, fmt.Errorf("mailbox list query failed: %w", err)
	}
	defer rows.Close()

	boxes := []string{"INBOX"}
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		if title != "INBOX" {
			boxes = append(boxes, title)
		}
	}
	return boxes, rows.Err()
}

// MailboxCreate creates a new mailbox for an account.
func (s *Store) MailboxCreate(ctx context.Context, email, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"INSERT INTO mail_box (email, title) VALUES (?, ?)", email, title)
	if err != nil {
		return fmt.Errorf("mailbox creation failed: %w", err)
	}
	return nil
}

// MailboxDelete removes a mailbox for an account.
func (s *Store) MailboxDelete(ctx context.Context, email, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"DELETE FROM mail_box WHERE email = ? AND title = ?", email, title)
	if err != nil {
		return fmt.Errorf("mailbox deletion failed: %w", err)
	}
	return nil
}

// MailboxRename renames a mailbox for an account.
func (s *Store) MailboxRename(ctx context.Context, email, oldTitle, newTitle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		"UPDATE mail_box SET title = ? WHERE email = ? AND title = ?",
		newTitle, email, oldTitle)
	if err != nil {
		return fmt.Errorf("mailbox rename failed: %w", err)
	}
	return nil
}

// MessageStoreFlags updates flags on messages matching the given UID range.
func (s *Store) MessageStoreFlags(ctx context.Context, email string, uidMin, uidMax int64, action string, flags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, flag := range flags {
		switch action {
		case "add":
			_, err := s.db.ExecContext(ctx,
				`UPDATE mail_received
				SET flags = JSON_INSERT(flags, '$[#]', ?)
				WHERE email = ? AND uid BETWEEN ? AND ? AND flags NOT LIKE ?`,
				flag, email, uidMin, uidMax, "%"+flag+"%")
			if err != nil {
				return fmt.Errorf("flag add failed: %w", err)
			}
		case "remove":
			_, err := s.db.ExecContext(ctx,
				`UPDATE mail_received
				SET flags = JSON_REMOVE(flags, (SELECT value FROM JSON_EACH(flags) WHERE value = ?))
				WHERE email = ? AND uid BETWEEN ? AND ? AND flags LIKE ?`,
				flag, email, uidMin, uidMax, "%"+flag+"%")
			if err != nil {
				return fmt.Errorf("flag remove failed: %w", err)
			}
		case "set":
			flagsJSON := "["
			for i, f := range flags {
				if i > 0 {
					flagsJSON += ","
				}
				flagsJSON += `"` + f + `"`
			}
			flagsJSON += "]"
			_, err := s.db.ExecContext(ctx,
				`UPDATE mail_received SET flags = ? WHERE email = ? AND uid BETWEEN ? AND ?`,
				flagsJSON, email, uidMin, uidMax)
			if err != nil {
				return fmt.Errorf("flag set failed: %w", err)
			}
			return nil // set replaces all flags at once, no per-flag iteration needed
		}
	}
	return nil
}

// MessageRow represents a row from the mail_received table.
type MessageRow struct {
	Attachments sql.NullString
	Date        sql.NullString
	Email       string
	Flags       sql.NullString
	From        sql.NullString
	HTML        sql.NullString
	HeaderLines sql.NullString
	Headers     sql.NullString
	ID          int64
	Mailbox     string
	MessageID   sql.NullString
	Subject     sql.NullString
	Text        sql.NullString
	TextAsHTML  sql.NullString
	To          sql.NullString
	UID         int64
}

// MailboxStats holds the result of a mailbox SELECT query.
type MailboxStats struct {
	Exists      int64
	UIDNext     int64
	UIDValidity int64
	Unseen      int64
}
