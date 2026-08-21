package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// safeFlagsExpr yields the flags column, substituting an empty array whenever
// the stored value is not valid JSON.
//
// Every reader of the column goes through JSON_EACH, which aborts the entire
// statement with "malformed JSON" rather than skipping the offending row. One
// corrupt value would therefore fail SELECT, EXPUNGE and STORE for the whole
// mailbox, which is what a client sees as "SELECT failed". Wrapping the input
// keeps a single bad row from taking the mailbox down with it.
const safeFlagsExpr = `CASE WHEN JSON_VALID(flags) THEN flags ELSE '[]' END`

// CanonicalFlags converts IMAP flag tokens (`\Seen`, `\Draft`) into the form
// stored in mail_received.flags: lowercase, without the leading backslash.
// Duplicates are dropped so a client repeating a flag cannot skew counts.
func CanonicalFlags(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), `\`))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// EncodeFlags renders canonical flag names as the JSON array stored in
// mail_received.flags. The value always comes from the JSON encoder, never
// from string concatenation: a flag name carrying a quote or a backslash would
// otherwise produce a value that breaks every later read of the mailbox.
func EncodeFlags(flags []string) string {
	if len(flags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(flags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// repairFlags rewrites rows whose flags value is not valid JSON.
//
// APPEND used to build the column by concatenating the raw IMAP flag list into
// brackets, so a draft saved with `(\Seen \Draft)` was stored as `[\Seen`, and
// from then on every query over that mailbox failed. The flag names are still
// readable in the broken value, so they are salvaged rather than discarded.
func repairFlags(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, flags FROM mail_received WHERE flags IS NOT NULL AND NOT JSON_VALID(flags)`)
	if err != nil {
		return fmt.Errorf("flag repair query failed: %w", err)
	}

	type repair struct {
		id    int64
		flags string
	}
	var pending []repair
	for rows.Next() {
		var id int64
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("flag repair scan failed: %w", err)
		}
		pending = append(pending, repair{
			id:    id,
			flags: EncodeFlags(CanonicalFlags(strings.Fields(strings.Trim(raw.String, "[]")))),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("flag repair query failed: %w", err)
	}

	for _, r := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mail_received SET flags = ? WHERE id = ?`, r.flags, r.id); err != nil {
			return fmt.Errorf("flag repair update failed: %w", err)
		}
	}
	if len(pending) > 0 {
		log.Printf("[Mail-DB] Repaired malformed flags on %d message(s)", len(pending))
	}
	return nil
}
