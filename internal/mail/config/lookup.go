package config

import "strings"

// MatchDomain resolves a hostname to its configured mail domain, walking up
// the label hierarchy so mail.example.com matches an example.com entry.
//
// The walk-up is the rule the TLS SNI callbacks and the DKIM signer already
// applied by hand; sharing it keeps "is this host ours" answering the same way
// at every door, including the SMTP postmaster gate that decides whether a
// message is accepted at all.
func (c Config) MatchDomain(host string) (Domain, bool) {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for h != "" {
		if d, ok := c.Domains[h]; ok {
			return d, true
		}
		idx := strings.Index(h, ".")
		if idx < 0 {
			break
		}
		h = h[idx+1:]
	}
	return Domain{}, false
}
