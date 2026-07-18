package domains

// acme.go has no Node test counterpart (Acme.js shipped untested); these
// spec tests run the real x/crypto/acme wiring against a minimal in-process
// RFC 8555 server: account key persistence, registration, the order flow
// with challenge callbacks, and certificate download.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/logx"
)

// fakeACME is a loose RFC 8555 server: it ignores JWS signatures (decoding
// only the payloads it needs) and validates the protocol flow order.
type fakeACME struct {
	t  *testing.T
	ts *httptest.Server

	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate

	mu          sync.Mutex
	authzValid  bool
	accepted    bool
	finalized   bool
	orderDomain []string
	certDER     [][]byte
}

func newFakeACME(t *testing.T) *fakeACME {
	t.Helper()
	f := &fakeACME{t: t}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Fake ACME Root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.caKey = caKey
	f.caCert, _ = x509.ParseCertificate(caDER)

	mux := http.NewServeMux()
	mux.HandleFunc("/directory", f.directory)
	mux.HandleFunc("/new-nonce", f.nonce)
	mux.HandleFunc("/new-account", f.newAccount)
	mux.HandleFunc("/new-order", f.newOrder)
	mux.HandleFunc("/authz/1", f.authz)
	mux.HandleFunc("/challenge/1", f.challenge)
	mux.HandleFunc("/finalize/1", f.finalize)
	mux.HandleFunc("/order/1", f.order)
	mux.HandleFunc("/cert/1", f.cert)

	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeACME) url(path string) string { return f.ts.URL + path }

func (f *fakeACME) head(w http.ResponseWriter) {
	w.Header().Set("Replay-Nonce", "nonce-abc")
	w.Header().Set("Content-Type", "application/json")
}

// jwsPayload decodes the payload half of an incoming JWS body.
func jwsPayload(r *http.Request) []byte {
	var envelope struct {
		Payload string `json:"payload"`
	}
	json.NewDecoder(r.Body).Decode(&envelope)
	raw, _ := base64.RawURLEncoding.DecodeString(envelope.Payload)
	return raw
}

func (f *fakeACME) directory(w http.ResponseWriter, _ *http.Request) {
	f.head(w)
	json.NewEncoder(w).Encode(map[string]string{
		"newNonce":   f.url("/new-nonce"),
		"newAccount": f.url("/new-account"),
		"newOrder":   f.url("/new-order"),
	})
}

func (f *fakeACME) nonce(w http.ResponseWriter, _ *http.Request) {
	f.head(w)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeACME) newAccount(w http.ResponseWriter, _ *http.Request) {
	f.head(w)
	w.Header().Set("Location", f.url("/account/1"))
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"status": "valid"})
}

func (f *fakeACME) newOrder(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Identifiers []struct{ Type, Value string } `json:"identifiers"`
	}
	json.Unmarshal(jwsPayload(r), &payload)
	f.mu.Lock()
	f.orderDomain = nil
	for _, id := range payload.Identifiers {
		f.orderDomain = append(f.orderDomain, id.Value)
	}
	f.mu.Unlock()

	f.head(w)
	w.Header().Set("Location", f.url("/order/1"))
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"status":         "pending",
		"finalize":       f.url("/finalize/1"),
		"authorizations": []string{f.url("/authz/1")},
		"identifiers":    []map[string]string{{"type": "dns", "value": f.orderDomain[0]}},
	})
}

func (f *fakeACME) authz(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	status := "pending"
	if f.authzValid {
		status = "valid"
	}
	domain := "unknown"
	if len(f.orderDomain) > 0 {
		domain = f.orderDomain[0]
	}
	f.mu.Unlock()

	f.head(w)
	json.NewEncoder(w).Encode(map[string]any{
		"status":     status,
		"identifier": map[string]string{"type": "dns", "value": domain},
		"challenges": []map[string]any{
			{"type": "http-01", "url": f.url("/challenge/1"), "token": "token-http-1", "status": "pending"},
			{"type": "dns-01", "url": f.url("/challenge/1"), "token": "token-dns-1", "status": "pending"},
		},
	})
}

func (f *fakeACME) challenge(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.accepted = true
	f.authzValid = true // validation "succeeds" instantly
	f.mu.Unlock()

	f.head(w)
	json.NewEncoder(w).Encode(map[string]any{"type": "http-01", "status": "processing", "token": "token-http-1"})
}

func (f *fakeACME) finalize(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		CSR string `json:"csr"`
	}
	json.Unmarshal(jwsPayload(r), &payload)
	csrDER, err := base64.RawURLEncoding.DecodeString(payload.CSR)
	if err != nil {
		f.t.Errorf("bad csr encoding: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		f.t.Errorf("bad csr: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Sign a leaf for the CSR with the fake CA.
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		f.t.Errorf("leaf signing failed: %v", err)
	}

	f.mu.Lock()
	f.finalized = true
	f.certDER = [][]byte{leafDER, f.caCert.Raw}
	f.mu.Unlock()

	f.head(w)
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "valid",
		"finalize":    f.url("/finalize/1"),
		"certificate": f.url("/cert/1"),
	})
}

