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

// Token helpers, the port of Api.js:53-94. The key is config.api.auth
// (64-char hex, generated at Init). Two token types remain from contract 0.1:
// the raw key itself (root) and base64(payload)+"."+hex(HMAC(key, payload))
// (app tokens). Node's third type, hex(HMAC(key, domainName)) for mail.send,
// was removed; see the package doc. Signatures are compared with hmac.Equal,
// where Node used plain !==; the contract records constant-time comparison as
// a strict improvement, not a compat break.

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
