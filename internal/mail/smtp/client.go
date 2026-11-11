package smtp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac/internal/mail/address"
	"odac/internal/mail/config"
)

const (
	dialTimeout = 30 * time.Second
	// commandTimeout and dataTimeout follow RFC 5321 section 4.5.3.2. They are
	// long because a delivery runs on a queue worker, never on a request path.
	commandTimeout = 5 * time.Minute
	dataTimeout    = 10 * time.Minute
	// maxReplyBytes caps everything one remote server may send in a session.
	// textproto reads a line of any length, and a session's replies (greeting,
	// EHLO list, a few status lines) come nowhere near this.
	maxReplyBytes = 1 << 20
	mxCacheTTL    = time.Hour
	// rateLimitPerHour bounds deliveries to one destination domain.
	rateLimitPerHour = 1000
)

// Signer DKIM-signs an outgoing message for its sender's domain.
type Signer interface {
	Sign(from string, body []byte) ([]byte, error)
}

// Client delivers one message to one remote recipient per call, with
// PTR-based source IP selection, an IPv4 fallback, MX resolution with caching
// and DKIM signing. It holds no connections between calls: each delivery dials
// its own session and closes it, so two deliveries never share a socket.
type Client struct {
	dial       func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error)
	getConfig  func() config.Config
	lookupHost func(ctx context.Context, host string) ([]string, error)
	lookupIP   func(ctx context.Context, host string) ([]net.IPAddr, error)
	lookupMX   func(ctx context.Context, domain string) ([]*net.MX, error)
	mxCache    sync.Map // domain -> *mxCacheEntry
	port       int
	rateLimit  sync.Map // domain -> *rateLimitEntry
	signer     Signer
}

type mxCacheEntry struct {
	hosts     []string
	timestamp time.Time
}

type rateLimitEntry struct {
	mu         sync.Mutex
	timestamps []time.Time
}

// LocalAddress holds the resolved local IP and EHLO hostname for outbound delivery.
type LocalAddress struct {
	Address string // Local IP to bind (empty = OS default)
	EHLO    string // EHLO hostname (from PTR or domain)
}

// NewClient creates an outbound client. signer may be nil.
func NewClient(getConfig func() config.Config, signer Signer) *Client {
	return &Client{
		dial: func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		getConfig:  getConfig,
		lookupHost: net.DefaultResolver.LookupHost,
		lookupIP:   net.DefaultResolver.LookupIPAddr,
		lookupMX:   net.DefaultResolver.LookupMX,
		port:       25,
		signer:     signer,
	}
}

// permanentError is a failure no retry can fix, such as a domain that does not
// exist. Status is the RFC 3463 code reported in the bounce.
type permanentError struct {
	status string
	msg    string
}

func (e *permanentError) Error() string { return e.msg }

// Delivery stages, named in errors and used to classify refusals.
const (
	stageGreeting = "greeting"
	stageHello    = "EHLO"
	stageTLS      = "STARTTLS"
	stageMail     = "MAIL FROM"
	stageRcpt     = "RCPT TO"
	stageData     = "DATA"
)

// replyError is a refusal the remote server answered with.
type replyError struct {
	code  int
	host  string
	msg   string
	stage string
}

func (e *replyError) Error() string {
	return fmt.Sprintf("%s refused %s: %d %s", e.host, e.stage, e.code, strings.ReplaceAll(e.msg, "\n", " "))
}

// permanent reports whether the refusal is the destination's final answer.
// A 5xx before MAIL FROM is about this host (a greeting that bans our IP, a
// broken TLS stack), so another MX may still take the message. RFC 5321
// section 4.5.3.1.10 asks clients to treat 552 at RCPT as temporary.
func (e *replyError) permanent() bool {
	if e.code < 500 {
		return false
	}
	switch e.stage {
	case stageGreeting, stageHello, stageTLS:
		return false
	case stageRcpt:
		return e.code != 552
	}
	return true
}

