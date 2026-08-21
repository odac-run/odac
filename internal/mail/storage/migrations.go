// Package storage implements the SQLite persistence layer for the ODAC mail server.
// Schema is backward-compatible with the existing Node.js Mail.js SQLite database
// at ~/.odac/db/mail, enabling zero-downtime migration without data loss.
package storage

// tables defines the schema creation statements. These mirror the exact schema
// from the Node.js Mail.js init() method to ensure full backward compatibility
// with existing databases.
//
// Tables, added columns and indexes are three separate phases run in that
// order by migrate(). An index over a column from addedColumns cannot live in
// this list: against a database that already has the table, CREATE TABLE IF
// NOT EXISTS is a no-op, so the column only appears in the ALTER phase.
var tables = []string{
	// mail_received: Stores all received/sent email messages
	`CREATE TABLE IF NOT EXISTS mail_received (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		uid         INTEGER NOT NULL,
		email       VARCHAR(255) NOT NULL,
		mailbox     VARCHAR(255),
		flags       JSON DEFAULT '[]',
		attachments JSON,
		headers     JSON,
		headerLines JSON,
		html        TEXT,
		text        TEXT,
		textAsHtml  TEXT,
		rawRef      TEXT,
		subject     TEXT,
		date        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		"to"        JSON,
		"from"      JSON,
		messageId   TEXT,
		UNIQUE(email, uid)
	)`,

	// mail_account: Stores mail account credentials
	`CREATE TABLE IF NOT EXISTS mail_account (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		email    VARCHAR(255) UNIQUE,
		password VARCHAR(255),
		domain   VARCHAR(255),
		created  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,

	// mail_box: Stores custom mailbox folders per account
	`CREATE TABLE IF NOT EXISTS mail_box (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		email   VARCHAR(255),
		title   VARCHAR(255),
		parent  INTEGER DEFAULT 0,
		deleted BOOLEAN DEFAULT 0,
		date    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(email, title)
	)`,
}

// indexes are created after addedColumns, so they may reference any column
// added by a later schema revision.
var indexes = []string{
	// Indexes for mail_account
	`CREATE INDEX IF NOT EXISTS idx_account_email  ON mail_account (email)`,
	`CREATE INDEX IF NOT EXISTS idx_account_domain ON mail_account (domain)`,

	// Indexes for mail_received
	`CREATE INDEX IF NOT EXISTS idx_received_uid   ON mail_received (uid)`,
	`CREATE INDEX IF NOT EXISTS idx_received_email ON mail_received (email)`,
	`CREATE INDEX IF NOT EXISTS idx_received_flags ON mail_received (flags)`,
	`CREATE INDEX IF NOT EXISTS idx_received_date  ON mail_received (date)`,

	// Indexes for mail_box
	`CREATE INDEX IF NOT EXISTS idx_box_email ON mail_box (email)`,
	`CREATE INDEX IF NOT EXISTS idx_box_title ON mail_box (title)`,

	// rawRef is probed per candidate by the blob sweeper to find live references.
	`CREATE INDEX IF NOT EXISTS idx_received_rawref ON mail_received (rawRef)`,
}

// addedColumns lists columns introduced after the original Node.js schema.
// SQLite has no ADD COLUMN IF NOT EXISTS, so migrate() consults PRAGMA
// table_info before issuing these against a pre-existing database.
var addedColumns = []struct {
	table  string
	column string
	def    string
}{
	// rawRef addresses the verbatim RFC 5322 message in the blob store.
	// Without it a message is only recoverable as the lossy html/text pair the
	// parser extracted, which is how attachments used to disappear.
	{"mail_received", "rawRef", "TEXT"},
}
