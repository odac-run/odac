package api

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"

	"odac-proxy/config"
	"odac-proxy/proxy"
)

var validTokenRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// acmeRequest represents the JSON payload for ACME HTTP-01 challenge token management.
type acmeRequest struct {
	KeyAuthorization string `json:"keyAuthorization"`
	Token            string `json:"token"`
}

type Server struct {
	proxy    *proxy.Proxy
	firewall *proxy.Firewall
}

func NewServer(p *proxy.Proxy, f *proxy.Firewall) *Server {
	return &Server{
		proxy:    p,
		firewall: f,
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

	log.Printf("Received config update: %d domains, firewall enabled: %v", len(cfg.Domains), cfg.Firewall.Enabled)

	s.proxy.UpdateConfig(cfg.Domains, cfg.SSL)
	s.firewall.UpdateConfig(cfg.Firewall)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/challenge", s.HandleACMEChallenge)
	mux.HandleFunc("/config", s.HandleConfig)
	mux.ServeHTTP(w, r)
}
