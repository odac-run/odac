package imap

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxMailboxNameLen bounds mailbox names so a client cannot fill the store
// with names that no longer fit in a single response line.
const maxMailboxNameLen = 512

// toNullString creates a sql.NullString from a string value.
func toNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// quoteString encodes s as an RFC 3501 quoted-string. Every value that reaches
// the wire inside quotes must go through here: an unescaped `"` or `\` in a
// mailbox name would otherwise close the token early and desynchronise the
// client's parser. Control characters are illegal in quoted-strings and are
// dropped rather than emitted raw.
func quoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case isControlRune(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// validMailboxName reports whether s is acceptable as a mailbox name. Control
// characters are rejected at the input boundary so they can never be stored
// and later echoed back in LIST/STATUS responses.
func validMailboxName(s string) bool {
	if s == "" || len(s) > maxMailboxNameLen {
		return false
	}
	return !hasControlChars(s) && utf8.ValidString(s)
}

// hasControlChars reports whether s contains characters that must never reach
// a protocol line or a log record (CR/LF injection, terminal escapes).
func hasControlChars(s string) bool {
	for _, r := range s {
		if isControlRune(r) {
			return true
		}
	}
	return false
}

func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// seqSetToUIDs resolves a sequence-set string to a list of UIDs from allUIDs.
// When isUID is true the set is interpreted as UID values; when false it is
// interpreted as 1-based sequence positions.  "*" is treated as the last
// position/UID in allUIDs.
func seqSetToUIDs(seqSet string, allUIDs []int64, isUID bool) []int64 {
	if len(allUIDs) == 0 {
		return nil
	}
	lastUID := allUIDs[len(allUIDs)-1]
	lastSeq := int64(len(allUIDs))

	parseNum := func(s string, isUIDCtx bool) int64 {
		if s == "*" {
			if isUIDCtx {
				return lastUID
			}
			return lastSeq
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}

	var out []int64
	parts := strings.Split(seqSet, ",")
	for _, part := range parts {
		var lo, hi int64
		if strings.Contains(part, ":") {
			bounds := strings.SplitN(part, ":", 2)
			lo = parseNum(bounds[0], isUID)
			hi = parseNum(bounds[1], isUID)
			if lo > hi {
				lo, hi = hi, lo
			}
		} else {
			lo = parseNum(part, isUID)
			hi = lo
		}

		if isUID {
			startIdx := sort.Search(len(allUIDs), func(i int) bool {
				return allUIDs[i] >= lo
			})
			endIdx := sort.Search(len(allUIDs), func(i int) bool {
				return allUIDs[i] > hi
			})
			if startIdx < endIdx {
				out = append(out, allUIDs[startIdx:endIdx]...)
			}
		} else {
			if lo < 1 {
				lo = 1
			}
			if hi > lastSeq {
				hi = lastSeq
			}
			if lo <= hi {
				out = append(out, allUIDs[lo-1:hi]...)
			}
		}
	}
	return out
}