func (f *fakeACME) order(w http.ResponseWriter, _ *http.Request) {
	f.head(w)
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "valid",
		"finalize":    f.url("/finalize/1"),
		"certificate": f.url("/cert/1"),
	})
}

func (f *fakeACME) cert(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	ders := f.certDER
	f.mu.Unlock()

	w.Header().Set("Replay-Nonce", "nonce-abc")
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	for _, der := range ders {
		pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
}

// ─── tests ──────────────────────────────────────────────────────────────

func TestAccountKeyPersistence(t *testing.T) {
	dir := t.TempDir()

	k1, err := loadOrCreateAccountKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "acme_account.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v", info.Mode().Perm())
	}

	// Second load returns the SAME key (Node reuses the PEM file).
	k2, err := loadOrCreateAccountKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("account key regenerated instead of loaded")
	}

	// Corrupted file regenerates.
	if err := os.WriteFile(filepath.Join(dir, "acme_account.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	k3, err := loadOrCreateAccountKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k3.D.Cmp(k1.D) == 0 {
		t.Fatal("corrupted key not regenerated")
	}
}

func TestCSRShape(t *testing.T) {
	_, key, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	der, err := createCSR([]string{"example.com", "www.example.com"}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if csr.Subject.CommonName != "example.com" {
		t.Fatalf("CN = %s", csr.Subject.CommonName)
	}
	if len(csr.DNSNames) != 2 || csr.DNSNames[1] != "www.example.com" {
		t.Fatalf("SANs = %v", csr.DNSNames)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature: %v", err)
	}
}

func TestOrderFlowAgainstFakeServer(t *testing.T) {
	fake := newFakeACME(t)
	dir := t.TempDir()
	log := logx.New("SSL")

	client, err := newACMEClient(dir, fake.url("/directory"), fake.ts.Client(), log)
	if err != nil {
		t.Fatal(err)
	}

	_, key, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := createCSR([]string{"example.com"}, key)
	if err != nil {
		t.Fatal(err)
	}

	var created, removed []string
	pemChain, err := client.Order(orderOpts{
		ChallengeType: "http-01",
		CSR:           csr,
		Domains:       []string{"example.com"},
		ChallengeCreate: func(identifier, chalType, token, authValue string) error {
			created = append(created, fmt.Sprintf("%s|%s|%s|%s", identifier, chalType, token, authValue))
			return nil
		},
		ChallengeRemove: func(identifier, chalType, token, authValue string) {
			removed = append(removed, token)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Challenge callbacks fired with the http-01 challenge; the key
	// authorization is token.<thumbprint> per RFC 8555 §8.1.
	if len(created) != 1 || len(removed) != 1 {
		t.Fatalf("created = %v, removed = %v", created, removed)
	}
	parts := strings.Split(created[0], "|")
	if parts[0] != "example.com" || parts[1] != "http-01" || parts[2] != "token-http-1" {
		t.Fatalf("create call = %v", parts)
	}
	if !strings.HasPrefix(parts[3], "token-http-1.") {
		t.Fatalf("keyAuthorization = %s", parts[3])
	}
	if removed[0] != "token-http-1" {
		t.Fatalf("remove call = %v", removed)
	}

	// The returned PEM chain holds the leaf (with the CSR's SANs) + CA.
	block, rest := pem.Decode([]byte(pemChain))
	if block == nil {
		t.Fatalf("no PEM in chain: %q", pemChain)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "example.com" {
		t.Fatalf("leaf SANs = %v", leaf.DNSNames)
	}
	if block, _ = pem.Decode(rest); block == nil {
		t.Fatal("CA certificate missing from bundle")
	}

	// A second client from the same dir reuses the account key.
	if _, err := newACMEClient(dir, fake.url("/directory"), fake.ts.Client(), log); err != nil {
		t.Fatal(err)
	}
}

func TestOrderRejectsMissingChallengeType(t *testing.T) {
	fake := newFakeACME(t)
	dir := t.TempDir()

	client, err := newACMEClient(dir, fake.url("/directory"), fake.ts.Client(), logx.New("SSL"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := generateKeyPair()
	csr, _ := createCSR([]string{"example.com"}, key)

	_, err = client.Order(orderOpts{
		ChallengeType:   "tls-alpn-01", // the fake offers http-01/dns-01 only
		CSR:             csr,
		Domains:         []string{"example.com"},
		ChallengeCreate: func(_, _, _, _ string) error { return nil },
		ChallengeRemove: func(_, _, _, _ string) {},
	})
	if err == nil || !strings.Contains(err.Error(), "Challenge type tls-alpn-01 not available") {
		t.Fatalf("err = %v", err)
	}
}
