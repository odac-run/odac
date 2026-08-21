package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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

// defaultMailboxes are provisioned automatically when a new account is created.
// INBOX is implicit and always returned by MailboxList, so it's not included here.
var defaultMailboxes = []string{"Drafts", "Junk", "Sent", "Trash"}

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

	for _, stmt := range tables {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("table migration failed: %w\nSQL: %s", err, stmt)
		}
	}

	// Columns come after the tables exist and before any index that may
	// reference them.
	for _, c := range addedColumns {
		has, err := columnExists(ctx, tx, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("column migration failed: %w\nSQL: %s", err, stmt)
		}
		log.Printf("[Mail-DB] Schema upgraded: %s.%s added", c.table, c.column)
	}

	for _, stmt := range indexes {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("index migration failed: %w\nSQL: %s", err, stmt)
		}
	}

	// Data repair runs last, once the schema it reads is guaranteed to exist.
	if err := repairFlags(ctx, tx); err != nil {
		return err
	}

	return tx.Commit()
}

// columnExists reports whether a table already has the named column.
func columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("table_info(%s) failed: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return false, fmt.Errorf("table_info scan failed: %w", err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
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
// Automatically provisions default mailboxes (Drafts, Junk, Sent, Trash).
func (s *Store) AccountCreate(ctx context.Context, email, hashedPassword, domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		"INSERT INTO mail_account (email, password, domain) VALUES (?, ?, ?)",
		email, hashedPassword, domain)
	if err != nil {
		return fmt.Errorf("account creation failed: %w", err)
	}

	for _, box := range defaultMailboxes {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO mail_box (email, title) VALUES (?, ?)", email, box)
		if err != nil {
			return fmt.Errorf("default mailbox creation failed: %w", err)
		}
	}

	return tx.Commit()
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

// AccountEntry is one account as the listing endpoints expose it.
type AccountEntry struct {
	Domain string `json:"domain"`
	Email  string `json:"email"`
}

// AccountList returns all accounts of a given domain.
func (s *Store) AccountList(ctx context.Context, domain string) ([]AccountEntry, error) {
	return s.accountQuery(ctx,
		"SELECT domain, email FROM mail_account WHERE domain = ? ORDER BY email", domain)
}

// AccountListAll returns every account across all domains, sorted by domain
// then address.
func (s *Store) AccountListAll(ctx context.Context) ([]AccountEntry, error) {
	return s.accountQuery(ctx,
		"SELECT domain, email FROM mail_account ORDER BY domain, email")
}

func (s *Store) accountQuery(ctx context.Context, query string, args ...any) ([]AccountEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("account list query failed: %w", err)
	}
	defer rows.Close()

	accounts := []AccountEntry{}
	for rows.Next() {
		var entry AccountEntry
		if err := rows.Scan(&entry.Domain, &entry.Email); err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		accounts = append(accounts, entry)
	}
	return accounts, rows.Err()
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

	// Auto-create mailbox if it doesn't exist (INBOX is implicit, skip it)
	if msg.Mailbox != "INBOX" {
		_, err = tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO mail_box (email, title) VALUES (?, ?)",
			msg.Email, msg.Mailbox)
		if err != nil {
			return fmt.Errorf("mailbox auto-create failed: %w", err)
		}
	}

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
			 html, text, textAsHtml, subject, "to", "from", messageId, flags, rawRef)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nextUID, msg.Email, msg.Mailbox, msg.Attachments, msg.Headers,
		msg.HeaderLines, msg.HTML, msg.Text, msg.TextAsHTML, msg.Subject,
		msg.To, msg.From, msg.MessageID, msg.Flags, msg.RawRef)
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
		headerLines, html, text, textAsHtml, subject, date, "to", "from", messageId, rawRef
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
	query += " ORDER BY uid ASC"

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
			&m.TextAsHTML, &m.Subject, &m.Date, &m.To, &m.From, &m.MessageID,
			&m.RawRef)
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
		`SELECT uid FROM mail_received WHERE email = ? AND mailbox = ?
		AND EXISTS (SELECT 1 FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value = 'deleted')`,
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
			`DELETE FROM mail_received WHERE email = ? AND mailbox = ?
			AND EXISTS (SELECT 1 FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value = 'deleted')`,
			email, mailbox)
		if err != nil {
			return nil, fmt.Errorf("expunge delete failed: %w", err)
		}
	}

	return uids, tx.Commit()
}

