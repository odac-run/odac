// Package dkim implements DKIM (DomainKeys Identified Mail) signing for outbound emails.
// Uses github.com/emersion/go-msgauth for RFC 6376 compliant signing.
// Reads private keys from disk paths provided via config sync from Node.js.
package dkim

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/emersion/go-msgauth/dkim"

	"odac-mail/config"
)

// Signer handles DKIM signing for outbound emails using per-domain keys.
// Keys are loaded lazily and cached until SSL cache is cleared.
type Signer struct {
	getConfig func() config.Config
	keyCache  sync.Map // map[string]crypto.Signer (domain → private key)
}

// NewSigner creates a new DKIM signer with the given config provider.
func NewSigner(getConfig func() config.Config) *Signer {
	return &Signer{getConfig: getConfig}
}

// Sign applies a DKIM signature to the given email body for the sender domain.
// Walks up the domain hierarchy to find DKIM config (e.g., sub.example.com → example.com).
// Returns the signed message or the original body if DKIM is not configured.
func (s *Signer) Sign(from string, body []byte) ([]byte, error) {
	// Extract sender domain
	at := strings.LastIndex(from, "@")
	if at < 0 {
		return body, nil
	}
	senderDomain := from[at+1:]

	cfg := s.getConfig()

	// Walk up domain hierarchy to find DKIM config
	var dkimCfg *config.DKIMConfig
	var signingDomain string
	domain := senderDomain
	for domain != "" {
		if d, ok := cfg.Domains[domain]; ok && d.Cert.DKIM != nil {
			dkimCfg = d.Cert.DKIM
			signingDomain = domain
			break
		}
		idx := strings.Index(domain, ".")
		if idx < 0 {
			break
		}
		domain = domain[idx+1:]
	}

	if dkimCfg == nil || dkimCfg.Private == "" {
		return body, nil // No DKIM configured, return unsigned
	}

	// Load private key (cached)
	key, err := s.loadKey(signingDomain, dkimCfg.Private)
	if err != nil {
		log.Printf("[DKIM] Failed to load key for %s: %v", signingDomain, err)
		return body, nil // Continue without DKIM on error
	}

	selector := dkimCfg.Selector
	if selector == "" {
		selector = "default"
	}

	// Sign the message
	opts := &dkim.SignOptions{
		Domain:   signingDomain,
		Selector: selector,
		Signer:   key,
		HeaderKeys: []string{
			"from", "to", "subject", "date", "message-id",
		},
	}

	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(body), opts)
	if err != nil {
		log.Printf("[DKIM] Signing failed for %s: %v", signingDomain, err)
		return body, nil // Continue without DKIM on error
	}

	log.Printf("[DKIM] Message signed for domain %s (selector: %s)", signingDomain, selector)
	return signed.Bytes(), nil
}

// ClearCache removes cached private keys, forcing reload on next sign.
func (s *Signer) ClearCache(domain string) {
	if domain == "" {
		s.keyCache = sync.Map{}
		return
	}
	s.keyCache.Delete(domain)
}

// loadKey loads and caches a DKIM private key from disk.
func (s *Signer) loadKey(domain, keyPath string) (crypto.Signer, error) {
	if val, ok := s.keyCache.Load(domain); ok {
		return val.(crypto.Signer), nil
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read key file %s: %w", keyPath, err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM data in %s", keyPath)
	}

	// Try PKCS#1 first (Node.js generates PKCS#1 for dkim-signer compatibility)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err == nil {
		s.keyCache.Store(domain, key)
		return key, nil
	}

	// Fallback to PKCS#8
	pkcs8Key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cannot parse private key: %w", err)
	}

	signer, ok := pkcs8Key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("key is not a signer")
	}

	s.keyCache.Store(domain, signer)
	return signer, nil
}
