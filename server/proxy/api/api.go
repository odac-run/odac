package api

import (
	"encoding/json"
	"log"
	"net/http"

	"odac-proxy/config"
	"odac-proxy/proxy"
)

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
	mux.HandleFunc("/config", s.HandleConfig)
	mux.ServeHTTP(w, r)
}