// MailboxSelect returns mailbox statistics for IMAP SELECT command.
//
// UIDNEXT is computed across all mailboxes for the account because UIDs in
// this schema are assigned globally per-account (see MessageStore), not
// per-mailbox. UIDVALIDITY is derived from the account creation timestamp so
// that it stays stable for the account's lifetime — RFC 3501 §2.3.1.1
// requires UIDVALIDITY to change only when UIDs are invalidated, otherwise
// strict clients (Apple Mail) treat the mailbox as unstable and refuse to
// sync new mail.
func (s *Store) MailboxSelect(ctx context.Context, email, mailbox string) (*MailboxStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		`SELECT
			(SELECT COUNT(*) FROM mail_received WHERE email = ? AND mailbox = ?),
			COALESCE((SELECT SUM(CASE WHEN EXISTS (SELECT 1 FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value = 'seen') THEN 0 ELSE 1 END) FROM mail_received WHERE email = ? AND mailbox = ?), 0),
			COALESCE((SELECT MAX(uid) + 1 FROM mail_received WHERE email = ?), 1),
			COALESCE(CAST(strftime('%s', (SELECT created FROM mail_account WHERE email = ?)) AS INTEGER), 1)`,
		email, mailbox, email, mailbox, email, email)

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

// maxUIDsPerStatement bounds how many UIDs are bound into one IN (?,?,...)
// clause. SQLite rejects any statement carrying more than
// SQLITE_MAX_VARIABLE_NUMBER (32766) bound parameters, so a mailbox-wide
// "STORE 1:* +FLAGS (\Seen)" has to be applied in batches rather than failing
// outright once the mailbox grows past that many messages.
const maxUIDsPerStatement = 512

// MessageStoreFlags updates flags on messages matching the given UIDs.
// action is "add", "remove" or "set"; "set" with an empty flag list clears all
// flags, which is how clients express STORE FLAGS (). All batches run inside a
// single transaction so a partially applied update is never observable.
func (s *Store) MessageStoreFlags(ctx context.Context, email string, uids []int64, action string, flags []string) error {
	if len(uids) == 0 {
		return nil
	}
	if action != "add" && action != "remove" && action != "set" {
		return fmt.Errorf("unknown flag action %q", action)
	}
	if action != "set" && len(flags) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	defer tx.Rollback()

	for start := 0; start < len(uids); start += maxUIDsPerStatement {
		end := min(start+maxUIDsPerStatement, len(uids))
		if err := storeFlagsBatch(ctx, tx, email, uids[start:end], action, flags); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("flag update commit failed: %w", err)
	}
	return nil
}

