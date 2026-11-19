package spam

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/textproto"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"odac/internal/mail/address"
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

// checkHeaders checks the header shape RFC 5322 requires and returns the
// single RFC5322.From address, "" when there is no single one.
func (e *evaluation) checkHeaders() string {
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

	froms := h.Values("From")
	switch len(froms) {
	case 0:
		e.hit("FROM_MISSING", 3)
		return ""
	case 1:
	default:
		e.hit("FROM_MULTIPLE", 3)
		return ""
	}
	list, err := mail.ParseAddressList(froms[0])
	if err != nil || len(list) == 0 {
		e.hit("FROM_INVALID", 2)
		return ""
	}
	if len(list) > 1 {
		// DMARC has no single author domain to evaluate (RFC 7489 6.6.1).
		e.hit("FROM_MULTIPLE", 3)
		return ""
	}
	return strings.ToLower(list[0].Address)
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

// checkDMARC evaluates RFC 7489 for the From domain. It returns whether SPF
// or DKIM produced an aligned pass (relaxed alignment when the domain
// publishes no policy, which is what the local spoof check relies on), and
// the action the published policy asks for on failure.
func (e *evaluation) checkDMARC(from string, spfPass bool, spfDomain string, dkimPass []string) (bool, Action) {
	if from == "" {
		return false, Inbox
	}
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
		return aligned, Inbox
	case errors.Is(err, dmarc.ErrNoPolicy):
		res.Value = authres.ResultNone
		return aligned, Inbox
	case err != nil:
		res.Value = authres.ResultPermError
		return aligned, Inbox
	}

	if aligned {
		res.Value = authres.ResultPass
		e.hit("DMARC_PASS", -1)
		return true, Inbox
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
			return false, Defer
		}
		e.hit("DMARC_REJECT", 4)
		return false, Reject
	case dmarc.PolicyQuarantine:
		e.hit("DMARC_QUARANTINE", 3)
		return false, Junk
	}
	e.hit("DMARC_FAIL", 1.5)
	return false, Inbox
}

// checkLocalDomains refuses mail from outside that claims one of this
// server's domains, in From or in the envelope, without an SPF or DKIM pass
// for it. The domain's owner is the only one who can make those pass, so a
// third-party service it set up still gets through, whatever DMARC record the
// domain publishes, if any.
func (e *evaluation) checkLocalDomains(fromDomain string, fromAligned, spfPass bool, spfDomain string, dkimPass []string) (Action, string) {
	var claimed []string
	if fromDomain != "" && !fromAligned && e.isLocal(fromDomain) {
		e.hit("FROM_LOCAL_SPOOF", 4)
		claimed = append(claimed, fromDomain)
	}
	envDomain := strings.ToLower(strings.TrimSuffix(address.Domain(e.env.MailFrom), "."))
	if envDomain != "" && envDomain != fromDomain && e.isLocal(envDomain) && !authorized(envDomain, spfPass, spfDomain, dkimPass) {
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
func (e *evaluation) forgedAccount(fromAddr string) (string, error) {
	if e.f.accounts == nil {
		return "", nil
	}
	seen := ""
	for _, addr := range []string{e.env.MailFrom, fromAddr} {
		addr = strings.ToLower(addr)
		if addr == "" || addr == seen || !address.Valid(addr) {
			continue
		}
		seen = addr
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
