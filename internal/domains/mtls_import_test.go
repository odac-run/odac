package domains

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/logx"
	"odac/internal/mtls"
)

// sharedCA is a CA made outside ODAC, the way an operator makes the one every
// edge node imports. Its key exists only for the test's lifetime.
type sharedCA struct {
	cert    *x509.Certificate
	certPEM string
	keyPEM  string
}

func newSharedCA(t *testing.T, usage x509.KeyUsage) *sharedCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Edge Client CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              usage,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &sharedCA{
		cert:    cert,
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
}

func (ca *sharedCA) id() string { return mtls.Fingerprint(ca.cert.Raw)[:16] }

// syncBuffer collects log lines written from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	logx.Stdout, logx.Stderr = buf, buf
	t.Cleanup(func() { logx.Stdout, logx.Stderr = io.Discard, io.Discard })
	return buf
}

func TestImportedCAKeepsItsIDOnEveryServer(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign|x509.KeyUsageCRLSign)
	a, b := newMTLSFixture(t), newMTLSFixture(t)
	for _, fx := range []*mtlsFixture{a, b} {
		data := mustOK(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM)).Data.(map[string]any)
		if data["id"] != ca.id() || data["name"] != "Edge Client CA" {
			t.Fatalf("reply = %v, want id %s", data, ca.id())
		}
	}

	var stored map[string]any
	a.cfg.View(func() { stored, _ = a.m.casLocked(false)[ca.id()].(map[string]any) })
	if stored["managed"] != true || stored["cert"] != ca.certPEM {
		t.Fatalf("stored CA = %v", stored)
	}

	// Importing again changes nothing and still succeeds, so a runbook can rerun.
	mustOK(t, a.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM))
	var count int
	a.cfg.View(func() { count = len(a.m.casLocked(false)) })
	if count != 1 {
		t.Fatalf("CAs = %d", count)
	}
}

// Ids of CAs ODAC creates are not touched by the import path: they were always
// the certificate's fingerprint, which is what makes an imported one line up.
func TestCreatedCAIDIsItsCertificateFingerprint(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	id := stringList(fx.record("x.com")["ca"])[0]
	block, _ := pem.Decode([]byte(fx.m.caPEM(id)))
	if block == nil || mtls.Fingerprint(block.Bytes)[:16] != id {
		t.Fatalf("CA %s is not keyed by its fingerprint", id)
	}
}

func TestImportedCASignsForTheRecordItIsBoundTo(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM))
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{ca.id()}}))
	if got := stringList(fx.record("x.com")["ca"]); len(got) != 1 || got[0] != ca.id() {
		t.Fatalf("x.com trusts %v", got)
	}

	reply := fx.issue(t, "x.com", "dev1", "x.com")
	if reply["ca"] != ca.id() {
		t.Fatalf("issued under %v", reply["ca"])
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := parseIssued(t, reply).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("the certificate does not chain to the imported CA: %v", err)
	}
}

func TestImportedCAKeyStaysOnDiskOnly(t *testing.T) {
	logs := captureLogs(t)
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	fx := newMTLSFixture(t)
	r := mustOK(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM))

	keyPath := filepath.Join(fx.base, "cert", "mtls", ca.id()+".key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v", info.Mode().Perm())
	}
	keyBody := strings.Split(strings.TrimSpace(ca.keyPEM), "\n")[1]
	if raw, _ := os.ReadFile(filepath.Join(fx.base, "config", "mtls.json")); bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte(keyBody)) {
		t.Fatal("the CA key reached the config file")
	}
	if _, dump := fx.proxyPayload(t); strings.Contains(dump, "PRIVATE KEY") {
		t.Fatal("the CA key reached the proxy payload")
	}
	if strings.Contains(fmtResult(r), "PRIVATE") || strings.Contains(fmtResult(r), keyBody) {
		t.Fatal("the reply echoes the key")
	}
	if got := logs.String(); strings.Contains(got, "PRIVATE") || strings.Contains(got, keyBody) || !strings.Contains(got, ca.id()) {
		t.Fatalf("logs = %q", got)
	}
}

func fmtResult(r api.Result) string {
	var b strings.Builder
	b.WriteString(str(r.Message))
	if data, ok := r.Data.(map[string]any); ok {
		for k, v := range data {
			b.WriteString(k + "=" + str(v) + " ")
		}
	}
	return b.String()
}

func TestImportCAIsTheOperatorsAlone(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	fx := newMTLSFixture(t)
	for _, caller := range []api.Caller{{App: "myapp"}, {}, {Root: true, App: "myapp"}} {
		mustFail(t, fx.m.ImportCA(caller, ca.certPEM, ca.keyPEM), "permission_denied")
	}
	if _, err := os.Stat(filepath.Join(fx.base, "cert", "mtls", ca.id()+".key")); !os.IsNotExist(err) {
		t.Fatal("a refused import wrote the key")
	}
	if api.AppMayCall("domain.mtls.ca.import") {
		t.Fatal("no grant may ever reach a CA import")
	}
}

func TestImportCARejectsWhatItCannotUse(t *testing.T) {
	fx := newMTLSFixture(t)
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	other := newSharedCA(t, x509.KeyUsageCertSign)

	mustFail(t, fx.m.ImportCA(api.Operator, ca.certPEM, other.keyPEM), "does not belong")
	mustFail(t, fx.m.ImportCA(api.Operator, ca.certPEM+ca.keyPEM, ca.keyPEM), "private key")
	mustFail(t, fx.m.ImportCA(api.Operator, newExternalCA(t, false), ca.keyPEM), "not a CA")
	mustFail(t, fx.m.ImportCA(api.Operator, ca.certPEM, "junk"), "Invalid CA key")
	mustFail(t, fx.m.ImportCA(api.Operator, ca.certPEM, "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"), "encrypted")
	signOnly := newSharedCA(t, x509.KeyUsageDigitalSignature)
	mustFail(t, fx.m.ImportCA(api.Operator, signOnly.certPEM, signOnly.keyPEM), "not allowed to sign")

	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	weakDER := x509.MarshalPKCS1PrivateKey(weak)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "weak"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &weak.PublicKey, weak)
	mustFail(t, fx.m.ImportCA(api.Operator,
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: weakDER}))), "2048")

	var count int
	fx.cfg.View(func() { count = len(fx.m.casLocked(false)) })
	if count != 0 {
		t.Fatalf("a refused import left %d CA(s)", count)
	}
}

// openssl ecparam -genkey writes SEC 1 with an EC PARAMETERS block in front.
func TestImportCAReadsOpenSSLKeyFormats(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "sec1"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	sec1, _ := x509.MarshalECPrivateKey(key)
	keyPEM := "-----BEGIN EC PARAMETERS-----\nBgUrgQQAIg==\n-----END EC PARAMETERS-----\n" +
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}))

	fx := newMTLSFixture(t)
	mustOK(t, fx.m.ImportCA(api.Operator, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyPEM))
	id := mtls.Fingerprint(der)[:16]
	if _, _, _, err := fx.m.loadCA(id); err != nil {
		t.Fatalf("the stored key cannot be loaded for signing: %v", err)
	}
}

// An external CA's certificates have no records. Turning it managed would make
// the proxy refuse all of them, so the import says so instead.
func TestImportCADoesNotTurnAnExternalCAManaged(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"caPem": ca.certPEM}))
	mustFail(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM), "external CA")
	var stored map[string]any
	fx.cfg.View(func() { stored, _ = fx.m.casLocked(false)[ca.id()].(map[string]any) })
	if stored["managed"] != false {
		t.Fatalf("stored = %v", stored)
	}
}
