// Package api implements the Control API for the ODAC mail server.
// The API listens on a Unix socket (or TCP fallback) and receives
// configuration updates and commands from the Node.js control plane.
// This mirrors the proxy's api/api.go and dns's api/api.go patterns.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"odac-mail/auth"
	"odac-mail/config"
	"odac-mail/storage"
)

// Server is the HTTP API server that receives commands from Node.js.
type Server struct {
	firewall   *auth.Firewall
	store      *storage.Store
	onConfig   func(config.Config)
	onSend     func(from, to string, body []byte) error // Callback for outbound delivery
	onSSLClear func(string)
}

// NewServer creates a new API server with the given dependencies.
func NewServer(store *storage.Store, fw *auth.Firewall, onConfig func(config.Config)) *Server {
	return &Server{
		firewall: fw,
		onConfig: onConfig,
		store:    store,
	}
}

// SetSSLClearCallback sets the callback for SSL cache clearing.
func (s *Server) SetSSLClearCallback(cb func(string)) {
	s.onSSLClear = cb
}

// SetSendCallback sets the callback for outbound email delivery.
func (s *Server) SetSendCallback(cb func(from, to string, body []byte) error) {
	s.onSend = cb
}

// HandleConfig processes full configuration syncs from Node.js.
// Replaces the entire mail configuration atomically.
// Endpoint: POST /config
func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		log.Printf("[Mail-API] Failed to decode config: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	log.Printf("[Mail-API] Config update: %d domains, %d accounts, hostname: %s",
		len(cfg.Domains), len(cfg.Accounts), cfg.Hostname)

	if s.onConfig != nil {
		s.onConfig(cfg)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// HandleHealth returns a simple health check response.
// Used by Node.js to verify the mail process is alive.
// Endpoint: GET /health
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// accountRequest represents the JSON payload for account operations.
type accountRequest struct {
	Domain   string `json:"domain"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Retype   string `json:"retype"`
}

// HandleAccountCreate creates a new mail account.
// Endpoint: POST /account
func (s *Server) HandleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req accountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" || req.Retype == "" {
		jsonError(w, "All fields are required", http.StatusBadRequest)
		return
	}

	if req.Password != req.Retype {
		jsonError(w, "Passwords do not match", http.StatusBadRequest)
		return
	}

	if !isValidEmail(req.Email) {
		jsonError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := s.store.AccountExists(ctx, req.Email)
	if err != nil {
		log.Printf("[Mail-API] Account exists check failed: %v", err)
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		jsonError(w, "Mail account already exists", http.StatusConflict)
		return
	}

	hashed, err := auth.HashPassword(req.Password)
	if err != nil {
		log.Printf("[Mail-API] Password hashing failed: %v", err)
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if err := s.store.AccountCreate(ctx, req.Email, hashed, req.Domain); err != nil {
		log.Printf("[Mail-API] Account creation failed: %v", err)
		jsonError(w, "Account creation failed", http.StatusInternalServerError)
		return
	}

	jsonSuccess(w, "Mail account created successfully")
}

// HandleAccountDelete removes a mail account.
// Endpoint: DELETE /account
func (s *Server) HandleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req accountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" {
		jsonError(w, "Email address is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := s.store.AccountExists(ctx, req.Email)
	if err != nil {
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		jsonError(w, "Mail account not found", http.StatusNotFound)
		return
	}

	if err := s.store.AccountDelete(ctx, req.Email); err != nil {
		log.Printf("[Mail-API] Account deletion failed: %v", err)
		jsonError(w, "Account deletion failed", http.StatusInternalServerError)
		return
	}

	jsonSuccess(w, "Mail account deleted successfully")
}

// HandleAccountPassword updates the password for an existing account.
// Endpoint: PUT /account/password
func (s *Server) HandleAccountPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req accountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" || req.Retype == "" {
		jsonError(w, "All fields are required", http.StatusBadRequest)
		return
	}

	if req.Password != req.Retype {
		jsonError(w, "Passwords do not match", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := s.store.AccountExists(ctx, req.Email)
	if err != nil {
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		jsonError(w, "Mail account not found", http.StatusNotFound)
		return
	}

	hashed, err := auth.HashPassword(req.Password)
	if err != nil {
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if err := s.store.AccountUpdatePassword(ctx, req.Email, hashed); err != nil {
		log.Printf("[Mail-API] Password update failed: %v", err)
		jsonError(w, "Password update failed", http.StatusInternalServerError)
		return
	}

	jsonSuccess(w, "Password updated successfully")
}

// HandleAccountList returns all accounts for a domain.
// Endpoint: GET /accounts?domain=example.com
func (s *Server) HandleAccountList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	domain := r.URL.Query().Get("domain")
	if domain == "" {
		jsonError(w, "Domain parameter is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	accounts, err := s.store.AccountList(ctx, domain)
	if err != nil {
		log.Printf("[Mail-API] Account list failed: %v", err)
		jsonError(w, "Internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"accounts": accounts,
		"success":  true,
	})
}

// HandleSend triggers outbound email delivery via the SMTP client.
// Endpoint: POST /send
func (s *Server) HandleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Body string `json:"body"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.From == "" || req.To == "" || req.Body == "" {
		jsonError(w, "from, to, and body are required", http.StatusBadRequest)
		return
	}

	if s.onSend == nil {
		jsonError(w, "Send not available", http.StatusServiceUnavailable)
		return
	}

	if err := s.onSend(req.From, req.To, []byte(req.Body)); err != nil {
		log.Printf("[Mail-API] Send failed: %v", err)
		jsonError(w, "Delivery failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonSuccess(w, "Mail sent successfully")
}

// HandleSSLClear clears the TLS context cache for a domain or all domains.
// Endpoint: POST /ssl/clear
func (s *Server) HandleSSLClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Domain string `json:"domain"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if s.onSSLClear != nil {
		s.onSSLClear(req.Domain)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// ServeHTTP implements http.Handler, routing requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/account":
		switch r.Method {
		case http.MethodPost:
			s.HandleAccountCreate(w, r)
		case http.MethodDelete:
			s.HandleAccountDelete(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "/account/password":
		s.HandleAccountPassword(w, r)
	case "/accounts":
		s.HandleAccountList(w, r)
	case "/config":
		s.HandleConfig(w, r)
	case "/health":
		s.HandleHealth(w, r)
	case "/send":
		s.HandleSend(w, r)
	case "/ssl/clear":
		s.HandleSSLClear(w, r)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// --- Helpers ---

func isValidEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	at := -1
	for i, c := range email {
		if c == '@' {
			if at >= 0 {
				return false // Multiple @
			}
			at = i
		}
	}
	if at < 1 || at >= len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if len(domain) < 3 || !containsDot(domain) {
		return false
	}
	return true
}

func containsDot(s string) bool {
	for _, c := range s {
		if c == '.' {
			return true
		}
	}
	return false
}

type apiResponse struct {
	Message string `json:"message"`
	Success bool   `json:"success"`
}

func jsonError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiResponse{Message: message, Success: false})
}

func jsonSuccess(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(apiResponse{Message: message, Success: true})
}
