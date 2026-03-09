// Package api implements the Control API for the ODAC DNS server.
// The API listens on a Unix socket (or TCP fallback) and receives
// configuration updates from the Node.js control plane. This mirrors
// the proxy's api/api.go pattern for architectural consistency.
package api

import (
	"encoding/json"
	"log"
	"net/http"

	"odac-dns/config"
	"odac-dns/resolver"
)

// Server is the HTTP API server that receives config updates from Node.js.
type Server struct {
	resolver *resolver.Resolver
}

// NewServer creates a new API server wrapping the DNS resolver.
func NewServer(r *resolver.Resolver) *Server {
	return &Server{resolver: r}
}

// HandleConfig processes full zone configuration syncs from Node.js.
// Replaces the entire zone database atomically. Endpoint: POST /config
func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		log.Printf("[DNS-API] Failed to decode config: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	log.Printf("[DNS-API] Received config update: %d zones", len(cfg.Zones))

	s.resolver.UpdateConfig(cfg)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// HandleHealth returns a simple health check response.
// Used by Node.js to verify the DNS process is alive.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// ServeHTTP implements http.Handler, routing requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/config":
		s.HandleConfig(w, r)
	case "/health":
		s.HandleHealth(w, r)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}
