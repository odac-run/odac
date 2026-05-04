// Package limits implements hierarchical connection limiting for IMAP/SMTP.
//
// The limiter enforces four concurrent-connection ceilings plus a per-IP
// new-connection rate cap. Caps are layered so a single misbehaving client
// cannot exhaust shared capacity, while NAT/CG-NAT users remain unaffected:
//
//	per (user, IP)  — caps a single client device on one IP
//	per user        — caps an authenticated user across devices/IPs
//	per IP          — caps a NAT egress / shared office IP
//	global          — sane node-wide ceiling against runaway accept loops
//
// The per-(user,IP) and per-user counters are only meaningful after
// successful authentication. Accept-time gating uses per-IP, global, and
// the new-connection rate bucket.
package limits

import (
	"sync"

	"golang.org/x/time/rate"
)

// Profile defines the ceilings for a single protocol/port.
//
// Defaults follow Dovecot/Postfix sizing for a single-node enterprise
// deployment serving up to ~10K accounts. Operators that need different
// values can construct a Profile directly today; a config-driven path
// can be wired in later without changing call sites.
type Profile struct {
	MaxPerUserIP int     // concurrent connections from one (user, IP) pair
	MaxPerUser   int     // concurrent connections per authenticated user
	MaxPerIP     int     // concurrent connections per source IP
	MaxTotal     int     // concurrent connections per limiter (node-wide)
	NewConnPerIP float64 // new connections per second per IP (token bucket rate)
	NewConnBurst int     // token bucket burst size
}

// IMAPProfile is the default profile for IMAP (ports 143/993).
//
// Apple Mail opens 3-4 connections per account; an "iPhone + Mac + Web"
// combo lands around 12-15. 20 leaves headroom without enabling abuse.
// 50 per user covers ~10 devices × 5 clients; 500 per IP absorbs CG-NAT
// and small-office NAT egress; 10K global is a sane single-node cap.
func IMAPProfile() Profile {
	return Profile{
		MaxPerUserIP: 20,
		MaxPerUser:   50,
		MaxPerIP:     500,
		MaxTotal:     10000,
		NewConnPerIP: 10,
		NewConnBurst: 20,
	}
}

// SMTPSubmissionProfile is the default profile for SMTP submission (port 465).
// Mirrors IMAP since clients also multiplex submission across devices.
func SMTPSubmissionProfile() Profile {
	return Profile{
		MaxPerUserIP: 20,
		MaxPerUser:   50,
		MaxPerIP:     500,
		MaxTotal:     10000,
		NewConnPerIP: 10,
		NewConnBurst: 20,
	}
}

// SMTPInboundProfile is the default profile for inbound SMTP (port 25).
//
// Inbound is unauthenticated by design, so the post-auth caps never engage.
// Per-IP is held lower to slow down spam bursts before reputation services
// (firewall, DNSBL) kick in; legitimate sending MTAs rarely exceed 50 in
// flight from a single IP to a single destination.
func SMTPInboundProfile() Profile {
	return Profile{
		MaxPerUserIP: 0,
		MaxPerUser:   0,
		MaxPerIP:     200,
		MaxTotal:     10000,
		NewConnPerIP: 5,
		NewConnBurst: 10,
	}
}

// Limiter tracks live connection counts and per-IP arrival rate.
//
// All counters are guarded by a single mutex. The hot path is short
// (a handful of map lookups and integer compares) and contention has
// not been observed to matter at the scale this serves; if it ever
// does, the perIP/perUser maps can be sharded by hash without changing
// the public API.
type Limiter struct {
	profile Profile

	mu        sync.Mutex
	total     int
	perIP     map[string]int
	perUser   map[string]int
	perUserIP map[userIPKey]int
	rates     map[string]*rate.Limiter
}

type userIPKey struct {
	user string
	ip   string
}

// New constructs a Limiter for the given profile.
func New(p Profile) *Limiter {
	return &Limiter{
		profile:   p,
		perIP:     make(map[string]int),
		perUser:   make(map[string]int),
		perUserIP: make(map[userIPKey]int),
		rates:     make(map[string]*rate.Limiter),
	}
}

// Reason describes why an Acquire/BindUser call was rejected.
// Used for logging and protocol-level error responses.
type Reason int

const (
	ReasonOK Reason = iota
	ReasonRate
	ReasonPerIP
	ReasonTotal
	ReasonPerUser
	ReasonPerUserIP
)