// status is the reply's RFC 3463 enhanced code, or a generic one.
func (e *replyError) status() string {
	if code, _, ok := strings.Cut(e.msg, " "); ok && isEnhancedCode(code) {
		return code
	}
	if e.code >= 500 {
		return "5.0.0"
	}
	return "4.0.0"
}

func isEnhancedCode(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || (parts[0] != "2" && parts[0] != "4" && parts[0] != "5") {
		return false
	}
	for _, p := range parts[1:] {
		if _, err := strconv.Atoi(p); err != nil || len(p) > 3 {
			return false
		}
	}
	return true
}

// IsPermanent reports whether a delivery error means the message can never be
// delivered, so the queue bounces it instead of trying again.
func IsPermanent(err error) bool {
	var pe *permanentError
	if errors.As(err, &pe) {
		return true
	}
	var re *replyError
	return errors.As(err, &re) && re.permanent()
}

// failureStatus is the RFC 3463 status a bounce reports for err.
func failureStatus(err error) string {
	var pe *permanentError
	if errors.As(err, &pe) {
		return pe.status
	}
	var re *replyError
	if errors.As(err, &re) {
		return re.status()
	}
	return "4.4.1"
}

// Send delivers body from one sender to one remote recipient: every MX host in
// preference order, on port 25. It makes one attempt; retrying is the queue's
// job. The error tells IsPermanent whether another attempt can succeed.
func (c *Client) Send(ctx context.Context, from, to string, body []byte) error {
	domain := strings.ToLower(address.Domain(to))
	if domain == "" {
		return &permanentError{status: "5.1.3", msg: fmt.Sprintf("invalid recipient address %q", to)}
	}

	if err := c.checkRateLimit(domain); err != nil {
		return err
	}

	if c.signer != nil {
		if signed, err := c.signer.Sign(from, body); err == nil && len(signed) > 0 {
			body = signed
		}
	}

	hosts, err := c.resolveMX(ctx, domain)
	if err != nil {
		return err
	}

	// Use the mail subdomain as EHLO hostname to match the PTR record (RFC 5321).
	ehloBase := "mail." + address.Domain(from)

	var lastErr error
	for _, host := range hosts {
		err := c.sendToHost(ctx, from, to, body, host, ehloBase, false)
		if err != nil && isIPv6NetworkError(err) {
			log.Printf("[SMTP-Client] IPv6 failed, falling back to IPv4 for %s", host)
			err = c.sendToHost(ctx, from, to, body, host, ehloBase, true)
		}
		if err == nil {
			log.Printf("[SMTP-Client] Delivered: %s -> %s via %s", from, to, host)
			return nil
		}
		// A permanent refusal from one MX is the domain's answer: the other
		// hosts serve the same mailboxes.
		if IsPermanent(err) || ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// sendToHost runs one SMTP session against host.
func (c *Client) sendToHost(ctx context.Context, from, to string, body []byte, host, ehloBase string, forceIPv4 bool) error {
	targetIPv6 := !forceIPv4 && c.hostSupportsIPv6(ctx, host)
	local := c.getLocalAddress(ehloBase, targetIPv6, c.getConfig().IPs)

	dialer := &net.Dialer{Timeout: dialTimeout}
	if local.Address != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(local.Address)}
	}
	network := "tcp"
	if !targetIPv6 {
		network = "tcp4"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(c.port))
	conn, err := c.dial(ctx, dialer, network, addr)
	if err != nil {
		return fmt.Errorf("connect to %s failed: %w", addr, err)
	}
	// Closing the socket is the only way to interrupt a blocked read, which
	// is what lets Stop end a delivery stuck on a slow server.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	s := newSession(conn, host)
	defer s.close()
	return s.deliver(sanitize(local.EHLO), from, to, body)
}

// session is one SMTP conversation with a remote server.
type session struct {
	conn net.Conn
	ext  map[string]bool
	host string
	text *textproto.Conn
}

func newSession(conn net.Conn, host string) *session {
	s := &session{host: host}
	s.setConn(conn)
	return s
}

func (s *session) setConn(conn net.Conn) {
	s.conn = conn
	s.text = textproto.NewConn(struct {
		io.Reader
		io.Writer
		io.Closer
	}{io.LimitReader(conn, maxReplyBytes), conn, conn})
}

