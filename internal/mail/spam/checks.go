package spam

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"unicode"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"odac/internal/mail/address"
	"odac/internal/mail/mimetree"
)

// heloDomain returns the HELO argument when it names a domain, "" for an
// address literal or a bare IP.
func heloDomain(helo string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(helo)), ".")
	if strings.HasPrefix(h, "[") || net.ParseIP(h) != nil {
		return ""
	}
	return h
}

func (e *evaluation) checkHelo() {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e.env.Helo)), ".")
	switch {
	case strings.HasPrefix(h, "["):
		// An address literal is legal (RFC 5321 4.1.3).
	case net.ParseIP(h) != nil:
		e.hit("HELO_BARE_IP", 1)
	case !strings.Contains(h, "."):
		e.hit("HELO_NOT_FQDN", 1)
	default:
		_, ours := e.cfg.MatchDomain(h)
		if ours || (e.cfg.Hostname != "" && h == strings.ToLower(e.cfg.Hostname)) {
			e.hit("HELO_FORGED_LOCAL", 3)
		}
	}
}

// checkSPF evaluates the MAIL FROM identity, or the HELO identity for the
// null reverse path (RFC 7208 2.4), and returns whether it passed together
// with the domain DMARC aligns against.
func (e *evaluation) checkSPF() (bool, string) {
	helo := heloDomain(e.env.Helo)
	sender := e.env.MailFrom
	domain := strings.ToLower(address.Domain(sender))
	if sender == "" && helo != "" {
		sender = "postmaster@" + helo
		domain = helo
	}

	res := &authres.SPFResult{From: safe(sender), Helo: safe(helo)}
	e.results = append(e.results, res)
	if domain == "" {
		res.Value = authres.ResultNone
		return false, ""
	}

	result, _ := spf.CheckHostWithSender(e.env.IP, helo, sender,
		spf.WithContext(e.ctx), spf.WithResolver(e.f.resolver))
	res.Value = authres.ResultValue(result)
	if result == spf.TempError {
		e.spfTemp = domain
	}
	switch result {
	case spf.Fail:
		e.hit("SPF_FAIL", 3.5)
	case spf.SoftFail:
		e.hit("SPF_SOFTFAIL", 1.5)
	case spf.PermError:
		e.hit("SPF_PERMERROR", 1)
	}
	return result == spf.Pass, domain
}

// checkHeaders checks the header shape RFC 5322 requires and returns every
// address a reader may take as the author: each address of every From field,
// in the form author gives it. Mail clients show one of them however many
// there are, so each is checked; returning none for an ambiguous From let
// the one a client showed through unchecked.
func (e *evaluation) checkHeaders() []string {
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(e.raw))).ReadMIMEHeader()
	if err != nil {
		e.hit("HEADER_MALFORMED", 2)
	}

	if len(h.Values("Date")) == 0 {
		e.hit("DATE_MISSING", 1)
	}
	if len(h.Values("Message-Id")) == 0 {
		e.hit("MSGID_MISSING", 0.5)
	}

	// Read leniently, as the display columns are: net/textproto stops at
	// the first malformed line, and a From below it is still shown.
	var froms []string
	for _, f := range mimetree.Fields(e.raw[:mimetree.HeaderEnd(e.raw)]) {
		if f.Name == "from" {
			froms = append(froms, f.Value)
		}
	}
	if len(froms) == 0 {
		e.hit("FROM_MISSING", 3)
		return nil
	}

	var authors []string
	invalid := false
	for _, v := range froms {
		addrs, ok := fromAddresses(v)
		invalid = invalid || !ok
		for _, a := range addrs {
			if a = author(a); a != "" && !slices.Contains(authors, a) {
				authors = append(authors, a)
			}
		}
	}
	if invalid || len(authors) == 0 {
		e.hit("FROM_INVALID", 2)
	}
	if len(froms) > 1 || len(authors) > 1 {
		// DMARC has no single author domain to evaluate (RFC 7489 6.6.1).
		e.hit("FROM_MULTIPLE", 3)
	}
	return authors
}

// fromAddresses returns the addresses of one From value and whether it
// parsed. A value net/mail refuses still reaches the reader, so every token
// holding an "@" counts: one of them is what a client salvages from it.
func fromAddresses(v string) ([]string, bool) {
	if list, err := mail.ParseAddressList(v); err == nil && len(list) > 0 {
		out := make([]string, len(list))
		for i, a := range list {
			out[i] = a.Address
		}
		return out, true
	}
	var out []string
	for _, tok := range strings.FieldsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`<>,;:"'()`, r)
	}) {
		if strings.Contains(tok, "@") {
			out = append(out, tok)
		}
	}
	return out, false
}

// author returns addr lowercased and without an absolute domain's trailing
// dot, which names the same mailbox and the same domain, or "" when it has
// no domain. Accounts and local domains are stored without the dot.
func author(addr string) string {
	addr = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(addr)), ".")
	if address.Domain(addr) == "" {
		return ""
	}
	return addr
}

