package smtp

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"odac-mail/config"
)

// clientTimeout is the default timeout for SMTP operations.
const clientTimeout = 30 * time.Second

// Client handles outbound SMTP delivery with PTR-based source IP selection,
// IPv6-first fallback, MX resolution with caching, connection pooling,
// and DKIM signing. Mirrors the Node.js smtp.js architecture exactly.
type Client struct {
	connPool   sync.Map // map[string]*pooledConn
	dkimSigner interface{ Sign(string, []byte) ([]byte, error) }
	getConfig  func() config.Config
	maxRetries int
	mxCache    sync.Map // map[string]*mxCacheEntry
	mu         sync.Mutex
	ports      []int
	rateLimit  sync.Map // map[string]*rateLimitEntry
}

type pooledConn struct {
	conn     net.Conn
	lastUsed time.Time
}

type mxCacheEntry struct {
	host      string
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

var (
	clientInstance *Client
	clientOnce     sync.Once
)

// InitClient initializes the singleton SMTP client with the config provider.
func InitClient(getConfig func() config.Config, dkimSigner interface{ Sign(string, []byte) ([]byte, error) }) {
	clientOnce.Do(func() {
		clientInstance = &Client{
			dkimSigner: dkimSigner,
			getConfig:  getConfig,
			maxRetries: 3,
			ports:      []int{25, 587, 465, 2525},
		}
		// Periodic cleanup of stale connections and caches
		go clientInstance.cleanupLoop()
	})
}

// GetClient returns the singleton SMTP client instance.
func GetClient() *Client {
	return clientInstance
}

// Send delivers an email to a single recipient using MX resolution,
// PTR-based source IP selection, and multi-port fallback.
func (c *Client) Send(from, to string, body []byte) error {
	domain := to[strings.LastIndex(to, "@")+1:]

	// Rate limiting per domain
	if err := c.checkRateLimit(domain); err != nil {
		return err
	}

	// DKIM sign the message before delivery
	if c.dkimSigner != nil {
		signed, err := c.dkimSigner.Sign(from, body)
		if err == nil && len(signed) > 0 {
			body = signed
		}
	}

	// Resolve MX host
	host, err := c.resolveMX(domain)
	if err != nil {
		return fmt.Errorf("MX resolution failed for %s: %w", domain, err)
	}

	senderDomain := from[strings.LastIndex(from, "@")+1:]
	// Use mail subdomain as EHLO hostname to match PTR record (RFC 5321)
	ehloBase := "mail." + senderDomain

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			log.Printf("[SMTP-Client] Retry %d/%d for %s (waiting %v)", attempt, c.maxRetries, to, delay)
			time.Sleep(delay)
		}

		lastErr = c.sendToHost(from, to, body, host, ehloBase, false)
		if lastErr == nil {
			return nil
		}

		// If IPv6 network error, retry with IPv4 forced
		if isIPv6NetworkError(lastErr) {
			log.Printf("[SMTP-Client] IPv6 failed, falling back to IPv4 for %s", host)
			lastErr = c.sendToHost(from, to, body, host, ehloBase, true)
			if lastErr == nil {
				return nil
			}
		}
	}

	return fmt.Errorf("delivery to %s failed after %d attempts: %w", to, c.maxRetries, lastErr)
}

