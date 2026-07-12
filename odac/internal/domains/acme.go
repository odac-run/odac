package domains

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme"

	"odac/internal/logx"
)

// This file replaces SSL/Acme.js — the hand-written RFC 8555 client — with
// golang.org/x/crypto/acme (the PLAN 3.5 decision). Preserved from Node:
// the EC P-256 account key persisted at <certDir>/acme_account.key (PKCS#8
// PEM, 0600, regenerated when missing or unreadable), EC P-256 domain keys,
// CSRs whose CN is the first domain with every domain in the SAN list, and
// the order flow (per-authorization challenge create → accept → poll →
// best-effort remove, then finalize + certificate download as a PEM chain).
// The JWS/nonce/directory plumbing Acme.js implemented by hand is the
// library's job now. Polling budgets: Node polled 30 attempts (~85s) per
// resource; here each authorization wait and the finalize get a 90-second
// deadline within a 5-minute order budget.

// letsEncryptURL is Acme.LETS_ENCRYPT — the production directory.
const letsEncryptURL = "https://acme-v02.api.letsencrypt.org/directory"

const (
	registerTimeout = 30 * time.Second
	waitTimeout     = 90 * time.Second
	orderTimeout    = 5 * time.Minute
)

// acmeClient is the real acmeOrderer over x/crypto/acme.
type acmeClient struct {
	client *acme.Client
	log    *logx.Logger
}

// newACMEClient ports Acme.create(): load-or-create the account key,
// discover the directory and register (or retrieve) the account. hc
// overrides the HTTP client for tests; nil uses the default.
func newACMEClient(certDir, directoryURL string, hc *http.Client, log *logx.Logger) (acmeOrderer, error) {
	key, err := loadOrCreateAccountKey(certDir)
	if err != nil {
		return nil, err
	}

	client := &acme.Client{
		Key:          key,
		DirectoryURL: directoryURL,
		HTTPClient:   hc,
	}

	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()
	_, err = client.Register(ctx, &acme.Account{}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("ACME account registration failed: %w", err)
	}

	return &acmeClient{client: client, log: log}, nil
}

// loadOrCreateAccountKey ports the account-key half of Acme.#init.
func loadOrCreateAccountKey(certDir string) (*ecdsa.PrivateKey, error) {
	keyPath := filepath.Join(certDir, "acme_account.key")

	if raw, err := os.ReadFile(keyPath); err == nil {
		if block, _ := pem.Decode(raw); block != nil {
			if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				if key, ok := parsed.(*ecdsa.PrivateKey); ok {
					return key, nil
				}
			}
		}
		// Corrupted or wrong type — fall through and regenerate, like Node.
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Order ports Acme.order(): create the order, satisfy each authorization
// with the requested challenge type, finalize with the CSR and download the
// certificate chain as PEM.
func (a *acmeClient) Order(o orderOpts) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), orderTimeout)
	defer cancel()

	order, err := a.client.AuthorizeOrder(ctx, acme.DomainIDs(o.Domains...))
	if err != nil {
		return "", fmt.Errorf("ACME order creation failed: %w", err)
	}

	for _, authzURL := range order.AuthzURLs {
		authz, err := a.client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return "", err
		}
		if authz.Status == acme.StatusValid {
			continue
		}

		var challenge *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == o.ChallengeType {
				challenge = c
				break
			}
		}
		if challenge == nil {
			return "", fmt.Errorf("Challenge type %s not available for %s", o.ChallengeType, authz.Identifier.Value)
		}

		// The library computes the RFC 8555 §8 auth values from the account
		// key thumbprint — what Acme.js derived by hand.
		var authValue string
		if o.ChallengeType == "dns-01" {
			authValue, err = a.client.DNS01ChallengeRecord(challenge.Token)
		} else {
			authValue, err = a.client.HTTP01ChallengeResponse(challenge.Token)
		}
		if err != nil {
			return "", err
		}

		if err := o.ChallengeCreate(authz.Identifier.Value, challenge.Type, challenge.Token, authValue); err != nil {
			return "", err
		}
		if _, err := a.client.Accept(ctx, challenge); err != nil {
			return "", err
		}

		waitCtx, waitCancel := context.WithTimeout(ctx, waitTimeout)
		_, err = a.client.WaitAuthorization(waitCtx, authz.URI)
		waitCancel()
		if err != nil {
			// Node's #poll throws before challengeRemoveFn runs — same here.
			return "", fmt.Errorf("ACME validation failed: %w", err)
		}

		if o.ChallengeRemove != nil {
			o.ChallengeRemove(authz.Identifier.Value, challenge.Type, challenge.Token, authValue)
		}
	}

	finCtx, finCancel := context.WithTimeout(ctx, waitTimeout)
	chain, _, err := a.client.CreateOrderCert(finCtx, order.FinalizeURL, o.CSR, true)
	finCancel()
	if err != nil {
		return "", err
	}

	var out []byte
	for _, der := range chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return string(out), nil
}

// generateKeyPair ports Acme.generateKeyPair(): an EC P-256 domain key as
// (PKCS#8 PEM, signer).
func generateKeyPair() (string, crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", nil, err
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return pemStr, key, nil
}

// createCSR ports Acme.createCsr(): DER-encoded PKCS#10 with CN = first
// domain and every domain in the SAN extension, signed ECDSA-SHA256.
func createCSR(domains []string, key crypto.Signer) ([]byte, error) {
	if len(domains) == 0 {
		return nil, errors.New("no domains for CSR")
	}
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domains[0]},
		DNSNames: domains,
	}
	return x509.CreateCertificateRequest(rand.Reader, tmpl, key)
}
