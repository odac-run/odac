package api

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"sync/atomic"

	"odac-proxy/config"
	"odac-proxy/proxy"
)

var validTokenRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// acmeRequest represents the JSON payload for ACME HTTP-01 challenge token management.
type acmeRequest struct {
	KeyAuthorization string `json:"keyAuthorization"`
	Token            string `json:"token"`
}

// Readiness reports whether the public listeners (HTTP :80 and HTTPS :443)
// are bound and serving. It is owned by main and shared with the API server
// so the /ready endpoint can answer authoritatively.
type Readiness struct {
	HTTP  atomic.Bool
	HTTPS atomic.Bool
}

// IsReady returns true only when both HTTP and HTTPS public listeners are bound.
// HTTP/3 is intentionally excluded — it is best-effort (UDP, may fail on hosts
// without sufficient privileges) and not required for a successful handover.
func (r *Readiness) IsReady() bool {
	return r.HTTP.Load() && r.HTTPS.Load()
}

type Server struct {
	proxy     *proxy.Proxy
	firewall  *proxy.Firewall
	readiness *Readiness
}

func NewServer(p *proxy.Proxy, f *proxy.Firewall, r *Readiness) *Server {
	return &Server{
		proxy:     p,
		firewall: f,
		readiness: r,
	}
}

// HandleACMEChallenge manages ACME HTTP-01 challenge tokens.
// POST sets a token, DELETE removes it. Used by Node.js SSL module during certificate generation.
func (s *Server) HandleACMEChallenge(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req acmeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if req.Token == "" || req.KeyAuthorization == "" {
			http.Error(w, "Missing token or keyAuthorization", http.StatusBadRequest)
			return
		}
		// Security: Validate token format (base64url characters only, max 256 chars)
		if len(req.Token) > 256 || !validTokenRegex.MatchString(req.Token) {
			http.Error(w, "Invalid token format", http.StatusBadRequest)
			return
		}
		s.proxy.SetACMEChallenge(req.Token, req.KeyAuthorization)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

	case http.MethodDelete:
		var req acmeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if req.Token == "" {
			http.Error(w, "Missing token", http.StatusBadRequest)
			return
		}
		s.proxy.DeleteACMEChallenge(req.Token)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleCachePurge clears cached assets for a specific domain or all domains.
// POST with {"domain": "example.com"} purges that domain.
// POST with empty body or {"domain": ""} purges all cached assets.
func (s *Server) HandleCachePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Domain string `json:"domain"`
	}
	// Body is optional — empty body means purge all
	json.NewDecoder(r.Body).Decode(&req)

	cache := s.proxy.Cache()
	var count int

	if req.Domain != "" {
		count = cache.Purge(req.Domain)
		s.proxy.Pages().Purge(req.Domain)
		s.proxy.Hints().Purge(req.Domain)
		log.Printf("[Cache] Purged %d entries for domain: %s", count, req.Domain)
	} else {
		count = cache.PurgeAll()
		s.proxy.Pages().PurgeAll()
		s.proxy.Hints().PurgeAll()
		log.Printf("[Cache] Purged all %d entries", count)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"purged": count,
	})
}

// HandleCacheStats returns current cache statistics.
func (s *Server) HandleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(s.proxy.Cache().Stats())
}

// HandleReady reports public-listener readiness for the zero-downtime handover.
// Returns 200 OK only after both :80 and :443 have been bound and are accepting
// connections. The Node.js Updater polls this before signaling the old container
// to release its overlap services — guaranteeing no traffic gap during takeover.
func (s *Server) HandleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.readiness != nil && s.readiness.IsReady() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not ready"))
}

func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		log.Printf("Failed to decode config: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	log.Printf("Received config update: %d domains, firewall enabled: %v, tunnels: %d", len(cfg.Domains), cfg.Firewall.Enabled, len(cfg.Tunnels))

	s.proxy.UpdateConfig(cfg.Domains, cfg.SSL, cfg.Tunnels, cfg.Memory)
	s.firewall.UpdateConfig(cfg.Firewall)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/challenge", s.HandleACMEChallenge)
	mux.HandleFunc("/cache/purge", s.HandleCachePurge)
	mux.HandleFunc("/cache/stats", s.HandleCacheStats)
	mux.HandleFunc("/config", s.HandleConfig)
	mux.HandleFunc("/ready", s.HandleReady)
	mux.ServeHTTP(w, r)
}
