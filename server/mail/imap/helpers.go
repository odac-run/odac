package imap

import "database/sql"

// toNullString creates a sql.NullString from a string value.
func toNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
