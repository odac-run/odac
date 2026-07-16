package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Token helpers, the port of Api.js:53–94. Key for every HMAC is
// config.api.auth (64-char hex, generated at Init). Three token types per
// contract 0.1: the raw key itself (root), hex(HMAC(key, domainName))
// (domain tokens, mail.send only), and base64(payload)+"."+hex(HMAC(key,
// payload)) (app tokens). Signatures are compared with hmac.Equal — Node
// used plain !==; the contract records constant-time comparison as a strict
// improvement, not a compat break.

// authKey reads the live config.api.auth like Node does on every use.
func (s *Server) authKey() string {
	var key string
	s.cfg.View(func() {
		if v, ok := s.cfg.Map("api")["auth"].(string); ok {
			key = v
		}
	})
	return key
}

// GenerateToken derives a domain token: hex(HMAC-SHA256(key=api.auth, domain)).
func (s *Server) GenerateToken(domain string) string {
	mac := hmac.New(sha256.New, []byte(s.authKey()))
	mac.Write([]byte(domain))
	return hex.EncodeToString(mac.Sum(nil))
}

// AddToken registers a domain's derived token for O(1) auth lookup.
func (s *Server) AddToken(domain string) {
	if domain == "" {
		return
	}
	token := s.GenerateToken(domain)
	s.mu.Lock()
	s.tokens[token] = domain
	s.mu.Unlock()
}

// RemoveToken forgets a domain's derived token.
func (s *Server) RemoveToken(domain string) {
	if domain == "" {
		return
	}
	token := s.GenerateToken(domain)
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// ReloadTokens rebuilds the domain-token map from config.domains. Node calls
// it only from init() as of v1.10.0 — domains added later get tokens when
// the Domain handlers (task 3.5) start calling AddToken/ReloadTokens.
func (s *Server) ReloadTokens() {
	domains := map[string]bool{}
	s.cfg.View(func() {
		for domain := range s.cfg.Map("domains") {
			domains[domain] = true
		}
	})
	s.mu.Lock()
	s.tokens = map[string]string{}
	s.mu.Unlock()
	for domain := range domains {
		s.AddToken(domain)
	}
}

// appTokenBody mirrors the payload JSON.stringify({n, p, t}) in
// generateAppToken — field order n, p, t is part of the signed bytes.
type appTokenBody struct {
	N string `json:"n"`
	P any    `json:"p"`
	T int64  `json:"t"`
}

// GenerateAppToken issues an app token (used by the App handlers, task 3.4,
// to mint ODAC_API_KEY for containers). permissions is an action list, may
// contain "*", or the literal true for all actions.
func (s *Server) GenerateAppToken(appName string, permissions any) string {
	payload, err := json.Marshal(appTokenBody{N: appName, P: permissions, T: time.Now().UnixMilli()})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(s.authKey()))
	mac.Write(payload)
	return base64.StdEncoding.EncodeToString(payload) + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyAppToken validates and decodes an app token; nil when invalid.
func (s *Server) verifyAppToken(token string) map[string]any {
	if token == "" || !strings.Contains(token, ".") {
		return nil
	}
	parts := strings.Split(token, ".")
	b64, sig := parts[0], parts[1]
	if b64 == "" || sig == "" {
		return nil
	}

	payload, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(s.authKey()))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return nil
	}

	var decoded map[string]any
	if json.Unmarshal(payload, &decoded) != nil {
		return nil
	}
	return decoded
}