func (s *session) close() { s.text.Close() }

// reply reads one, possibly multi-line, response and checks its code against
// expect, which may be a prefix ("25" accepts 250 and 251).
func (s *session) reply(stage string, expect int, timeout time.Duration) (string, error) {
	s.conn.SetDeadline(time.Now().Add(timeout))
	_, msg, err := s.text.ReadResponse(expect)
	var te *textproto.Error
	if errors.As(err, &te) {
		return "", &replyError{code: te.Code, host: s.host, msg: te.Msg, stage: stage}
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", s.host, stage, err)
	}
	return msg, nil
}

func (s *session) cmd(stage string, expect int, format string, args ...any) (string, error) {
	s.conn.SetDeadline(time.Now().Add(commandTimeout))
	id, err := s.text.Cmd(format, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", s.host, stage, err)
	}
	s.text.StartResponse(id)
	defer s.text.EndResponse(id)
	return s.reply(stage, expect, commandTimeout)
}

// hello sends EHLO and records the extensions, falling back to HELO for a
// server that does not know EHLO.
func (s *session) hello(name string) error {
	msg, err := s.cmd(stageHello, 250, "EHLO %s", name)
	var re *replyError
	if errors.As(err, &re) && (re.code == 500 || re.code == 502) {
		s.ext = map[string]bool{}
		_, err = s.cmd(stageHello, 250, "HELO %s", name)
		return err
	}
	if err != nil {
		return err
	}
	s.ext = map[string]bool{}
	lines := strings.Split(msg, "\n")
	for _, line := range lines[1:] {
		keyword, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		s.ext[strings.ToUpper(keyword)] = true
	}
	return nil
}

func (s *session) deliver(ehlo, from, to string, body []byte) error {
	if _, err := s.reply(stageGreeting, 220, commandTimeout); err != nil {
		return err
	}
	if err := s.hello(ehlo); err != nil {
		return err
	}

	if s.ext["STARTTLS"] {
		if _, err := s.cmd(stageTLS, 220, "STARTTLS"); err != nil {
			return err
		}
		tlsConn := tls.Client(s.conn, opportunisticTLS(s.host))
		s.conn.SetDeadline(time.Now().Add(commandTimeout))
		if err := tlsConn.Handshake(); err != nil {
			log.Printf("[SMTP-Client] STARTTLS handshake failed for %s: %v", s.host, err)
			return fmt.Errorf("%s STARTTLS handshake failed: %w", s.host, err)
		}
		// Everything from here on, the reader included, runs over TLS.
		s.setConn(tlsConn)
		if err := s.hello(ehlo); err != nil {
			return err
		}
	}

	param := ""
	if s.ext["8BITMIME"] {
		param = " BODY=8BITMIME"
	}
	if _, err := s.cmd(stageMail, 250, "MAIL FROM:<%s>%s", sanitize(from), param); err != nil {
		return err
	}
	if _, err := s.cmd(stageRcpt, 25, "RCPT TO:<%s>", sanitize(to)); err != nil {
		return err
	}
	if _, err := s.cmd(stageData, 354, "DATA"); err != nil {
		return err
	}

	// The dot writer applies RFC 5321 section 4.5.2: CRLF line endings,
	// dot-stuffing of lines starting with ".", and the final "." line.
	s.conn.SetDeadline(time.Now().Add(dataTimeout))
	w := s.text.DotWriter()
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("%s body write failed: %w", s.host, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%s body write failed: %w", s.host, err)
	}
	if _, err := s.reply(stageData, 250, dataTimeout); err != nil {
		return err
	}

	// The message is accepted; a failed QUIT changes nothing about that.
	s.cmd("QUIT", 221, "QUIT")
	return nil
}

