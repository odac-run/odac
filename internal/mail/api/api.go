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

	"odac/internal/mail/address"
	"odac/internal/mail/auth"
	"odac/internal/mail/config"
	"odac/internal/mail/storage"
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

	if !address.Valid(req.Email) {
		jsonError(w, "Invalid email address", http.StatusBadRequest)
		return
	}
	// New accounts are stored canonically. Lookups match case-insensitively
	// either way, so this is about the spelling every mailbox row inherits
	// from AccountRow.Email, not about whether the account can be found.
	email := address.Normalize(req.Email)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := s.store.AccountExists(ctx, email)
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

	if err := s.store.AccountCreate(ctx, email, hashed, address.Normalize(req.Domain)); err != nil {
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

	if err := s.store.AccountDelete(ctx, existing.Email); err != nil {
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

	if err := s.store.AccountUpdatePassword(ctx, existing.Email, hashed); err != nil {
		log.Printf("[Mail-API] Password update failed: %v", err)
		jsonError(w, "Password update failed", http.StatusInternalServerError)
		return
	}

	jsonSuccess(w, "Password updated successfully")
}

// HandleAccountList returns the accounts of one domain, or every account
// when the domain parameter is omitted.
// Endpoint: GET /accounts[?domain=example.com]
func (s *Server) HandleAccountList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		accounts []storage.AccountEntry
		err      error
	)
	if domain := r.URL.Query().Get("domain"); domain != "" {
		accounts, err = s.store.AccountList(ctx, domain)
	} else {
		accounts, err = s.store.AccountListAll(ctx)
	}
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
