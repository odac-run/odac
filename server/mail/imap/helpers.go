package imap

import (
	"database/sql"
	"sort"
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