// opportunisticTLS is the STARTTLS configuration for MX delivery (RFC 7435),
// matching the universal MTA-to-MTA relay default (e.g. Postfix
// smtp_tls_security_level=may): always encrypt, but never let an unverifiable
// peer certificate block delivery. The destination MX comes from
// unauthenticated DNS and the only alternatives are plaintext (worse) or
// non-delivery (worst).
//
// Verification still runs against the system roots, for observability (a
// stale CA bundle or a peer chain change shows in the logs) and as the hook a
// future MTA-STS/DANE policy would turn into enforcement.
func opportunisticTLS(mxHost string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         mxHost,
		InsecureSkipVerify: true, // opportunistic; verification handled below, non-fatal
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			inter := x509.NewCertPool()
			for _, peer := range cs.PeerCertificates[1:] {
				inter.AddCert(peer)
			}
			// Nil Roots makes Verify use the cached system pool directly.
			if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				DNSName:       mxHost,
				Intermediates: inter,
			}); err != nil {
				log.Printf("[SMTP-Client] TLS to %s unverified, delivering opportunistically: %v", mxHost, err)
			}
			return nil
		},
	}
}

// getLocalAddress resolves the best local IP for outbound delivery using PTR matching.
// Priority (when target supports IPv6):
//  1. PTR-matched IPv6
//  2. PTR-matched IPv4
//  3. First public IPv6
//  4. First public IPv4
//
// Priority (when target is IPv4 only):
//  1. PTR-matched IPv4
//  2. First public IPv4
func (c *Client) getLocalAddress(domain string, targetIPv6 bool, ips config.IPConfig) LocalAddress {
	// Extract root domain for broader PTR matching (mail.example.com → example.com)
	rootDomain := domain
	if idx := strings.Index(domain, "."); idx >= 0 {
		rootDomain = domain[idx+1:]
	}
	if rootDomain == "" {
		rootDomain = domain
	}

	// 1. Find IPv6 with PTR matching this domain (highest priority, if target supports)
	if targetIPv6 {
		for _, ip := range ips.IPv6 {
			if !ip.Public || ip.PTR == "" {
				continue
			}
			if ptrMatchesDomain(ip.PTR, domain, rootDomain) {
				log.Printf("[SMTP-Client] Using PTR-matched IPv6 %s (%s) for %s", ip.Address, ip.PTR, domain)
				return LocalAddress{Address: ip.Address, EHLO: ip.PTR}
			}
		}
	}

	// 2. Find IPv4 with PTR matching this domain
	for _, ip := range ips.IPv4 {
		if !ip.Public || ip.PTR == "" {
			continue
		}
		if ptrMatchesDomain(ip.PTR, domain, rootDomain) {
			log.Printf("[SMTP-Client] Using PTR-matched IPv4 %s (%s) for %s", ip.Address, ip.PTR, domain)
			return LocalAddress{Address: ip.Address, EHLO: ip.PTR}
		}
	}

	// 3. First public IPv6 (no PTR match, if target supports)
	if targetIPv6 {
		for _, ip := range ips.IPv6 {
			if ip.Public {
				ehlo := domain
				if ip.PTR != "" {
					ehlo = ip.PTR
				}
				log.Printf("[SMTP-Client] Using default public IPv6 %s for %s", ip.Address, domain)
				return LocalAddress{Address: ip.Address, EHLO: ehlo}
			}
		}
	}

	// 4. First public IPv4 (no PTR match)
	for _, ip := range ips.IPv4 {
		if ip.Public {
			ehlo := domain
			if ip.PTR != "" {
				ehlo = ip.PTR
			}
			log.Printf("[SMTP-Client] Using default public IPv4 %s for %s", ip.Address, domain)
			return LocalAddress{Address: ip.Address, EHLO: ehlo}
		}
	}

	// Fallback to primary IP
	if ips.Primary != "" && ips.Primary != "127.0.0.1" {
		return LocalAddress{Address: ips.Primary, EHLO: domain}
	}

	return LocalAddress{EHLO: domain}
}

// ptrMatchesDomain checks if a PTR record matches the sender domain.
// Matches: exact, suffix (.rootDomain), or domain suffix (.ptr).
func ptrMatchesDomain(ptr, domain, rootDomain string) bool {
	return ptr == domain ||
		strings.HasSuffix(ptr, "."+rootDomain) ||
		strings.HasSuffix(domain, "."+ptr)
}

