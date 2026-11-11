package proxy

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"odac/internal/proxy/config"
)

const (
	// firewallMaxTracked bounds the rate-limit table. Once it is full of live records a new
	// client goes untracked rather than resetting everyone: clearing the table let any
	// client with enough addresses wipe every other client's counter.
	firewallMaxTracked = 20000

	// firewallSweepInterval throttles the expiry walk a full table triggers, so a flood of
	// new addresses cannot make every request walk the table under the lock.
	firewallSweepInterval = time.Second

	// firewallIPv6Prefix is the block one IPv6 client is counted as. A single host
	// commonly holds a whole /64, which would otherwise give it 2^64 separate budgets.
	firewallIPv6Prefix = 64
)

type Firewall struct {
	config        config.Firewall
	blacklist     addrList
	whitelist     addrList
	requestCounts map[string]*requestRecord
	wsCounts      map[string]int // Active WebSocket connections per client
	lastSweep     time.Time
	mu            sync.RWMutex
	stopCleanup   chan struct{}
}

type requestRecord struct {
	count     int
	timestamp int64
}

// addrList matches a client against single addresses and CIDR blocks. Entries that parse
// as neither are kept as raw strings so an existing list never loses a match.
type addrList struct {
	addrs    map[netip.Addr]struct{}
	prefixes []netip.Prefix
	raw      map[string]struct{}
}

func newAddrList(entries []string) addrList {
	l := addrList{addrs: make(map[netip.Addr]struct{}), raw: make(map[string]struct{})}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil {
				l.prefixes = append(l.prefixes, netip.PrefixFrom(p.Addr().Unmap(), unmappedBits(p)).Masked())
				continue
			}
		} else if a, err := netip.ParseAddr(e); err == nil {
			l.addrs[a.Unmap()] = struct{}{}
			continue
		}
		l.raw[e] = struct{}{}
	}
	return l
}

// unmappedBits converts a prefix length on an IPv4-mapped address to its IPv4 length.
func unmappedBits(p netip.Prefix) int {
	if p.Addr().Is4In6() {
		return max(p.Bits()-96, 0)
	}
	return p.Bits()
}

func (l addrList) contains(addr netip.Addr, raw string) bool {
	if addr.IsValid() {
		if _, ok := l.addrs[addr]; ok {
			return true
		}
		for _, p := range l.prefixes {
			if p.Contains(addr) {
				return true
			}
		}
	}
	_, ok := l.raw[raw]
	return ok
}

// clientKey names the budget a request is counted against: the address for IPv4, its /64
// for IPv6.
func clientKey(addr netip.Addr, raw string) string {
	if !addr.IsValid() {
		return raw
	}
	if addr.Is6() {
		return netip.PrefixFrom(addr, firewallIPv6Prefix).Masked().String()
	}
	return addr.String()
}

func NewFirewall(cfg config.Firewall) *Firewall {
	f := &Firewall{
		config:        cfg,
		blacklist:     newAddrList(cfg.Blacklist),
		whitelist:     newAddrList(cfg.Whitelist),
		requestCounts: make(map[string]*requestRecord),
		wsCounts:      make(map[string]int),
		stopCleanup:   make(chan struct{}),
	}
	go f.startCleanupLoop()
	return f
}

func (f *Firewall) UpdateConfig(cfg config.Firewall) {
	blacklist, whitelist := newAddrList(cfg.Blacklist), newAddrList(cfg.Whitelist)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = cfg
	f.blacklist = blacklist
	f.whitelist = whitelist
}

func (f *Firewall) GetRequestTimeout() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.config.RequestTimeout
}

func (f *Firewall) startCleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	for {
		select {
		case <-ticker.C:
			f.cleanup()
		case <-f.stopCleanup:
			ticker.Stop()
			return
		}
	}
}

func (f *Firewall) cleanup() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweepLocked(time.Now())
}

// sweepLocked drops records whose window has passed. f.mu must be held for writing.
func (f *Firewall) sweepLocked(now time.Time) {
	f.lastSweep = now
	if f.config.RateLimit.WindowMs == 0 {
		return
	}
	nowMs := now.UnixMilli()
	windowMs := int64(f.config.RateLimit.WindowMs)
	for key, record := range f.requestCounts {
		if nowMs-record.timestamp > windowMs {
			delete(f.requestCounts, key)
		}
	}
}

// countRequest records one request for key and returns its count in the current window,
// or 0 when the table is full of live records and key is not among them.
func (f *Firewall) countRequest(key string, windowMs int) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	record, exists := f.requestCounts[key]
	if !exists && len(f.requestCounts) >= firewallMaxTracked {
		if now.Sub(f.lastSweep) >= firewallSweepInterval {
			f.sweepLocked(now)
		}
		if len(f.requestCounts) >= firewallMaxTracked {
			return 0
		}
	}

	nowMs := now.UnixMilli()
	switch {
	case !exists:
		record = &requestRecord{count: 1, timestamp: nowMs}
		f.requestCounts[key] = record
	case nowMs-record.timestamp > int64(windowMs):
		record.count = 1
		record.timestamp = nowMs
	default:
		record.count++
	}
	return record.count
}

func (f *Firewall) Check(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.RLock()
		if !f.config.Enabled {
			f.mu.RUnlock()
			next.ServeHTTP(w, r)
			return
		}
		blacklist := f.blacklist
		whitelist := f.whitelist
		rateLimit := f.config.RateLimit
		maxWS := f.config.MaxWSPerIP
		f.mu.RUnlock()

		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		addr, err := netip.ParseAddr(ip)
		if err == nil {
			addr = addr.Unmap()
			ip = addr.String()
		}
		key := clientKey(addr, ip)

		if whitelist.contains(addr, ip) {
			next.ServeHTTP(w, r)
			return
		}

		if blacklist.contains(addr, ip) {
			log.Printf("Blocked request from blacklisted IP: %s", ip)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		if rateLimit.Enabled {
			if count := f.countRequest(key, rateLimit.WindowMs); count > rateLimit.Max {
				if count == rateLimit.Max+1 {
					log.Printf("Rate limit exceeded for %s", key)
				}
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
		}

		if maxWS > 0 && isWebSocketUpgrade(r.Header) {
			f.mu.Lock()
			if f.wsCounts[key] >= maxWS {
				f.mu.Unlock()
				log.Printf("Blocked WebSocket from %s: Max concurrent connections (%d) reached", key, maxWS)
				http.Error(w, "Too Many WebSocket Connections", http.StatusTooManyRequests)
				return
			}
			f.wsCounts[key]++
			f.mu.Unlock()

			defer func() {
				f.mu.Lock()
				f.wsCounts[key]--
				if f.wsCounts[key] <= 0 {
					delete(f.wsCounts, key)
				}
				f.mu.Unlock()
			}()
		}

		next.ServeHTTP(w, r)
	})
}
