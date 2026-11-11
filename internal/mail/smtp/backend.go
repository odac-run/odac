// Package smtp implements the inbound SMTP server and outbound delivery client.
// The inbound server uses github.com/emersion/go-smtp with a custom Backend
// that authenticates against the SQLite store and delivers to local mailboxes.
// Architecture mirrors the Node.js Mail.js onAuth/onData/onMailFrom/onRcptTo callbacks.
package smtp

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"odac/internal/mail/address"
	"odac/internal/mail/auth"
	"odac/internal/mail/blob"
	"odac/internal/mail/config"
	"odac/internal/mail/limits"
	"odac/internal/mail/message"
	"odac/internal/mail/storage"
)

// Backend implements smtp.Backend for the inbound SMTP server.
// Handles authentication, message reception, and local delivery.
//
// One Backend instance is bound to one listener (port 25 or 465) so the
// limiter and log tag reflect that listener's traffic in isolation.
type Backend struct {
	blobs     *blob.Store
	firewall  *auth.Firewall
	getConfig func() config.Config
	limiter   *limits.Limiter
	outbox    Outbox
	store     *storage.Store
	tag       string // "inbound" or "submission" — included in log lines
}

// Outbox is told when a transaction has queued mail for remote recipients.
type Outbox interface {
	Wake()
}

// NewBackend creates a new SMTP backend with the given dependencies. outbox
// may be nil, in which case queued rows wait for the queue's next poll.
func NewBackend(store *storage.Store, blobs *blob.Store, fw *auth.Firewall, getConfig func() config.Config, limiter *limits.Limiter, outbox Outbox, tag string) *Backend {
	return &Backend{
		blobs:     blobs,
		firewall:  fw,
		getConfig: getConfig,
		limiter:   limiter,
		outbox:    outbox,
		store:     store,
		tag:       tag,
	}
}

// NewSession is called for each new SMTP connection.
// Checks IP blocklist and acquires a limiter handle before allowing the
// session to proceed. The handle is released in Logout.
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	ip := extractIP(c.Conn().RemoteAddr().String())
	if b.firewall.IsBlocked(ip) {
		log.Printf("[SMTP %s] Connection blocked by firewall: %s", b.tag, ip)
		return nil, errors.New("your IP is blocked due to suspicious activity")
	}

	handle, reason := b.limiter.Acquire(ip)
	if reason != limits.ReasonOK {
		log.Printf("[SMTP %s] Rejecting %s: %s", b.tag, ip, reason)
		return nil, errors.New("too many connections, try again later")
	}

	total, ips, users := b.limiter.Snapshot()
	log.Printf("[SMTP %s] Connection accepted: %s (total=%d ips=%d users=%d)", b.tag, ip, total, ips, users)

	return &Session{
		backend: b,
		ip:      ip,
		limit:   handle,
	}, nil
}

// Session represents a single SMTP connection session.
// Tracks authentication state, sender, and recipients per transaction.
type Session struct {
	backend    *Backend
	from       string
	ip         string
	limit      *limits.Handle // released in Logout
	recipients []string
	user       string // Authenticated user (empty if unauthenticated)
}

// AuthMechanisms returns the supported SASL authentication mechanisms.
func (s *Session) AuthMechanisms() []string {
	return []string{"PLAIN", "LOGIN"}
}

