// Package auth — firewall.go implements IP-based brute-force protection
// and connection blocking for the mail server. Mirrors the Node.js Mail.js
// #handleFailedAuth / #block / #isBlocked logic with identical thresholds.
package auth

import (
	"log"
	"sync"
	"time"
)

const (
	maxFailedAttempts = 5
	blockDuration     = 24 * time.Hour
	attemptResetAfter = 1 * time.Hour
)

type attemptRecord struct {
	attempts int
	last     time.Time
}

// Firewall tracks failed authentication attempts per IP and blocks
// offending addresses for 24 hours after 5 consecutive failures.
// Thread-safe via sync.Map for lock-free reads on the hot path.
type Firewall struct {
	blocked  sync.Map // map[string]time.Time (IP → unblock time)
	attempts sync.Map // map[string]*attemptRecord
	Enabled  bool     // Set to false to disable blocking (testing)
	mu       sync.Mutex
}

// NewFirewall creates a new Firewall instance with blocking enabled.
func NewFirewall() *Firewall {
	return &Firewall{Enabled: true}
}

// HandleFailedAuth records a failed authentication attempt for the given IP.
// Blocks the IP after maxFailedAttempts consecutive failures within attemptResetAfter.
func (f *Firewall) HandleFailedAuth(ip string) {
	if !f.Enabled {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	val, _ := f.attempts.LoadOrStore(ip, &attemptRecord{})
	rec := val.(*attemptRecord)

	// Reset counter if last attempt was more than 1 hour ago
	if time.Since(rec.last) > attemptResetAfter {
		rec.attempts = 0
	}

	rec.attempts++
	rec.last = time.Now()

	if rec.attempts > maxFailedAttempts {
		f.Block(ip, "Too many failed login attempts")
		f.attempts.Delete(ip)
	}
}

// ClearAttempts removes the failed attempt counter for an IP after successful login.
func (f *Firewall) ClearAttempts(ip string) {
	f.attempts.Delete(ip)
}

// Block adds an IP to the blocklist for 24 hours.
func (f *Firewall) Block(ip, reason string) {
	if _, loaded := f.blocked.LoadOrStore(ip, time.Now().Add(blockDuration)); !loaded {
		log.Printf("[Mail-FW] Blocking IP %s: %s", ip, reason)
	}
}

// IsBlocked checks if an IP is currently blocked.
// Automatically removes expired blocks (lazy cleanup).
func (f *Firewall) IsBlocked(ip string) bool {
	if !f.Enabled {
		return false
	}
	val, ok := f.blocked.Load(ip)
	if !ok {
		return false
	}

	unblockTime := val.(time.Time)
	if time.Now().After(unblockTime) {
		f.blocked.Delete(ip)
		return false
	}
	return true
}