// resolveMX returns the domain's mail hosts in preference order, cached for
// an hour. A domain with no MX record falls back to its own address (RFC 5321
// section 5.1); a domain that does not exist, or publishes a null MX (RFC
// 7505), is a permanent failure. Any other lookup error is temporary.
func (c *Client) resolveMX(ctx context.Context, domain string) ([]string, error) {
	if val, ok := c.mxCache.Load(domain); ok {
		entry := val.(*mxCacheEntry)
		if time.Since(entry.timestamp) < mxCacheTTL {
			return entry.hosts, nil
		}
		c.mxCache.Delete(domain)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	records, err := c.lookupMX(lookupCtx, domain)
	if err != nil && !isNotFound(err) {
		return nil, fmt.Errorf("MX lookup for %s failed: %w", domain, err)
	}

	var hosts []string
	switch {
	case len(records) == 0:
		if _, herr := c.lookupHost(lookupCtx, domain); herr != nil {
			if isNotFound(herr) {
				return nil, &permanentError{status: "5.1.2", msg: fmt.Sprintf("domain %s has no MX or address records", domain)}
			}
			return nil, fmt.Errorf("address lookup for %s failed: %w", domain, herr)
		}
		hosts = []string{domain}
	case len(records) == 1 && strings.TrimSuffix(records[0].Host, ".") == "":
		return nil, &permanentError{status: "5.1.10", msg: fmt.Sprintf("domain %s accepts no mail (null MX)", domain)}
	default:
		// Go's resolver sorts by preference and shuffles equal preferences.
		for _, r := range records {
			if h := strings.TrimSuffix(r.Host, "."); h != "" {
				hosts = append(hosts, h)
			}
		}
	}

	c.mxCache.Store(domain, &mxCacheEntry{hosts: hosts, timestamp: time.Now()})
	log.Printf("[SMTP-Client] MX for %s: %s", domain, strings.Join(hosts, ", "))
	return hosts, nil
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// checkRateLimit enforces the per-domain hourly delivery limit. Hitting it is
// a temporary failure: the queue holds the message and tries again later.
func (c *Client) checkRateLimit(domain string) error {
	val, _ := c.rateLimit.LoadOrStore(domain, &rateLimitEntry{})
	entry := val.(*rateLimitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	entry.timestamps = pruneBefore(entry.timestamps, now.Add(-time.Hour))

	if len(entry.timestamps) >= rateLimitPerHour {
		return fmt.Errorf("rate limit exceeded for domain %s", domain)
	}

	entry.timestamps = append(entry.timestamps, now)
	return nil
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// Prune drops expired MX entries and idle rate-limit windows, so both maps
// stay bounded by the domains mailed within the last hour. The queue calls it
// from its own loop, which is why the client runs no goroutine of its own.
func (c *Client) Prune() {
	now := time.Now()
	c.mxCache.Range(func(key, val any) bool {
		if now.Sub(val.(*mxCacheEntry).timestamp) > mxCacheTTL {
			c.mxCache.Delete(key)
		}
		return true
	})
	c.rateLimit.Range(func(key, val any) bool {
		entry := val.(*rateLimitEntry)
		entry.mu.Lock()
		entry.timestamps = pruneBefore(entry.timestamps, now.Add(-time.Hour))
		idle := len(entry.timestamps) == 0
		entry.mu.Unlock()
		if idle {
			c.rateLimit.Delete(key)
		}
		return true
	})
}

// --- Network Helpers ---

// hostSupportsIPv6 checks if the target host has AAAA records.
func (c *Client) hostSupportsIPv6(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	addrs, err := c.lookupIP(ctx, host)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if addr.IP.To4() == nil && addr.IP.To16() != nil {
			return true
		}
	}
	return false
}

// isIPv6NetworkError checks if an error is an IPv6 connectivity issue.
func isIPv6NetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "address not available")
}

// sanitize prevents SMTP injection by removing CR/LF characters.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}
