// Package address centralizes RFC 5321 envelope address handling for the mail
// server: validation, canonicalization and local/domain splitting.
//
// It exists because SMTP, IMAP and the control API each grew their own copy of
// "is this an address" and "what is the part after the @", and the copies had
// already drifted apart. Address comparison is a correctness boundary here: a
// mismatch decides whether a message is relayed, delivered or refused.
package address

import "strings"

// maxLength is the RFC 5321 4.5.3.1.3 ceiling on a forward/reverse path.
const maxLength = 254

// Normalize returns the canonical storage form of an address: surrounding
// space removed and the whole address lowercased.
//
// The domain is case-insensitive by RFC 5321 and the local part is, strictly,
// case-sensitive. Preserving that distinction would mean treating <Ali@x.com>
// and <ali@x.com> as two mailboxes, which no operator expects and no mainstream
// server does. Canonicalizing both halves is the deliberate choice; it is
// applied when an address is first written, never to reinterpret stored data.
func Normalize(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// Split returns the local part and domain of an address. Both are empty when
// the address carries no "@".
func Split(addr string) (local, domain string) {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return "", ""
	}
	return addr[:at], addr[at+1:]
}

// Local returns the part before the "@", lowercased so callers can compare it
// against a literal such as "postmaster".
func Local(addr string) string {
	local, _ := Split(addr)
	return strings.ToLower(local)
}

// Domain returns the part after the "@", lowercased for map lookups against
// the configured domain set.
func Domain(addr string) string {
	_, domain := Split(addr)
	return strings.ToLower(domain)
}

// Valid reports whether addr is usable as an envelope path.
//
// The check is deliberately narrow rather than a full RFC 5322 parse: exactly
// one "@", a non-empty local part, a domain carrying a dot, no control
// characters or spaces. Quoted local parts holding an "@" or a space are
// refused as a consequence, which is the trade the stricter of the two
// pre-existing validators already made; addresses reach log lines, and a
// control character there forges log records.
func Valid(addr string) bool {
	if addr == "" || len(addr) > maxLength {
		return false
	}
	if strings.Count(addr, "@") != 1 {
		return false
	}
	for _, r := range addr {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return false
		}
	}
	local, domain := Split(addr)
	if local == "" || len(domain) < 3 {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	// A leading dot never forms a resolvable label. A trailing one is left
	// alone: an absolute FQDN is legal in an envelope path and refusing it
	// would drop outbound mail that the previous validators accepted.
	return !strings.HasPrefix(domain, ".")
}
