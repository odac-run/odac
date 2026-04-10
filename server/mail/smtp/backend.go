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
	"odac-mail/storage"
)

// Backend implements smtp.Backend for the inbound SMTP server.
// Handles authentication, message reception, and local delivery.
type Backend struct {
	firewall  *auth.Firewall
	getConfig func() config.Config
	store     *storage.Store
}

// NewBackend creates a new SMTP backend with the given dependencies.
func NewBackend(store *storage.Store, fw *auth.Firewall, getConfig func() config.Config) *Backend {
	return &Backend{
		firewall:  fw,
		getConfig: getConfig,
		store:     store,
	}
}

// NewSession is called for each new SMTP connection.
// Checks IP blocklist before allowing the session to proceed.
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	ip := extractIP(c.Conn().RemoteAddr().String())
	if b.firewall.IsBlocked(ip) {
		return nil, errors.New("your IP is blocked due to suspicious activity")
	}

	return &Session{
		backend: b,
		ip:      ip,
	}, nil
}

// Session represents a single SMTP connection session.
// Tracks authentication state, sender, and recipients per transaction.
type Session struct {
	backend    *Backend
	from       string
	ip         string
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
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		account, err := s.backend.store.AccountExists(ctx, username)
		if err != nil || account == nil {
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		match, err := auth.ComparePassword(password, account.Password)
		if err != nil || !match {
			s.backend.firewall.HandleFailedAuth(s.ip)
			return errors.New("invalid username or password")
		}

		// Successful login — clear failed attempts
		s.backend.firewall.ClearAttempts(s.ip)
		s.user = username
		log.Printf("[SMTP] User authenticated: %s from %s", username, s.ip)
		return nil
	}), nil
}

// Mail is called for MAIL FROM command. Validates sender address format.
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	if !isValidEmail(from) {
		return errors.New("invalid email address")
	}
	s.from = from
	return nil
}

// Rcpt is called for RCPT TO command. Validates recipient address format.
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !isValidEmail(to) {
		return errors.New("invalid email address")
	}
	s.recipients = append(s.recipients, to)
	return nil
}

// Data is called when the message body is received.
// Stores the message locally for known recipients and triggers
// outbound delivery for authenticated senders.
func (s *Session) Data(r io.Reader) error {
	body, err := io.ReadAll(io.LimitReader(r, 10*1024*1024)) // 10MB limit
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, rcpt := range s.recipients {
		// Check if sender is a local authenticated user
		senderIsLocal := s.user != "" && s.from == s.user

		// Check if recipient is a local account
		rcptAccount, _ := s.backend.store.AccountExists(ctx, rcpt)
		rcptIsLocal := rcptAccount != nil

		// Also accept postmaster/hostmaster for any configured domain
		if !rcptIsLocal {
			localPart := strings.SplitN(rcpt, "@", 2)[0]
			rcptIsLocal = localPart == "postmaster" || localPart == "hostmaster"
		}

		// Reject if neither sender nor recipient is local
		if !senderIsLocal && !rcptIsLocal {
			log.Printf("[SMTP] Rejected relay attempt: %s -> %s from %s", s.from, rcpt, s.ip)
			return errors.New("relay access denied")
		}

		// Store locally for local recipients
		if rcptIsLocal {
			mailbox := "INBOX"
			flags := "[]"
			// If sender is sending to themselves, mark as Sent
			if rcpt == s.from {
				mailbox = "Sent"
				flags = `["seen"]`
			}

			msg := &storage.MessageRow{
				Email:   rcpt,
				Flags:   toNullString(flags),
				HTML:    toNullString(string(body)),
				Mailbox: mailbox,
				Subject: toNullString(extractSubject(body)),
			}
			if err := s.backend.store.MessageStore(ctx, msg); err != nil {
				log.Printf("[SMTP] Failed to store message for %s: %v", rcpt, err)
			}
		}

		// Outbound delivery for authenticated local senders
		if senderIsLocal && !rcptIsLocal {
			// Outbound delivery will be handled by the Client in a goroutine
			go func(recipient string, data []byte) {
				if err := GetClient().Send(s.from, recipient, data); err != nil {
					log.Printf("[SMTP] Outbound delivery failed: %s -> %s: %v", s.from, recipient, err)
				}
			}(rcpt, body)
		}
	}

	return nil
}

// Reset is called between transactions (RSET command).
func (s *Session) Reset() {
	s.from = ""
	s.recipients = nil
}

// Logout is called when the connection is closed.
func (s *Session) Logout() error {
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

func extractSubject(body []byte) string {
	// Simple header extraction — full MIME parsing deferred to Phase 3
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.ToLower(line), "subject:") {
			return strings.TrimSpace(line[8:])
		}
		if line == "" || line == "\r" {
			break // End of headers
		}
	}
	return ""
}
