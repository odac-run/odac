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

	"odac-mail/auth"
	"odac-mail/config"
	"odac-mail/limits"
	"odac-mail/storage"
)

// Backend implements smtp.Backend for the inbound SMTP server.
// Handles authentication, message reception, and local delivery.
//
// One Backend instance is bound to one listener (port 25 or 465) so the
// limiter and log tag reflect that listener's traffic in isolation.
type Backend struct {
	firewall  *auth.Firewall
	getConfig func() config.Config
	limiter   *limits.Limiter
	store     *storage.Store
	tag       string // "inbound" or "submission" — included in log lines
}

// NewBackend creates a new SMTP backend with the given dependencies.
func NewBackend(store *storage.Store, fw *auth.Firewall, getConfig func() config.Config, limiter *limits.Limiter, tag string) *Backend {
	return &Backend{
		firewall:  fw,
		getConfig: getConfig,
		limiter:   limiter,
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
		if !isValidEmail(username) {
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
		s.user = username
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
	if !isValidEmail(from) {
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
	if !isValidEmail(to) {
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

	localPart := to
	if idx := strings.Index(to, "@"); idx >= 0 {
		localPart = to[:idx]
	}

	if localPart == "postmaster" || localPart == "hostmaster" {
		s.recipients = append(s.recipients, to)
		log.Printf("[SMTP] RCPT TO <%s> accepted (postmaster/hostmaster) from=%s ip=%s", to, s.from, s.ip)
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
	const maxBody = 10 * 1024 * 1024 // 10MB limit
	body, err := io.ReadAll(io.LimitReader(r, maxBody))
	if err != nil {
		log.Printf("[SMTP] DATA read failed (read=%d sender=%s rcpts=%d ip=%s): %v",
			len(body), s.from, len(s.recipients), s.ip, err)
		return err
	}
	if len(body) == maxBody {
		log.Printf("[SMTP] DATA hit 10MB cap (truncated) sender=%s rcpts=%d ip=%s",
			s.from, len(s.recipients), s.ip)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Parse RFC 2822 message into headers and body parts
	parsed := parseMessage(body)
	log.Printf("[SMTP] DATA received: %d bytes sender=%s rcpts=%v ip=%s msg-id=%q subject=%q html=%dB text=%dB",
		len(body), s.from, s.recipients, s.ip,
		parsed.messageID, parsed.subject, len(parsed.html), len(parsed.text))

	sentStored := false
	storedCount := 0
	outboundCount := 0

	for _, rcpt := range s.recipients {
		// Check if sender is a local authenticated user
		senderIsLocal := s.user != "" && strings.EqualFold(s.from, s.user)

		// Check if recipient is a local account
		rcptAccount, lookupErr := s.backend.store.AccountExists(ctx, rcpt)
		if lookupErr != nil {
			log.Printf("[SMTP] DATA rcpt lookup error for %s: %v", rcpt, lookupErr)
		}
		rcptIsLocal := rcptAccount != nil

		// Also accept postmaster/hostmaster for any configured domain
		if !rcptIsLocal {
			localPart := strings.SplitN(rcpt, "@", 2)[0]
			rcptIsLocal = localPart == "postmaster" || localPart == "hostmaster"
		}

		log.Printf("[SMTP] DATA dispatch: rcpt=%s sender_local=%v rcpt_local=%v from=%s ip=%s",
			rcpt, senderIsLocal, rcptIsLocal, s.from, s.ip)

		// Reject if neither sender nor recipient is local
		if !senderIsLocal && !rcptIsLocal {
			log.Printf("[SMTP] Rejected relay attempt: %s -> %s from %s", s.from, rcpt, s.ip)
			return errors.New("relay access denied")
		}

		// Store locally for local recipients
		if rcptIsLocal && rcpt != s.from {
			msg := &storage.MessageRow{
				Email:       rcpt,
				Flags:       toNullString("[]"),
				From:        toNullString(parsed.from),
				Headers:     toNullString(parsed.headersJSON),
				HeaderLines: toNullString(parsed.headerLinesJSON),
				HTML:        toNullString(parsed.html),
				Mailbox:     "INBOX",
				MessageID:   toNullString(parsed.messageID),
				Subject:     toNullString(parsed.subject),
				Text:        toNullString(parsed.text),
				To:          toNullString(parsed.to),
			}
			if err := s.backend.store.MessageStore(ctx, msg); err != nil {
				log.Printf("[SMTP] Failed to store message for %s: %v", rcpt, err)
			} else {
				storedCount++
				log.Printf("[SMTP] Stored INBOX message: rcpt=%s msg-id=%q subject=%q",
					rcpt, parsed.messageID, parsed.subject)
			}
		} else if rcptIsLocal && rcpt == s.from {
			log.Printf("[SMTP] Skipped store (self-loop, rcpt==from): %s", rcpt)
		}

		// Outbound delivery for authenticated local senders
		if senderIsLocal && !rcptIsLocal {
			outboundCount++
			go func(recipient string, data []byte) {
				if err := GetClient().Send(s.from, recipient, data); err != nil {
					log.Printf("[SMTP] Outbound delivery failed: %s -> %s: %v", s.from, recipient, err)
				}
			}(rcpt, body)
		}

		// Store once in Sent folder for authenticated local senders
		if senderIsLocal && !sentStored {
			sentStored = true
			sentMsg := &storage.MessageRow{
				Email:       s.from,
				Flags:       toNullString(`["seen"]`),
				From:        toNullString(parsed.from),
				Headers:     toNullString(parsed.headersJSON),
				HeaderLines: toNullString(parsed.headerLinesJSON),
				HTML:        toNullString(parsed.html),
				Mailbox:     "Sent",
				MessageID:   toNullString(parsed.messageID),
				Subject:     toNullString(parsed.subject),
				Text:        toNullString(parsed.text),
				To:          toNullString(parsed.to),
			}
			if err := s.backend.store.MessageStore(ctx, sentMsg); err != nil {
				log.Printf("[SMTP] Failed to store sent message for %s: %v", s.from, err)
			} else {
				log.Printf("[SMTP] Stored Sent message: from=%s msg-id=%q", s.from, parsed.messageID)
			}
		}
	}

	log.Printf("[SMTP] DATA complete: stored=%d outbound=%d sender=%s rcpts=%d ip=%s",
		storedCount, outboundCount, s.from, len(s.recipients), s.ip)
	return nil
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

func isValidEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at < 1 || at >= len(email)-1 {
		return false
	}
	domain := email[at+1:]
	return len(domain) >= 3 && strings.Contains(domain, ".")
}

