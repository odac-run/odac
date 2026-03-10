package resolver

// Rate limiter for DNS queries using a lock-free sync.Map for O(1) per-IP
// tracking. Mirrors Node.js DNS.js rate limiting: 2500 requests/minute/IP
// with localhost bypass. A background goroutine periodically evicts stale
// entries to prevent unbounded memory growth.

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const (
	defaultRateLimit  = 2500        // Max requests per IP per window
	defaultWindowMs   = 60000       // 1 minute window in milliseconds
	cleanupInterval   = 2 * time.Minute
	maxTrackedIPs     = 100000      // Memory safety cap
)

// RateLimiter wraps the Resolver with per-IP request rate limiting.
// Implements dns.Handler so it can be used as middleware in the handler chain.
type RateLimiter struct {
	counts    sync.Map // IP string -> *ipCounter
	limit     int
	next      dns.Handler
	stop      chan struct{}
	totalIPs  atomic.Int64
	windowMs  int64
}

// ipCounter tracks request count and window start for a single IP.
// Packed into a single struct for cache-line efficiency.
type ipCounter struct {
	count      atomic.Int64
	firstReqMs atomic.Int64
}

// NewRateLimiter creates a rate limiter wrapping the given DNS handler.
// Starts a background cleanup goroutine for stale entry eviction.
func NewRateLimiter(next dns.Handler) *RateLimiter {
	rl := &RateLimiter{
		limit:    defaultRateLimit,
		next:     next,
		stop:     make(chan struct{}),
		windowMs: defaultWindowMs,
	}

	go rl.cleanupLoop()
	return rl
}

// Stop terminates the background cleanup goroutine.
// Must be called during graceful shutdown to prevent goroutine leaks.
func (rl *RateLimiter) Stop() {
	close(rl.stop)
}

// ServeDNS implements dns.Handler. Checks rate limits before delegating
// to the wrapped resolver. Localhost addresses bypass rate limiting.
func (rl *RateLimiter) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	clientIP := extractIP(w.RemoteAddr())

	// Bypass rate limiting for localhost/loopback (same as Node.js DNS.js)
	if clientIP == "127.0.0.1" || clientIP == "::1" || clientIP == "localhost" {
		rl.next.ServeDNS(w, req)
		return
	}

	now := time.Now().UnixMilli()

	// Load or create counter — lock-free O(1)
	val, loaded := rl.counts.LoadOrStore(clientIP, &ipCounter{})
	counter := val.(*ipCounter)

	if !loaded {
		// New entry
		counter.count.Store(1)
		counter.firstReqMs.Store(now)
		rl.totalIPs.Add(1)

		// Memory safety: if too many unique IPs, force cleanup
		if rl.totalIPs.Load() > maxTrackedIPs {
			go rl.forceCleanup()
		}

		rl.next.ServeDNS(w, req)
		return
	}

	// Existing entry — check window
	firstReq := counter.firstReqMs.Load()
	if now-firstReq > rl.windowMs {
		// Window expired — reset
		counter.count.Store(1)
		counter.firstReqMs.Store(now)
		rl.next.ServeDNS(w, req)
		return
	}

	// Increment and check limit
	count := counter.count.Add(1)
	if count > int64(rl.limit) {
		// Rate limited — send empty response (same as Node.js DNS.js)
		if count == int64(rl.limit)+1 {
			log.Printf("[DNS] Rate limit exceeded for %s", clientIP)
		}
		msg := new(dns.Msg)
		msg.SetRcode(req, dns.RcodeRefused)
		w.WriteMsg(msg)
		return
	}

	rl.next.ServeDNS(w, req)
}

// cleanupLoop periodically evicts expired IP counters to prevent memory leaks.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stop:
			return
		}
	}
}

// cleanup evicts all entries whose window has expired.
func (rl *RateLimiter) cleanup() {
	now := time.Now().UnixMilli()
	evicted := 0

	rl.counts.Range(func(key, value any) bool {
		counter := value.(*ipCounter)
		if now-counter.firstReqMs.Load() > rl.windowMs {
			rl.counts.Delete(key)
			rl.totalIPs.Add(-1)
			evicted++
		}
		return true
	})

	if evicted > 0 {
		log.Printf("[DNS] Rate limiter cleanup: evicted %d stale entries", evicted)
	}
}

// forceCleanup is called when memory safety cap is hit.
func (rl *RateLimiter) forceCleanup() {
	log.Printf("[DNS] Rate limiter: memory cap reached (%d IPs), forcing cleanup", rl.totalIPs.Load())
	rl.cleanup()
}

// extractIP extracts the IP address from a net.Addr, stripping the port.
func extractIP(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}

	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
