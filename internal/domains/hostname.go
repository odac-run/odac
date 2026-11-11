package domains

import (
	"net/netip"
	"strings"

	"golang.org/x/net/idna"
)

const (
	maxHostnameLen = 253
	maxLabelLen    = 63
)

// hostnameValid reports whether domain (already lowercased) is a name ODAC
// can serve, and returns its ASCII form: an IPv4 literal, "localhost", or a
// hostname of LDH labels with an optional leading "*." wildcard label.
// Internationalized names are converted to punycode, since that is what DNS
// queries, SNI and ACME carry; a Unicode key would match none of them.
func hostnameValid(domain string) (string, bool) {
	if domain == "localhost" {
		return domain, true
	}
	if addr, err := netip.ParseAddr(domain); err == nil {
		return domain, addr.Is4()
	}

	name := domain
	wildcard := strings.HasPrefix(name, "*.")
	if wildcard {
		name = name[2:]
	}
	if !isASCII(name) {
		converted, err := idna.Lookup.ToASCII(name)
		if err != nil {
			return "", false
		}
		name = converted
	}

	limit := maxHostnameLen
	if wildcard {
		limit -= len("*.")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 || len(name) > limit {
		return "", false
	}
	for _, label := range labels {
		if !ldhLabel(label) {
			return "", false
		}
	}
	// RFC 3696 section 2: a top-level label is never all digits, which also
	// keeps 999.1.1.1 from passing as a name after failing as an address.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", false
	}
	if wildcard {
		name = "*." + name
	}
	return name, true
}

// ldhLabel is RFC 1123: letters, digits and interior hyphens, 1 to 63 bytes.
// Underscores are refused because no public CA issues for them.
func ldhLabel(label string) bool {
	if label == "" || len(label) > maxLabelLen || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