func (r Reason) String() string {
	switch r {
	case ReasonRate:
		return "new-connection rate exceeded"
	case ReasonPerIP:
		return "per-IP connection limit exceeded"
	case ReasonTotal:
		return "global connection limit exceeded"
	case ReasonPerUser:
		return "per-user connection limit exceeded"
	case ReasonPerUserIP:
		return "per-(user,IP) connection limit exceeded"
	}
	return "ok"
}

// Handle is returned by a successful Acquire and tracks the live counters
// associated with one connection. Release MUST be called exactly once,
// typically via defer, regardless of authentication outcome.
type Handle struct {
	limiter *Limiter
	ip      string
	user    string // populated by BindUser
	closed  bool
}

// Acquire performs the accept-time gate: rate bucket, per-IP, global.
// On success it increments per-IP/global counters and returns a Handle
// that the caller must Release when the connection terminates.
//
// On failure the returned Reason indicates which ceiling was hit; no
// counters are incremented and Release is a no-op (Handle is nil-safe).
func (l *Limiter) Acquire(ip string) (*Handle, Reason) {
	if !l.allowRate(ip) {
		return nil, ReasonRate
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.profile.MaxTotal > 0 && l.total >= l.profile.MaxTotal {
		return nil, ReasonTotal
	}
	if l.profile.MaxPerIP > 0 && l.perIP[ip] >= l.profile.MaxPerIP {
		return nil, ReasonPerIP
	}

	l.total++
	l.perIP[ip]++

	return &Handle{limiter: l, ip: ip}, ReasonOK
}

// BindUser performs the post-auth gate: per-user and per-(user,IP).
//
// Must be called exactly once per Handle, immediately after the auth
// exchange succeeds and before the session enters the authenticated
// command loop. On rejection the per-user counters are not bumped and
// the caller should send a protocol BYE/421 and close.
func (h *Handle) BindUser(user string) Reason {
	if h == nil || h.closed || user == "" {
		return ReasonOK
	}

	l := h.limiter
	l.mu.Lock()
	defer l.mu.Unlock()

	key := userIPKey{user: user, ip: h.ip}
	if l.profile.MaxPerUser > 0 && l.perUser[user] >= l.profile.MaxPerUser {
		return ReasonPerUser
	}
	if l.profile.MaxPerUserIP > 0 && l.perUserIP[key] >= l.profile.MaxPerUserIP {
		return ReasonPerUserIP
	}

	l.perUser[user]++
	l.perUserIP[key]++
	h.user = user
	return ReasonOK
}

// Release decrements every counter that this Handle holds. Safe to call
// on a nil receiver and idempotent so deferred releases stay safe even
// if a Reject path also closed the handle.
func (h *Handle) Release() {
	if h == nil || h.closed {
		return
	}
	h.closed = true

	l := h.limiter
	l.mu.Lock()
	defer l.mu.Unlock()

	l.total--
	if l.total < 0 {
		l.total = 0
	}
	if n := l.perIP[h.ip] - 1; n <= 0 {
		delete(l.perIP, h.ip)
	} else {
		l.perIP[h.ip] = n
	}

	if h.user != "" {
		if n := l.perUser[h.user] - 1; n <= 0 {
			delete(l.perUser, h.user)
		} else {
			l.perUser[h.user] = n
		}
		key := userIPKey{user: h.user, ip: h.ip}
		if n := l.perUserIP[key] - 1; n <= 0 {
			delete(l.perUserIP, key)
		} else {
			l.perUserIP[key] = n
		}
	}
}

// Snapshot returns the live counter totals. Intended for /debug or metrics.
func (l *Limiter) Snapshot() (total, ips, users int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, len(l.perIP), len(l.perUser)
}

func (l *Limiter) allowRate(ip string) bool {
	if l.profile.NewConnPerIP <= 0 {
		return true
	}

	l.mu.Lock()
	rl, ok := l.rates[ip]
	if !ok {
		rl = rate.NewLimiter(rate.Limit(l.profile.NewConnPerIP), l.profile.NewConnBurst)
		l.rates[ip] = rl
		// Cap the rate map so a flood of unique IPs cannot grow it without
		// bound. Old entries get rebuilt on demand; the only cost is losing
		// burst credit, which is what we want for a stale IP anyway.
		if len(l.rates) > 65536 {
			for k := range l.rates {
				delete(l.rates, k)
				if len(l.rates) <= 32768 {
					break
				}
			}
		}
	}
	l.mu.Unlock()

	return rl.Allow()
}