// authorDomains returns the distinct domains of authors, in order.
func authorDomains(authors []string) []string {
	var out []string
	for _, a := range authors {
		if d := address.Domain(a); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// checkDKIM verifies up to maxDKIMSignatures signatures and returns the
// domains whose signature passed.
func (e *evaluation) checkDKIM() []string {
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(e.raw), &dkim.VerifyOptions{
		LookupTXT:        e.lookupTXT,
		MaxVerifications: maxDKIMSignatures,
	})
	if err != nil {
		e.results = append(e.results, &authres.DKIMResult{Value: authres.ResultPermError})
		return nil
	}
	if len(verifs) == 0 {
		e.results = append(e.results, &authres.DKIMResult{Value: authres.ResultNone})
		return nil
	}

	var passed []string
	failed := false
	for _, v := range verifs {
		res := &authres.DKIMResult{Domain: safe(strings.ToLower(v.Domain)), Identifier: safe(v.Identifier)}
		switch {
		case v.Err == nil:
			res.Value = authres.ResultPass
			passed = append(passed, strings.ToLower(v.Domain))
		case dkim.IsTempFail(v.Err):
			res.Value = authres.ResultTempError
			e.dkimTemp = append(e.dkimTemp, strings.ToLower(v.Domain))
		case dkim.IsPermFail(v.Err):
			res.Value = authres.ResultPermError
			failed = true
		default:
			res.Value = authres.ResultFail
			failed = true
		}
		e.results = append(e.results, res)
	}
	if len(passed) == 0 && failed {
		e.hit("DKIM_INVALID", 1.5)
	}
	return passed
}

// dmarcResult is the outcome of DMARC for one author domain.
type dmarcResult struct {
	aligned bool
	action  Action
	test    string
	score   float64
}

// checkDMARC evaluates RFC 7489 for every author domain and applies the
// strictest policy among the ones that fail (RFC 7489 6.6.1). It returns, per
// domain, whether SPF or DKIM produced an aligned pass (relaxed alignment
// when the domain publishes no policy, which is what the local spoof check
// relies on), the action and the domain that decided it.
func (e *evaluation) checkDMARC(domains []string, spfPass bool, spfDomain string, dkimPass []string) (map[string]bool, Action, string) {
	aligned := make(map[string]bool, len(domains))
	var worst dmarcResult
	worstDomain := ""
	allPass := len(domains) > 0
	for i, d := range domains {
		r := e.dmarcFor(d, spfPass, spfDomain, dkimPass)
		aligned[d] = r.aligned
		allPass = allPass && r.test == "DMARC_PASS"
		if i == 0 || r.action > worst.action || (r.action == worst.action && r.score > worst.score) {
			worst, worstDomain = r, d
		}
	}
	// One score per message: a pass for each of several domains the sender
	// controls must not buy its way under the Junk threshold.
	if allPass {
		e.hit("DMARC_PASS", -1)
		return aligned, Inbox, ""
	}
	if worst.test != "" {
		e.hit(worst.test, worst.score)
	}
	return aligned, worst.action, worstDomain
}

// dmarcFor evaluates RFC 7489 for one From domain.
func (e *evaluation) dmarcFor(from string, spfPass bool, spfDomain string, dkimPass []string) dmarcResult {
	res := &authres.DMARCResult{From: safe(from)}
	e.results = append(e.results, res)

	rec, policyDomain, err := e.lookupDMARC(from)
	spfMode, dkimMode := dmarc.AlignmentRelaxed, dmarc.AlignmentRelaxed
	if rec != nil {
		spfMode, dkimMode = string(rec.SPFAlignment), string(rec.DKIMAlignment)
	}
	aligned := spfPass && isAligned(spfDomain, from, spfMode)
	for _, d := range dkimPass {
		aligned = aligned || isAligned(d, from, dkimMode)
	}

	switch {
	case dmarc.IsTempFail(err):
		res.Value = authres.ResultTempError
		return dmarcResult{aligned: aligned}
	case errors.Is(err, dmarc.ErrNoPolicy):
		res.Value = authres.ResultNone
		return dmarcResult{aligned: aligned}
	case err != nil:
		res.Value = authres.ResultPermError
		return dmarcResult{aligned: aligned}
	}

	if aligned {
		res.Value = authres.ResultPass
		return dmarcResult{aligned: true, test: "DMARC_PASS", score: -1}
	}
	res.Value = authres.ResultFail

	policy := rec.Policy
	if policyDomain != from && rec.SubdomainPolicy != "" {
		policy = rec.SubdomainPolicy
	}
	// pct samples which failing messages get the policy; the rest get the
	// next weaker one (RFC 7489 6.6.4).
	if rec.Percent != nil && e.f.roll() >= *rec.Percent {
		switch policy {
		case dmarc.PolicyReject:
			policy = dmarc.PolicyQuarantine
		case dmarc.PolicyQuarantine:
			policy = dmarc.PolicyNone
		}
	}

	switch policy {
	case dmarc.PolicyReject:
		if e.pending(from, spfMode, dkimMode) {
			// The pass may only be missing because DNS timed out.
			res.Value = authres.ResultTempError
			return dmarcResult{action: Defer}
		}
		return dmarcResult{action: Reject, test: "DMARC_REJECT", score: 4}
	case dmarc.PolicyQuarantine:
		return dmarcResult{action: Junk, test: "DMARC_QUARANTINE", score: 3}
	}
	return dmarcResult{test: "DMARC_FAIL", score: 1.5}
}

// checkLocalDomains refuses mail from outside that claims one of this
// server's domains, in From or in the envelope, without an SPF or DKIM pass
// for it. The domain's owner is the only one who can make those pass, so a
// third-party service it set up still gets through, whatever DMARC record the
// domain publishes, if any.
func (e *evaluation) checkLocalDomains(fromDomains []string, fromAligned map[string]bool, spfPass bool, spfDomain string, dkimPass []string) (Action, string) {
	var claimed []string
	for _, d := range fromDomains {
		if !fromAligned[d] && e.isLocal(d) {
			claimed = append(claimed, d)
		}
	}
	if len(claimed) > 0 {
		e.hit("FROM_LOCAL_SPOOF", 4)
	}
	envDomain := address.Domain(author(e.env.MailFrom))
	if envDomain != "" && !slices.Contains(fromDomains, envDomain) && e.isLocal(envDomain) && !authorized(envDomain, spfPass, spfDomain, dkimPass) {
		e.hit("MAILFROM_LOCAL_SPOOF", 4)
		claimed = append(claimed, envDomain)
	}
	if len(claimed) == 0 {
		return Inbox, ""
	}
	for _, d := range claimed {
		if !e.pending(d, dmarc.AlignmentRelaxed, dmarc.AlignmentRelaxed) {
			return Reject, fmt.Sprintf("%s is a local domain and this message passes neither its SPF nor its DKIM", d)
		}
	}
	return Defer, deferReason
}

func (e *evaluation) isLocal(domain string) bool {
	_, ok := e.cfg.MatchDomain(domain)
	return ok
}

// authorized reports whether SPF or DKIM passed for domain under relaxed
// alignment.
func authorized(domain string, spfPass bool, spfDomain string, dkimPass []string) bool {
	if spfPass && isAligned(spfDomain, domain, dmarc.AlignmentRelaxed) {
		return true
	}
	for _, d := range dkimPass {
		if isAligned(d, domain, dmarc.AlignmentRelaxed) {
			return true
		}
	}
	return false
}

// pending reports whether an SPF or DKIM check that hit a temporary DNS
// failure could still have authenticated domain.
func (e *evaluation) pending(domain, spfMode, dkimMode string) bool {
	if e.spfTemp != "" && isAligned(e.spfTemp, domain, spfMode) {
		return true
	}
	for _, d := range e.dkimTemp {
		if isAligned(d, domain, dkimMode) {
			return true
		}
	}
	return false
}

// forgedAccount returns the local account a message names as its sender,
// in the envelope or in From, or "" when it names none. Every local account
// sends through an authenticated session, so a message from outside naming
// one did not leave this server and is forged, whatever SPF or DKIM say.
func (e *evaluation) forgedAccount(authors []string) (string, error) {
	if e.f.accounts == nil {
		return "", nil
	}
	var seen []string
	for _, addr := range append([]string{author(e.env.MailFrom)}, authors...) {
		if addr == "" || slices.Contains(seen, addr) || !address.Valid(addr) {
			continue
		}
		seen = append(seen, addr)
		local, err := e.f.accounts(e.ctx, addr)
		if err != nil {
			return "", err
		}
		if local {
			return addr, nil
		}
	}
	return "", nil
}

// lookupDMARC finds the policy for domain, falling back to its organizational
// domain, and returns the domain the record was found at.
func (e *evaluation) lookupDMARC(domain string) (*dmarc.Record, string, error) {
	opts := &dmarc.LookupOptions{LookupTXT: e.lookupTXT}
	rec, err := dmarc.LookupWithOptions(domain, opts)
	if !errors.Is(err, dmarc.ErrNoPolicy) {
		return rec, domain, err
	}
	org := orgDomain(domain)
	if org == domain {
		return nil, domain, err
	}
	rec, err = dmarc.LookupWithOptions(org, opts)
	return rec, org, err
}

func orgDomain(domain string) string {
	org, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return org
}

func isAligned(authDomain, fromDomain, mode string) bool {
	a := strings.ToLower(strings.TrimSuffix(authDomain, "."))
	f := strings.ToLower(strings.TrimSuffix(fromDomain, "."))
	if a == "" || f == "" {
		return false
	}
	if mode == string(dmarc.AlignmentStrict) {
		return a == f
	}
	return orgDomain(a) == orgDomain(f)
}