// sendToHost attempts delivery to a specific MX host, trying all configured ports.
func (c *Client) sendToHost(from, to string, body []byte, host, ehloBase string, forceIPv4 bool) error {
	cfg := c.getConfig()

	// Check if target supports IPv6
	targetIPv6 := false
	if !forceIPv4 {
		targetIPv6 = hostSupportsIPv6(host)
	}

	// Resolve best local IP via PTR matching (IPv6 priority)
	local := c.getLocalAddress(ehloBase, targetIPv6, cfg.IPs)

	var lastErr error
	for _, port := range c.ports {
		conn, reused, err := c.connect(local, host, port)
		if err != nil {
			lastErr = err
			continue
		}

		err = c.deliverMessage(conn, local.EHLO, from, to, body, host, reused)
		if err != nil {
			conn.Close()
			lastErr = err
			continue
		}

		// Return connection to pool
		c.returnToPool(host, port, conn)
		log.Printf("[SMTP-Client] Delivered: %s -> %s via %s:%d", from, to, host, port)
		return nil
	}

	return fmt.Errorf("all ports failed for %s: %w", host, lastErr)
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
//
// This is a direct port of the Node.js smtp.js #getLocalAddressForDomain method.
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

// connect establishes a TCP or TLS connection to the target SMTP server.
// Uses the resolved local address for source IP binding.
func (c *Client) connect(local LocalAddress, host string, port int) (net.Conn, bool, error) {
	// Check pool first
	key := fmt.Sprintf("%s:%d", host, port)
	if val, ok := c.connPool.Load(key); ok {
		pc := val.(*pooledConn)
		if time.Since(pc.lastUsed) < 5*time.Minute {
			// Test if connection is still alive
			pc.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
			buf := make([]byte, 1)
			_, err := pc.conn.Read(buf)
			pc.conn.SetReadDeadline(time.Time{})
			if err == nil || isTimeoutError(err) {
				log.Printf("[SMTP-Client] Reusing pooled connection to %s", key)
				return pc.conn, true, nil
			}
			pc.conn.Close()
		} else {
			pc.conn.Close()
		}
		c.connPool.Delete(key)
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	dialer := &net.Dialer{Timeout: clientTimeout}

	if local.Address != "" {
		// Bind to specific local IP for PTR matching
		if strings.Contains(local.Address, ":") {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(local.Address)}
		} else {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(local.Address)}
		}
	}

	if port == 465 {
		// Implicit TLS
		tlsConn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		})
		if err != nil {
			return nil, false, fmt.Errorf("TLS connect to %s failed: %w", addr, err)
		}
		return tlsConn, false, nil
	}

	// Plaintext connection (STARTTLS will be attempted during delivery)
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return nil, false, fmt.Errorf("connect to %s failed: %w", addr, err)
	}
	return conn, false, nil
}

// deliverMessage performs the SMTP transaction: EHLO, STARTTLS, MAIL FROM, RCPT TO, DATA.
// mxHost is the MX hostname used as TLS ServerName for certificate validation.
func (c *Client) deliverMessage(conn net.Conn, ehlo, from, to string, body []byte, mxHost string, reused bool) error {
	if !reused {
		// Read greeting
		if _, err := c.readResponse(conn); err != nil {
			return fmt.Errorf("greeting failed: %w", err)
		}
	}

	// EHLO
	resp, err := c.command(conn, fmt.Sprintf("EHLO %s\r\n", sanitize(ehlo)))
	if err != nil {
		return fmt.Errorf("EHLO failed: %w", err)
	}

	// Attempt STARTTLS if available and not already TLS
	if _, isTLS := conn.(*tls.Conn); !isTLS && strings.Contains(resp, "STARTTLS") {
		_, err := c.command(conn, "STARTTLS\r\n")
		if err == nil {
			tlsConn := tls.Client(conn, &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: mxHost,
			})
			if err := tlsConn.Handshake(); err != nil {
				log.Printf("[SMTP-Client] STARTTLS handshake failed for %s: %v", mxHost, err)
				return fmt.Errorf("STARTTLS handshake failed: %w", err)
			}
			conn = tlsConn
			// Re-EHLO after STARTTLS
			if _, err := c.command(conn, fmt.Sprintf("EHLO %s\r\n", sanitize(ehlo))); err != nil {
				return fmt.Errorf("post-STARTTLS EHLO failed: %w", err)
			}
		}
	}

	// MAIL FROM
	resp, err = c.command(conn, fmt.Sprintf("MAIL FROM:<%s>\r\n", sanitize(from)))
	if err != nil || !strings.HasPrefix(resp, "2") {
		return fmt.Errorf("MAIL FROM rejected: %s", resp)
	}

	// RCPT TO
	resp, err = c.command(conn, fmt.Sprintf("RCPT TO:<%s>\r\n", sanitize(to)))
	if err != nil || !strings.HasPrefix(resp, "2") {
		return fmt.Errorf("RCPT TO rejected: %s", resp)
	}

	// DATA
	resp, err = c.command(conn, "DATA\r\n")
	if err != nil || (!strings.HasPrefix(resp, "2") && !strings.HasPrefix(resp, "3")) {
		return fmt.Errorf("DATA rejected: %s", resp)
	}

	// Send body + terminator
	conn.SetWriteDeadline(time.Now().Add(clientTimeout))
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("body write failed: %w", err)
	}
	resp, err = c.command(conn, "\r\n.\r\n")
	if err != nil || !strings.HasPrefix(resp, "2") {
		return fmt.Errorf("message rejected: %s", resp)
	}

	return nil
}

