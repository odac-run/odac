// Package spam screens unauthenticated inbound mail before it reaches a
// mailbox. It checks the sender's authentication (SPF, DKIM, DMARC), the
// HELO name and the basic header shape, adds the results to a score, and
// decides whether the message goes to the inbox, to Junk, or is refused.
//
// Every DNS failure is "unknown", never evidence: a resolver that stalls must
// not turn every inbound message into spam.
package spam

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/authres"

	"odac/internal/mail/config"
)

// Mode decides how far a verdict is acted on. It exists so an operator can
// watch the filter's decisions (tag) or soften them (junk) without turning it
// off, since a false positive on a rejected message is invisible to the
// recipient.
type Mode string

const (
	// ModeOff skips every check and adds no header.
	ModeOff Mode = "off"
	// ModeTag adds the result headers but always delivers to the inbox.
	ModeTag Mode = "tag"
	// ModeJunk moves suspected spam to Junk and never refuses a message.
	ModeJunk Mode = "junk"
	// ModeEnforce also refuses mail whose domain publishes DMARC p=reject,
	// and mail from outside naming a local account as its sender.
	ModeEnforce Mode = "enforce"
)

// ParseMode reads a mode name, case-insensitively.
func ParseMode(s string) (Mode, bool) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case ModeOff, ModeTag, ModeJunk, ModeEnforce:
		return m, true
	}
	return "", false
}

// ModeFromEnv returns the mode set by ODAC_MAIL_SPAM, enforce when unset or
// invalid.
func ModeFromEnv() Mode {
	raw := os.Getenv("ODAC_MAIL_SPAM")
	if raw == "" {
		return ModeEnforce
	}
	m, ok := ParseMode(raw)
	if !ok {
		log.Printf("[SMTP Spam] Ignoring invalid ODAC_MAIL_SPAM=%q, using %s", raw, ModeEnforce)
		return ModeEnforce
	}
	return m
}

// Action is where a screened message goes.
type Action int

const (
	// Inbox delivers normally.
	Inbox Action = iota
	// Junk delivers to the recipient's Junk mailbox.
	Junk
	// Defer refuses the message with a temporary SMTP error, for a check
	// that could not reach a conclusion and would otherwise refuse it. The
	// sending server retries.
	Defer
	// Reject refuses the message with a permanent SMTP error.
	Reject
)

func (a Action) String() string {
	switch a {
	case Junk:
		return "junk"
	case Defer:
		return "defer"
	case Reject:
		return "reject"
	}
	return "inbox"
}

// JunkThreshold is the score at which a message is moved to Junk.
const JunkThreshold = 5.0

const (
	defaultTimeout    = 15 * time.Second
	maxDKIMSignatures = 5
	// maxAuthors bounds the From addresses checked, each with a DMARC lookup
	// and an account lookup.
	maxAuthors = 8

	deferReason = "cannot verify the sender right now, try again later"
)

// Envelope is what the SMTP session knows about a message besides its bytes.
type Envelope struct {
	IP       net.IP
	Helo     string
	MailFrom string // empty for the null reverse path
}

// Verdict is the outcome of screening one message. The zero value means "not
// screened": inbox, and Stamp adds nothing.
type Verdict struct {
	Action Action
	Score  float64
	Tests  []string
	// Reason is the SMTP reply text when Action is Defer or Reject.
	Reason string

	checked    bool
	authservID string
	results    []authres.Result
}

// Mailbox is the mailbox the message is stored in.
func (v Verdict) Mailbox() string {
	if v.Action == Junk {
		return "Junk"
	}
	return "INBOX"
}

// Stamp prepends the Authentication-Results and X-Spam-Status headers to the
// raw message. Mail clients and later filters read the topmost
// Authentication-Results carrying this server's id, so the sender's own
// copies further down are left in place.
func (v Verdict) Stamp(raw []byte) []byte {
	if !v.checked {
		return raw
	}
	var b bytes.Buffer
	b.Grow(len(raw) + 512)
	b.WriteString("Authentication-Results: ")
	// Values are restricted to safe tokens (see safe), so "; " only appears
	// between results and is a valid place to fold. Format leaves a space
	// after a result with no parameters ("dkim=none ;"), which goes too.
	ar := strings.TrimSpace(strings.ReplaceAll(authres.Format(v.authservID, v.results), " ;", ";"))
	b.WriteString(strings.ReplaceAll(ar, "; ", ";\r\n\t"))
	b.WriteString("\r\nX-Spam-Status: ")
	if v.Action != Inbox || v.Score >= JunkThreshold {
		b.WriteString("Yes")
	} else {
		b.WriteString("No")
	}
	tests := "none"
	if len(v.Tests) > 0 {
		tests = strings.Join(v.Tests, ",")
	}
	fmt.Fprintf(&b, ", score=%.1f required=%.1f tests=%s\r\n", v.Score, JunkThreshold, tests)
	b.Write(raw)
	return b.Bytes()
}

// Resolver is the DNS surface the checks need; *net.Resolver satisfies it.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Filter screens inbound messages. It holds no per-message state and is safe
// for concurrent use.
type Filter struct {
	getConfig func() config.Config
	mode      Mode
	resolver  Resolver
	timeout   time.Duration
	roll      func() int // 0..99, for DMARC pct sampling
	accounts  AccountLookup
}