// storeFlagsBatch applies one action to a batch of UIDs small enough to fit in
// a single statement's parameter budget.
func storeFlagsBatch(ctx context.Context, tx *sql.Tx, email string, uids []int64, action string, flags []string) error {
	// Build a parameterized IN clause (?,?,...) with the UIDs as bound args so no
	// user/data-derived value is ever concatenated into the SQL text.
	placeholders := make([]string, len(uids))
	uidArgs := make([]any, len(uids))
	for i, uid := range uids {
		placeholders[i] = "?"
		uidArgs[i] = uid
	}
	inClause := strings.Join(placeholders, ",")

	if action == "set" {
		// Marshal flags into a JSON array so values containing quotes/backslashes
		// can't produce malformed JSON. A nil slice must still encode as [].
		list := flags
		if list == nil {
			list = []string{}
		}
		flagsJSON, err := json.Marshal(list)
		if err != nil {
			return fmt.Errorf("flag set failed: %w", err)
		}
		query := fmt.Sprintf(`UPDATE mail_received SET flags = ? WHERE email = ? AND uid IN (%s)`, inClause)
		args := append([]any{string(flagsJSON), email}, uidArgs...)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("flag set failed: %w", err)
		}
		return nil
	}

	for _, flag := range flags {
		var query string
		if action == "add" {
			query = fmt.Sprintf(`UPDATE mail_received
				SET flags = JSON_INSERT(`+safeFlagsExpr+`, '$[#]', ?)
				WHERE email = ? AND uid IN (%s)
				AND NOT EXISTS (SELECT 1 FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value = ?)`, inClause)
		} else {
			query = fmt.Sprintf(`UPDATE mail_received
				SET flags = (SELECT JSON_GROUP_ARRAY(value) FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value != ?)
				WHERE email = ? AND uid IN (%s)
				AND EXISTS (SELECT 1 FROM JSON_EACH(`+safeFlagsExpr+`) WHERE value = ?)`, inClause)
		}
		args := append([]any{flag, email}, uidArgs...)
		args = append(args, flag)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("flag %s failed: %w", action, err)
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
	RawRef      sql.NullString
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

// MessageCopy copies messages from one mailbox to another by UID range.
func (s *Store) MessageCopy(ctx context.Context, email string, uidMin, uidMax int64, sourceMailbox, targetMailbox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get next UID for target
	var nextUID int64
	err = tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(uid), 0) + 1 FROM mail_received WHERE email = ?",
		email).Scan(&nextUID)
	if err != nil {
		return fmt.Errorf("UID query failed: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT email, flags, attachments, headers, headerLines, html, text,
			textAsHtml, subject, "to", "from", messageId, rawRef
		FROM mail_received WHERE email = ? AND mailbox = ? AND uid BETWEEN ? AND ?`,
		email, sourceMailbox, uidMin, uidMax)
	if err != nil {
		return fmt.Errorf("copy source query failed: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m MessageRow
		err := rows.Scan(&m.Email, &m.Flags, &m.Attachments, &m.Headers,
			&m.HeaderLines, &m.HTML, &m.Text, &m.TextAsHTML, &m.Subject,
			&m.To, &m.From, &m.MessageID, &m.RawRef)
		if err != nil {
			return fmt.Errorf("row scan failed: %w", err)
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO mail_received
				(uid, email, mailbox, attachments, headers, headerLines,
				 html, text, textAsHtml, subject, "to", "from", messageId, flags, rawRef)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nextUID, m.Email, targetMailbox, m.Attachments, m.Headers,
			m.HeaderLines, m.HTML, m.Text, m.TextAsHTML, m.Subject,
			m.To, m.From, m.MessageID, m.Flags, m.RawRef)
		if err != nil {
			return fmt.Errorf("copy insert failed: %w", err)
		}
		nextUID++
	}

	return tx.Commit()
}

// MessageFlagRow is the minimal projection a flag-based search needs.
type MessageFlagRow struct {
	Flags sql.NullString
	UID   int64
}

// MessageFlags returns the UID and flags of every message in a mailbox.
//
// SEARCH used to run through MessageFetch, which selects every column: each
// search pulled the full body of every message in the mailbox into memory to
// decide which UIDs matched a flag. Clients issue SEARCH constantly, so the
// projection matters more here than anywhere else in the IMAP path.
func (s *Store) MessageFlags(ctx context.Context, email, mailbox string) ([]MessageFlagRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT uid, flags FROM mail_received
		WHERE email = ? AND mailbox = ? ORDER BY uid ASC`, email, mailbox)
	if err != nil {
		return nil, fmt.Errorf("flag fetch failed: %w", err)
	}
	defer rows.Close()

	var out []MessageFlagRow
	for rows.Next() {
		var m MessageFlagRow
		if err := rows.Scan(&m.UID, &m.Flags); err != nil {
			return nil, fmt.Errorf("flag row scan failed: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RawRefExists reports whether any stored message still references a raw
// message blob. The blob sweeper asks per candidate rather than loading every
// live reference into memory, which keeps its footprint flat as the mail store
// grows; idx_received_rawref makes each lookup an index probe.
func (s *Store) RawRefExists(ctx context.Context, ref string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM mail_received WHERE rawRef = ? LIMIT 1", ref).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rawRef lookup failed: %w", err)
	}
	return true, nil
}

// MessageUIDs returns all UIDs for a given email and mailbox in ASC order.
// Used for computing IMAP sequence numbers without loading full message bodies.
func (s *Store) MessageUIDs(ctx context.Context, email, mailbox string) ([]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		"SELECT uid FROM mail_received WHERE email = ? AND mailbox = ? ORDER BY uid ASC",
		email, mailbox)
	if err != nil {
		return nil, fmt.Errorf("UID list query failed: %w", err)
	}
	defer rows.Close()

	var uids []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("row scan failed: %w", err)
		}
		uids = append(uids, uid)
	}
	return uids, rows.Err()
}