// Auth handles SMTP authentication via SASL mechanism.
// Returns a sasl.Server that validates credentials against the SQLite store.
func (s *Session) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if !address.Valid(username) {
			log.Printf("[SMTP] Auth failed (invalid username format) %q from %s", username, s.ip)
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		account, err := s.backend.store.AccountExists(ctx, username)
		if err != nil {
			log.Printf("[SMTP] Auth lookup error for %s from %s: %v", username, s.ip, err)
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}
		if account == nil {
			log.Printf("[SMTP] Auth failed (no such account) %s from %s", username, s.ip)
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		match, err := auth.ComparePassword(password, account.Password)
		if err != nil {
			log.Printf("[SMTP] Auth password compare error for %s from %s: %v", username, s.ip, err)
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}
		if !match {
			log.Printf("[SMTP] Auth failed (bad password) %s from %s", username, s.ip)
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		if reason := s.limit.BindUser(username); reason != limits.ReasonOK {
			log.Printf("[SMTP %s] Post-auth limit hit for %s from %s: %s", s.backend.tag, username, s.ip, reason)
			return errors.New("too many connections for user, try again later")
		}

		// Successful login — clear failed attempts
		s.backend.firewall.ClearAttempts(s.ip)
		// The account's stored spelling, not the peer's: every mailbox row this
		// session writes is keyed off it, so a login as <Ali@x.com> must not
		// open a second set of rows beside <ali@x.com>.
		s.user = account.Email
		log.Printf("[SMTP %s] User authenticated: %s from %s", s.backend.tag, username, s.ip)

		// Transparent password upgrade: rehash legacy N=16384 → current N=32768
		if auth.NeedsRehash(account.Password) {
			go func() {
				newHash, err := auth.HashPassword(password)
				if err != nil {
					return
				}
				ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel2()
				if err := s.backend.store.AccountUpdatePassword(ctx2, username, newHash); err == nil {
					log.Printf("[SMTP] Password rehashed for %s (scrypt N upgraded)", username)
				}
			}()
		}

		return nil
	}), nil
}

// Mail is called for MAIL FROM command. Validates sender address format.
// When the session is authenticated, the sender address must match the
// authenticated user to prevent impersonation of other accounts.
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	if !address.Valid(from) {
		log.Printf("[SMTP] MAIL FROM rejected (invalid email %q) from %s", from, s.ip)
		return errors.New("invalid email address")
	}
	if s.user != "" && !strings.EqualFold(from, s.user) {
		log.Printf("[SMTP] Sender mismatch: %s attempted MAIL FROM <%s> from %s", s.user, from, s.ip)
		return errors.New("sender address does not match authenticated user")
	}
	s.from = from
	log.Printf("[SMTP] MAIL FROM <%s> accepted (auth=%q) from %s", from, s.user, s.ip)
	return nil
}

// Rcpt is called for RCPT TO command.
// Enforces anti-relay: unauthenticated sessions can only deliver to local accounts.
// Authenticated users can send to any address (outbound delivery).
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !address.Valid(to) {
		log.Printf("[SMTP] RCPT TO rejected (invalid email %q) from=%s ip=%s", to, s.from, s.ip)
		return errors.New("invalid email address")
	}

	// Authenticated users can send anywhere
	if s.user != "" {
		s.recipients = append(s.recipients, to)
		log.Printf("[SMTP] RCPT TO <%s> accepted (authenticated %s) from %s", to, s.user, s.ip)
		return nil
	}

	// Unauthenticated: only allow delivery to local accounts or postmaster/hostmaster
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if s.isRoleAddress(to) {
		s.recipients = append(s.recipients, to)
		log.Printf("[SMTP] RCPT TO <%s> accepted (role address, configured domain) from=%s ip=%s", to, s.from, s.ip)
		return nil
	}

	account, err := s.backend.store.AccountExists(ctx, to)
	if err != nil {
		log.Printf("[SMTP] RCPT TO <%s> account lookup error from=%s ip=%s: %v", to, s.from, s.ip, err)
		return errors.New("relay access denied")
	}
	if account == nil {
		log.Printf("[SMTP] RCPT TO <%s> rejected (no local account) from=%s ip=%s", to, s.from, s.ip)
		return errors.New("relay access denied")
	}

	s.recipients = append(s.recipients, to)
	log.Printf("[SMTP] RCPT TO <%s> accepted (local account) from=%s ip=%s", to, s.from, s.ip)
	return nil
}

