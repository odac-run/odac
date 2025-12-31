package proxy

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"odac-proxy/config"
)

type Firewall struct {
	config        config.Firewall
	requestCounts map[string]*requestRecord
	mu            sync.RWMutex
	stopCleanup   chan struct{}
}

type requestRecord struct {
	count     int
	timestamp int64
}

func NewFirewall(cfg config.Firewall) *Firewall {
	f := &Firewall{
		config:        cfg,
		requestCounts: make(map[string]*requestRecord),
		stopCleanup:   make(chan struct{}),
	}
	go f.startCleanupLoop()
	return f
}

func (f *Firewall) UpdateConfig(cfg config.Firewall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = cfg
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

	if f.config.RateLimit.WindowMs == 0 {
		return
	}

	now := time.Now().UnixMilli()
	windowMs := int64(f.config.RateLimit.WindowMs)

	for ip, record := range f.requestCounts {
		if now-record.timestamp > windowMs {
			delete(f.requestCounts, ip)
		}
	}
}

func (f *Firewall) Check(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.RLock()
		if !f.config.Enabled {
			f.mu.RUnlock()
			next.ServeHTTP(w, r)
			return
		}
		
		// Copy config values needed for checking to avoid holding RLock too long if possible,
		// but checking slice contains is fast enough to keep lock.
		// However, we need to upgrade lock for rate limiting.
		
		blacklist := f.config.Blacklist
		whitelist := f.config.Whitelist
		rateLimit := f.config.RateLimit
		f.mu.RUnlock()

		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		
		// Handle X-Forwarded-For if needed (Node.js version does)
		forwarded := r.Header.Get("X-Forwarded-For")
		if forwarded != "" {
			parts := strings.Split(forwarded, ",")
			ip = strings.TrimSpace(parts[0])
		}

		// Normalize IPv6 mapped IPv4
		if strings.HasPrefix(ip, "::ffff:") {
			ip = ip[7:]
		}

		if contains(whitelist, ip) {
			next.ServeHTTP(w, r)
			return
		}

		if contains(blacklist, ip) {
			log.Printf("Blocked request from blacklisted IP: %s", ip)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		if rateLimit.Enabled {
			f.mu.Lock()
			// Memory protection
			if len(f.requestCounts) > 20000 {
				f.requestCounts = make(map[string]*requestRecord)
				log.Println("Firewall request counts cleared due to memory limit")
			}

			now := time.Now().UnixMilli()
			record, exists := f.requestCounts[ip]

			if !exists {
				f.requestCounts[ip] = &requestRecord{count: 1, timestamp: now}
			} else {
				if now-record.timestamp > int64(rateLimit.WindowMs) {
					record.count = 1
					record.timestamp = now
				} else {
					record.count++
				}
			}

			count := f.requestCounts[ip].count
			f.mu.Unlock()

			if count > rateLimit.Max {
				if count == rateLimit.Max+1 {
					log.Printf("Rate limit exceeded for IP: %s", ip)
				}
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
