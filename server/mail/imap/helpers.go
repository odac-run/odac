package imap

import (
	"database/sql"
	"strconv"
	"strings"
)

// toNullString creates a sql.NullString from a string value.
func toNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
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

	var lo, hi int64
	if strings.Contains(seqSet, ":") {
		parts := strings.SplitN(seqSet, ":", 2)
		lo = parseNum(parts[0], isUID)
		hi = parseNum(parts[1], isUID)
		if lo > hi {
			lo, hi = hi, lo
		}
	} else {
		lo = parseNum(seqSet, isUID)
		hi = lo
	}

	var out []int64
	for i, uid := range allUIDs {
		if isUID {
			if uid >= lo && uid <= hi {
				out = append(out, uid)
			}
		} else {
			seq := int64(i + 1)
			if seq >= lo && seq <= hi {
				out = append(out, uid)
			}
		}
	}
	return out
}