// Data is called when the message body is received.
// Parses the RFC 2822 message, stores locally for known recipients,
// and triggers outbound delivery for authenticated senders.
func (s *Session) Data(r io.Reader) error {
	maxBody := config.MaxMessageBytes()
	// Read one byte past the ceiling so a message of exactly maxBody bytes is
	// accepted and only a genuinely oversized one trips the check below.
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		log.Printf("[SMTP] DATA read failed (read=%d sender=%s rcpts=%d ip=%s): %v",
			len(body), s.from, len(s.recipients), s.ip, err)
		return err
	}
	// go-smtp enforces the same ceiling and answers 552 before reaching here.
	// Hitting it anyway means a truncated message, which must be refused
	// rather than stored: a half message parses into a plausible-looking body
	// with every attachment past the cut silently missing.
	if int64(len(body)) > maxBody {
		log.Printf("[SMTP] DATA exceeded %d byte cap sender=%s rcpts=%d ip=%s",
			maxBody, s.from, len(s.recipients), s.ip)
		return &smtp.SMTPError{
			Code:         552,
			EnhancedCode: smtp.EnhancedCode{5, 3, 4},
			Message:      "message exceeds maximum size",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Every recipient is classified before anything is written, so a refusal
	// or a failed lookup never leaves part of the transaction stored.
	plan, err := s.planDelivery(ctx)
	if err != nil {
		return err
	}

	// The verbatim message is the source of truth: IMAP serves BODY[] and
	// BODYSTRUCTURE from it, so attachments survive exactly as sent. The
	// parsed fields below are derived display data, not the record itself.
	rawRef := s.storeRaw(body)
	// Local copies survive a failed blob write through their parsed columns,
	// a queued message does not: the queue reads the bytes it sends from the
	// blob store.
	if rawRef == "" && len(plan.outbound) > 0 {
		log.Printf("[SMTP] DATA cannot queue without the raw message, answering 451: sender=%s ip=%s", s.from, s.ip)
		return errTryLater
	}

	// Parse RFC 2822 message into headers and body parts
	parsed := message.Parse(body)
	log.Printf("[SMTP] DATA received: %d bytes sender=%s rcpts=%v ip=%s msg-id=%q subject=%q html=%dB text=%dB",
		len(body), s.from, s.recipients, s.ip,
		parsed.MessageID, parsed.Subject, len(parsed.HTML), len(parsed.Text))

	rows := make([]*storage.MessageRow, 0, len(plan.inbox)+1)
	for _, email := range plan.inbox {
		rows = append(rows, &storage.MessageRow{
			Email:   email,
			Flags:   toNullString("[]"),
			Mailbox: "INBOX",
			RawRef:  toNullString(rawRef),
		})
	}
	if plan.sent {
		rows = append(rows, &storage.MessageRow{
			Email:   s.user,
			Flags:   toNullString(`["seen"]`),
			Mailbox: "Sent",
			RawRef:  toNullString(rawRef),
		})
	}
	for _, row := range rows {
		parsed.Apply(row)
	}

	// One reply covers every recipient, so the rows commit together or not at
	// all, queue rows included, and a failure answers 451: the sender keeps
	// the message and retries instead of being told it was delivered.
	outbound := OutboundRows(s.from, plan.outbound, rawRef, time.Now())
	if err := s.backend.store.Deliver(ctx, rows, outbound); err != nil {
		log.Printf("[SMTP] DATA store failed, answering 451: sender=%s rcpts=%v ip=%s: %v",
			s.from, s.recipients, s.ip, err)
		return errTryLater
	}
	if len(outbound) > 0 && s.backend.outbox != nil {
		s.backend.outbox.Wake()
	}

	log.Printf("[SMTP] DATA complete: stored=%d queued=%d sender=%s rcpts=%d ip=%s",
		len(plan.inbox), len(plan.outbound), s.from, len(s.recipients), s.ip)
	return nil
}

// errTryLater is the transient reply for a transaction the server could not
// commit. A 4xx makes the sending MTA keep the message and retry.
var errTryLater = &smtp.SMTPError{
	Code:         451,
	EnhancedCode: smtp.EnhancedCode{4, 3, 0},
	Message:      "temporary local failure, try again later",
}

// deliveryPlan is where one transaction's message goes.
type deliveryPlan struct {
	inbox    []string // stored spelling of each local account to receive it
	outbound []string // remote recipients of an authenticated local sender
	sent     bool     // keep a copy in the sender's Sent folder
}

// planDelivery classifies every recipient of the transaction. A lookup error
// is transient and refuses the whole transaction with 451, since storing for
// the recipients that answered and failing the rest has no honest reply.
func (s *Session) planDelivery(ctx context.Context) (deliveryPlan, error) {
	var plan deliveryPlan
	senderIsLocal := s.user != "" && strings.EqualFold(s.from, s.user)
	seen := map[string]bool{}

	for _, rcpt := range s.recipients {
		rcptAccount, err := s.backend.store.AccountExists(ctx, rcpt)
		if err != nil {
			log.Printf("[SMTP] DATA rcpt lookup error for %s, answering 451: %v", rcpt, err)
			return deliveryPlan{}, errTryLater
		}
		rcptIsLocal := rcptAccount != nil

		// The role addresses answer for a configured domain even with no
		// account behind them, the same rule Rcpt applied. Both doors ask the
		// same helper: a domain check that held only at RCPT would still let a
		// hand-built transaction store a row here.
		roleAddress := !rcptIsLocal && s.isRoleAddress(rcpt)

		log.Printf("[SMTP] DATA dispatch: rcpt=%s sender_local=%v rcpt_local=%v role=%v from=%s ip=%s",
			rcpt, senderIsLocal, rcptIsLocal, roleAddress, s.from, s.ip)

		if !senderIsLocal && !rcptIsLocal && !roleAddress {
			log.Printf("[SMTP] Rejected relay attempt: %s -> %s from %s", s.from, rcpt, s.ip)
			return deliveryPlan{}, errors.New("relay access denied")
		}

		switch {
		case roleAddress:
			// Accepted and dropped. Storing it would write a row keyed to an
			// address no session can log in as, so nothing could ever open it
			// while the row and its blob still consume disk. Creating the
			// mailbox is what makes the mail readable.
			log.Printf("[SMTP] Accepted and discarded (role address, no mailbox): rcpt=%s from=%s ip=%s", rcpt, s.from, s.ip)
		case rcptIsLocal && strings.EqualFold(rcpt, s.from):
			log.Printf("[SMTP] Skipped store (self-loop, rcpt==from): %s", rcpt)
		case rcptIsLocal:
			// The account's stored spelling keys the row, so a message
			// addressed to <Ali@x.com> lands in the same mailbox IMAP opens
			// for <ali@x.com> instead of a set of rows nothing can reach.
			if key := "in:" + strings.ToLower(rcptAccount.Email); !seen[key] {
				seen[key] = true
				plan.inbox = append(plan.inbox, rcptAccount.Email)
			}
		default:
			// Only an authenticated local sender reaches here. A role address
			// on a configured domain is local even without a mailbox, so it is
			// never sent outward: that would hand the message straight back.
			if key := "out:" + strings.ToLower(rcpt); !seen[key] {
				seen[key] = true
				plan.outbound = append(plan.outbound, rcpt)
			}
		}
	}

	plan.sent = senderIsLocal && len(s.recipients) > 0
	return plan, nil
}

// storeRaw persists the verbatim message and returns its content address.
//
// A blob failure is logged and swallowed: the message still lands in the
// mailbox with its parsed body, and IMAP falls back to the synthesized form
// used for pre-blob rows. Losing a delivery because a disk write failed would
// be a far worse outcome than losing attachment fidelity on one message.
func (s *Session) storeRaw(body []byte) string {
	if s.backend.blobs == nil {
		return ""
	}
	ref, err := s.backend.blobs.Put(body)
	if err != nil {
		log.Printf("[SMTP] Raw message store failed (sender=%s size=%d): %v", s.from, len(body), err)
		return ""
	}
	return ref
}

// Reset is called between transactions (RSET command).
func (s *Session) Reset() {
	s.from = ""
	s.recipients = nil
}

// Logout is called when the connection is closed.
// Releases the limiter handle acquired in NewSession.
func (s *Session) Logout() error {
	s.limit.Release()
	return nil
}

// --- Helpers ---

func extractIP(addr string) string {
	// Remove port from address (e.g., "192.168.1.1:12345" -> "192.168.1.1")
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		// Handle IPv6 addresses like "[::1]:12345"
		if strings.Contains(addr, "[") {
			return strings.Trim(addr[:idx], "[]")
		}
		return addr[:idx]
	}
	return addr
}

// isRoleAddress reports whether to is one of the RFC 5321 role addresses that
// must be reachable without an account, on a domain this server is configured
// to carry.
//
// The domain check is the point. Without it the bypass accepted mail for
// postmaster@anything, so any sender could have rows written under an address
// belonging to a domain the server does not host, on a mailbox nobody can open:
// unbounded disk growth behind a trivially discoverable open acceptor.
func (s *Session) isRoleAddress(to string) bool {
	switch address.Local(to) {
	case "postmaster", "hostmaster":
	default:
		return false
	}
	_, ok := s.backend.getConfig().MatchDomain(address.Domain(to))
	return ok
}