// AccountLookup reports whether email is a local account.
type AccountLookup func(ctx context.Context, email string) (bool, error)

// Option configures a Filter.
type Option func(*Filter)

// WithResolver replaces the system resolver.
func WithResolver(r Resolver) Option { return func(f *Filter) { f.resolver = r } }

// WithAccounts enables refusing mail from outside that names a local account
// as its sender.
func WithAccounts(lookup AccountLookup) Option { return func(f *Filter) { f.accounts = lookup } }

// New returns a Filter that reads the local domains and hostname from
// getConfig on every message, so a config push applies to the next one.
func New(getConfig func() config.Config, mode Mode, opts ...Option) *Filter {
	f := &Filter{
		getConfig: getConfig,
		mode:      mode,
		resolver:  net.DefaultResolver,
		timeout:   defaultTimeout,
		roll:      func() int { return rand.IntN(100) },
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Mode reports the filter's mode.
func (f *Filter) Mode() Mode { return f.mode }

// Check screens one message. It never fails: a check that cannot reach a
// conclusion contributes nothing, and a panic in a parser yields the zero
// Verdict, so the worst case is the behaviour without a filter.
func (f *Filter) Check(ctx context.Context, env Envelope, raw []byte) (v Verdict) {
	if f == nil || f.mode == ModeOff || env.IP == nil || env.IP.IsLoopback() {
		return Verdict{}
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SMTP Spam] Check panicked for ip=%s from=%s, delivering unscreened: %v\n%s",
				env.IP, env.MailFrom, r, debug.Stack())
			v = Verdict{}
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	return f.evaluate(ctx, env, raw)
}

func (f *Filter) evaluate(ctx context.Context, env Envelope, raw []byte) Verdict {
	cfg := f.getConfig()
	e := &evaluation{ctx: ctx, f: f, cfg: cfg, env: env, raw: raw}

	e.checkHelo()
	spfPass, spfDomain := e.checkSPF()
	authors := e.checkHeaders()
	tooMany := len(authors) > maxAuthors
	if tooMany {
		e.hit("FROM_TOO_MANY", JunkThreshold)
		authors = authors[:maxAuthors]
	}
	domains := authorDomains(authors)
	dkimPass := e.checkDKIM()
	aligned, policy, policyDomain := e.checkDMARC(domains, spfPass, spfDomain, dkimPass)

	if !spfPass && len(dkimPass) == 0 {
		e.hit("NO_AUTH", 1.5)
	}

	var reason string
	switch policy {
	case Reject:
		reason = fmt.Sprintf("message from %s rejected per its domain's DMARC policy", policyDomain)
	case Defer:
		reason = deferReason
	}
	if local, why := e.checkLocalDomains(domains, aligned, spfPass, spfDomain, dkimPass); local >= policy && local != Inbox {
		policy, reason = local, why
	}

	// A failed account lookup proves nothing either way, so the sender is
	// asked to retry rather than told its mail was forged.
	switch forged, err := e.forgedAccount(authors); {
	case err != nil:
		log.Printf("[SMTP Spam] Account lookup failed, deferring ip=%s from=%s: %v", e.env.IP, e.env.MailFrom, err)
		if policy < Defer {
			policy, reason = Defer, deferReason
		}
	case forged != "":
		e.hit("LOCAL_ACCOUNT_FORGED", 10)
		policy = Reject
		reason = fmt.Sprintf("%s is a local account and only sends through this server", forged)
	}

	// Past maxAuthors some addresses went unchecked, and no legitimate
	// message names that many authors (RFC 7489 6.6.1).
	if tooMany && policy != Reject {
		policy = Reject
		reason = fmt.Sprintf("message names more than %d From addresses", maxAuthors)
	}

	action := Inbox
	if e.score >= JunkThreshold {
		action = Junk
	}
	if policy > action {
		action = policy
	}
	switch f.mode {
	case ModeTag:
		action = Inbox
	case ModeJunk:
		action = min(action, Junk)
	}

	v := Verdict{
		Action:     action,
		Score:      e.score,
		Tests:      e.tests,
		checked:    true,
		authservID: authservID(cfg),
		results:    e.results,
	}
	if action >= Defer {
		v.Reason = reason
	}
	return v
}

// evaluation is the state of one Check.
type evaluation struct {
	ctx context.Context
	f   *Filter
	cfg config.Config
	env Envelope
	raw []byte

	score   float64
	tests   []string
	results []authres.Result

	// Domains whose SPF or DKIM check hit a temporary DNS failure.
	spfTemp  string
	dkimTemp []string
}

func (e *evaluation) hit(test string, score float64) {
	e.tests = append(e.tests, test)
	e.score += score
}

func (e *evaluation) lookupTXT(name string) ([]string, error) {
	return e.f.resolver.LookupTXT(e.ctx, name)
}

func authservID(cfg config.Config) string {
	if id := safe(strings.ToLower(cfg.Hostname)); id != "" {
		return id
	}
	return "odac"
}

// safe returns s when it is a plain token that cannot break out of an
// Authentication-Results value, and "" otherwise. Every value placed in the
// header comes from the sender.
func safe(s string) string {
	if len(s) > 255 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || c == ';' || c == '"' || c == '\\' || c == '(' || c == ')' {
			return ""
		}
	}
	return s
}
