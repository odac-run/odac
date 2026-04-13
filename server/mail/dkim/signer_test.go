package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odac-mail/config"
)

func generateTestKey(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key generation failed: %v", err)
	}

	keyPath := filepath.Join(dir, "test.key")
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("write key failed: %v", err)
	}
	return keyPath
}

func TestSigner_Sign_WithDKIM(t *testing.T) {
	dir := t.TempDir()
	keyPath := generateTestKey(t, dir)

	cfg := config.Config{
		Domains: map[string]config.Domain{
			"example.com": {
				Cert: config.DomainCert{
					DKIM: &config.DKIMConfig{
						Private:  keyPath,
						Selector: "default",
					},
				},
			},
		},
	}

	signer := NewSigner(func() config.Config { return cfg })

	body := []byte("From: user@example.com\r\nTo: other@test.com\r\nSubject: Test\r\n\r\nHello World\r\n")
	signed, err := signer.Sign("user@example.com", body)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	// Signed message should contain DKIM-Signature header
	if !strings.Contains(string(signed), "DKIM-Signature") {
		t.Error("signed message should contain DKIM-Signature header")
	}

	// Should be larger than original (signature added)
	if len(signed) <= len(body) {
		t.Error("signed message should be larger than original")
	}
}

func TestSigner_Sign_NoDKIMConfig(t *testing.T) {
	cfg := config.Config{
		Domains: map[string]config.Domain{
			"example.com": {},
		},
	}

	signer := NewSigner(func() config.Config { return cfg })

	body := []byte("From: user@example.com\r\nTo: other@test.com\r\nSubject: Test\r\n\r\nHello\r\n")
	result, err := signer.Sign("user@example.com", body)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	// Without DKIM config, body should be returned unchanged
	if string(result) != string(body) {
		t.Error("without DKIM config, body should be unchanged")
	}
}

func TestSigner_Sign_SubdomainWalkup(t *testing.T) {
	dir := t.TempDir()
	keyPath := generateTestKey(t, dir)

	cfg := config.Config{
		Domains: map[string]config.Domain{
			"example.com": {
				Cert: config.DomainCert{
					DKIM: &config.DKIMConfig{
						Private:  keyPath,
						Selector: "default",
					},
				},
			},
		},
	}

	signer := NewSigner(func() config.Config { return cfg })

	// Sending from sub.example.com should walk up to example.com for DKIM
	body := []byte("From: user@sub.example.com\r\nTo: other@test.com\r\nSubject: Test\r\n\r\nHello\r\n")
	signed, err := signer.Sign("user@sub.example.com", body)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	if !strings.Contains(string(signed), "DKIM-Signature") {
		t.Error("subdomain walkup should find parent domain DKIM config")
	}
}

func TestSigner_ClearCache(t *testing.T) {
	dir := t.TempDir()
	keyPath := generateTestKey(t, dir)

	cfg := config.Config{
		Domains: map[string]config.Domain{
			"example.com": {
				Cert: config.DomainCert{
					DKIM: &config.DKIMConfig{
						Private:  keyPath,
						Selector: "default",
					},
				},
			},
		},
	}

	signer := NewSigner(func() config.Config { return cfg })

	// Sign once to populate cache
	body := []byte("From: user@example.com\r\nTo: other@test.com\r\nSubject: Test\r\n\r\nHello\r\n")
	signer.Sign("user@example.com", body)

	// Clear cache
	signer.ClearCache("example.com")

	// Should still work after cache clear (reloads key)
	signed, err := signer.Sign("user@example.com", body)
	if err != nil {
		t.Fatalf("Sign after cache clear failed: %v", err)
	}
	if !strings.Contains(string(signed), "DKIM-Signature") {
		t.Error("should sign after cache clear")
	}
}

func TestSigner_InvalidKeyPath(t *testing.T) {
	cfg := config.Config{
		Domains: map[string]config.Domain{
			"example.com": {
				Cert: config.DomainCert{
					DKIM: &config.DKIMConfig{
						Private:  "/nonexistent/path/key.pem",
						Selector: "default",
					},
				},
			},
		},
	}

	signer := NewSigner(func() config.Config { return cfg })

	body := []byte("From: user@example.com\r\nTo: other@test.com\r\nSubject: Test\r\n\r\nHello\r\n")
	result, err := signer.Sign("user@example.com", body)
	if err != nil {
		t.Fatalf("Sign should not error on invalid key: %v", err)
	}

	// Should return original body when key can't be loaded
	if string(result) != string(body) {
		t.Error("should return original body when key is invalid")
	}
}