// command sends an SMTP command and reads the response.
func (c *Client) command(conn net.Conn, cmd string) (string, error) {
	conn.SetWriteDeadline(time.Now().Add(clientTimeout))
	if _, err := io.WriteString(conn, cmd); err != nil {
		return "", err
	}
	return c.readResponse(conn)
}

// readResponse reads a multi-line SMTP response.
func (c *Client) readResponse(conn net.Conn) (string, error) {
	conn.SetReadDeadline(time.Now().Add(clientTimeout))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// returnToPool stores a connection for reuse.
func (c *Client) returnToPool(host string, port int, conn net.Conn) {
	key := fmt.Sprintf("%s:%d", host, port)
	c.connPool.Store(key, &pooledConn{conn: conn, lastUsed: time.Now()})
}

// resolveMX resolves the MX host for a domain with 1-hour caching.
func (c *Client) resolveMX(domain string) (string, error) {
	// Check cache
	if val, ok := c.mxCache.Load(domain); ok {
		entry := val.(*mxCacheEntry)
		if time.Since(entry.timestamp) < time.Hour {
			return entry.host, nil
		}
		c.mxCache.Delete(domain)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resolver := &net.Resolver{}
	records, err := resolver.LookupMX(ctx, domain)
	if err != nil || len(records) == 0 {
		return "", fmt.Errorf("no MX records for %s", domain)
	}

	// MX records are already sorted by preference by Go's resolver
	host := strings.TrimSuffix(records[0].Host, ".")

	c.mxCache.Store(domain, &mxCacheEntry{host: host, timestamp: time.Now()})
	log.Printf("[SMTP-Client] MX for %s: %s", domain, host)
	return host, nil
}

// checkRateLimit enforces per-domain hourly rate limiting (1000/hour).
func (c *Client) checkRateLimit(domain string) error {
	val, _ := c.rateLimit.LoadOrStore(domain, &rateLimitEntry{})
	entry := val.(*rateLimitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	hourAgo := now.Add(-time.Hour)

	// Remove old timestamps
	filtered := entry.timestamps[:0]
	for _, ts := range entry.timestamps {
		if ts.After(hourAgo) {
			filtered = append(filtered, ts)
		}
	}
	entry.timestamps = filtered

	if len(entry.timestamps) >= 1000 {
		return fmt.Errorf("rate limit exceeded for domain %s", domain)
	}

	entry.timestamps = append(entry.timestamps, now)
	return nil
}

// cleanupLoop periodically cleans stale connections and caches.
func (c *Client) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		// Clean connection pool
		c.connPool.Range(func(key, val any) bool {
			pc := val.(*pooledConn)
			if time.Since(pc.lastUsed) > 5*time.Minute {
				pc.conn.Close()
				c.connPool.Delete(key)
			}
			return true
		})

		// Clean MX cache (entries older than 1 hour)
		c.mxCache.Range(func(key, val any) bool {
			entry := val.(*mxCacheEntry)
			if time.Since(entry.timestamp) > time.Hour {
				c.mxCache.Delete(key)
			}
			return true
		})
	}
}

// Stop cleans up all connections and caches.
func (c *Client) Stop() {
	c.connPool.Range(func(key, val any) bool {
		pc := val.(*pooledConn)
		pc.conn.Close()
		c.connPool.Delete(key)
		return true
	})
	c.mxCache = sync.Map{}
	c.rateLimit = sync.Map{}
	log.Println("[SMTP-Client] Stopped, all connections and caches cleared")
}

// --- Network Helpers ---

// hostSupportsIPv6 checks if the target host has AAAA records.
func hostSupportsIPv6(host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resolver := &net.Resolver{}
	addrs, err := resolver.LookupIPAddr(ctx, host)
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

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
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

func extractHostFromConn(conn net.Conn) string {
	addr := conn.RemoteAddr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
