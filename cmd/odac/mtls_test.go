package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"odac/internal/apiproto"
	"odac/internal/mtls"
)

// signingServer answers domain.mtls.cert.issue the way the real one does, so the
// CLI's file output can be checked against an actual chain.
type signingServer struct {
	addr string
	ca   *x509.Certificate
	mu   sync.Mutex
	raw  []string
	fail string
}

func newSigningServer(t *testing.T) *signingServer {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	s := &signingServer{ca: ca}
	s.addr = fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		dump, _ := json.Marshal(req)
		s.mu.Lock()
		s.raw = append(s.raw, string(dump))
		fail := s.fail
		s.mu.Unlock()
		if fail != "" {
			fmt.Fprintf(conn, `{"id":"e","result":false,"message":%q}`, fail)
			return
		}
		opts := req.Data[1].(map[string]any)
		block, _ := pem.Decode([]byte(opts["csr"].(string)))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			fmt.Fprintf(conn, `{"id":"e","result":false,"message":"bad csr"}`)
			return
		}
		der, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ali"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, ca, csr.PublicKey, caKey)
		body, _ := json.Marshal(map[string]any{
			"id": "e", "result": true, "message": "issued",
			"data": map[string]any{
				"cert":        string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
				"chain":       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
				"fingerprint": mtls.Fingerprint(der),
			},
		})
		conn.Write(body)
	})
	return s
}

func (s *signingServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.raw...)
}

func TestIssueWritesAP12HoldingAKeyTheServerNeverSaw(t *testing.T) {
	s := newSigningServer(t)
	a, out, errOut := testApp(t, s.addr)
	file := filepath.Join(t.TempDir(), "ali.p12")

	code := a.dispatch([]string{"domain", "mtls", "issue", "-d", "x.com", "--name", "ali", "--hosts", "auth.x.com", "--out", file, "--password", "s3cret"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	key, cert, chain, err := pkcs12.DecodeChain(raw, "s3cret")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if key.(*ecdsa.PrivateKey).PublicKey.Equal(cert.PublicKey) == false {
		t.Fatal("the certificate does not match the key")
	}
	if len(chain) != 1 || !chain[0].Equal(s.ca) {
		t.Fatalf("chain = %v", chain)
	}
	if _, _, _, err := pkcs12.DecodeChain(raw, "wrong"); err == nil {
		t.Fatal("the .p12 is not protected by its password")
	}

	for _, req := range s.requests() {
		if strings.Contains(req, "PRIVATE KEY") {
			t.Fatal("the private key reached the server")
		}
		if strings.Contains(req, "s3cret") {
			t.Fatal("the password reached the server")
		}
	}
	if !strings.Contains(out.String(), file) {
		t.Fatalf("output = %q", out.String())
	}
}

func TestIssueWithCSRKeepsTheKeyElsewhere(t *testing.T) {
	s := newSigningServer(t)
	a, _, errOut := testApp(t, s.addr)
	dir := t.TempDir()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	csr := filepath.Join(dir, "ali.csr")
	if err := os.WriteFile(csr, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "ali.crt")

	if code := a.dispatch([]string{"domain", "mtls", "issue", "-d", "x.com", "--name", "ali", "--hosts", "x.com", "--csr", csr, "--out", file}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("the output should hold only certificates")
	}
	if n := bytes.Count(raw, []byte("BEGIN CERTIFICATE")); n != 2 {
		t.Fatalf("%d certificates in the output, want leaf and CA", n)
	}
}

func TestIssueRefusesBeforeSigning(t *testing.T) {
	s := newSigningServer(t)
	a, _, errOut := testApp(t, s.addr)
	dir := t.TempDir()

	exists := filepath.Join(dir, "taken.p12")
	if err := os.WriteFile(exists, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.dispatch([]string{"domain", "mtls", "issue", "-d", "x.com", "--name", "ali", "--hosts", "x.com", "--out", exists, "--password", "p"}); code == 0 {
		t.Fatal("overwrote an existing file")
	}
	if raw, _ := os.ReadFile(exists); string(raw) != "keep" {
		t.Fatal("existing file was changed")
	}

	if code := a.dispatch([]string{"domain", "mtls", "issue", "-d", "x.com", "--name", "ali", "--hosts", "x.com", "--out", filepath.Join(dir, "new.p12")}); code == 0 {
		t.Fatalf("issued with no password: %s", errOut.String())
	}
	if got := s.requests(); len(got) != 0 {
		t.Fatalf("the server signed anyway: %v", got)
	}
}

func TestIssueShowsTheServerRefusal(t *testing.T) {
	s := newSigningServer(t)
	s.fail = "App is exposed through a tunnel"
	a, _, errOut := testApp(t, s.addr)
	file := filepath.Join(t.TempDir(), "ali.p12")
	if code := a.dispatch([]string{"domain", "mtls", "issue", "-d", "x.com", "--name", "ali", "--hosts", "x.com", "--out", file, "--password", "p"}); code == 0 {
		t.Fatal("exit 0 on a refusal")
	}
	if !strings.Contains(errOut.String(), "tunnel") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a file was written for a refused request")
	}
}

func TestOnOverlayPicksTheLongestMount(t *testing.T) {
	mountinfo := strings.Join([]string{
		"1 0 0:1 / / rw - overlay overlay rw",
		"2 1 8:1 / /app rw - ext4 /dev/sda1 rw",
		"3 2 8:2 / /app/data rw - overlay other rw",
	}, "\n")
	cases := map[string]bool{
		"/root/ali.p12":     true,
		"/app/ali.p12":      false,
		"/application/a":    true,
		"/app/data/ali.p12": true,
	}
	for path, want := range cases {
		if got := onOverlay(mountinfo, path); got != want {
			t.Errorf("onOverlay(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCertFileName(t *testing.T) {
	cases := map[string]string{"ali": "ali", "Ali Veli": "Ali-Veli", "../../etc/passwd": "..-..-etc-passwd", "": "client"}
	for name, want := range cases {
		if got := certFileName(name); got != want {
			t.Errorf("certFileName(%q) = %q, want %q", name, got, want)
		}
	}
}

// set answers its warnings twice, as a sentence and as data for the dashboard;
// the CLI shows the sentence only.
func TestSetPrintsTheMessageWithoutItsData(t *testing.T) {
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		conn.Write([]byte(`{"id":"e","result":true,"message":"Client certificates are optional for x.com.","data":{"published":[8080],"restart":true,"siblings":[]}}`))
	})
	for _, args := range [][]string{
		{"domain", "mtls", "set", "-d", "x.com", "--mode", "optional"},
		{"domain", "mtls", "off", "-d", "x.com"},
	} {
		a, out, errOut := testApp(t, addr)
		if code := a.dispatch(args); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut.String())
		}
		if got := out.String(); got != "Client certificates are optional for x.com.\n" {
			t.Fatalf("%v printed %q", args, got)
		}
	}
}

func TestCAImportSendsBothFilesToTheLocalAPI(t *testing.T) {
	var got apiproto.Request
	var mu sync.Mutex
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		mu.Lock()
		got = req
		mu.Unlock()
		conn.Write([]byte(`{"id":"e","result":true,"message":"Imported client CA 0123456789abcdef.","data":{"id":"0123456789abcdef","name":"CA"}}`))
	})
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	os.WriteFile(certFile, []byte("CERT PEM"), 0o600)
	os.WriteFile(keyFile, []byte("KEY PEM"), 0o600)

	a, out, errOut := testApp(t, addr)
	if code := a.dispatch([]string{"domain", "mtls", "ca", "import", "--cert", certFile, "--key", keyFile}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Action != "domain.mtls.ca.import" || got.Auth != "roottoken" || len(got.Data) != 2 || got.Data[0] != "CERT PEM" || got.Data[1] != "KEY PEM" {
		t.Fatalf("request = %+v", got)
	}
	if out.String() != "Imported client CA 0123456789abcdef.\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestImportSendsOneObjectWithTheCertificate(t *testing.T) {
	var got apiproto.Request
	var mu sync.Mutex
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		mu.Lock()
		got = req
		mu.Unlock()
		conn.Write([]byte(`{"id":"e","result":true,"message":"Certificate ali imported.","data":{"fingerprint":"aa"}}`))
	})
	certFile := filepath.Join(t.TempDir(), "ali.crt")
	os.WriteFile(certFile, []byte("CERT PEM"), 0o600)

	a, out, errOut := testApp(t, addr)
	if code := a.dispatch([]string{"domain", "mtls", "import", "--cert", certFile, "--hosts", "a.x.com, b.x.com", "--name", "ali"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	want := []any{map[string]any{"cert": "CERT PEM", "hosts": []any{"a.x.com", "b.x.com"}, "name": "ali"}}
	if got.Action != "domain.mtls.cert.import" || !reflect.DeepEqual(got.Data, want) {
		t.Fatalf("request = %+v", got)
	}
	if out.String() != "Certificate ali imported.\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCAImportFailsBeforeCallingWithoutItsFiles(t *testing.T) {
	calls := make(chan struct{}, 1)
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		select {
		case calls <- struct{}{}:
		default:
		}
	})
	a, _, errOut := testApp(t, addr)
	if code := a.dispatch([]string{"domain", "mtls", "ca", "import", "--cert", filepath.Join(t.TempDir(), "missing"), "--key", "x"}); code == 0 {
		t.Fatal("a missing file must fail")
	}
	if len(calls) != 0 || !strings.Contains(errOut.String(), "Failed to read") {
		t.Fatalf("calls = %d, err = %q", len(calls), errOut.String())
	}
}

// A per-host issuer sits between the leaf and the CA, and a p12 that dropped it
// would leave the client with a certificate nothing can chain.
func TestP12CarriesTheWholeChain(t *testing.T) {
	mk := func(cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: isCA, BasicConstraintsValid: isCA, KeyUsage: x509.KeyUsageCertSign,
		}
		if parent == nil {
			parent, parentKey = tmpl, key
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(der)
		return cert, key
	}
	encode := func(c *x509.Certificate) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	ca, caKey := mk("CA", nil, nil, true)
	issuer, issuerKey := mk("a.x.com", ca, caKey, true)
	leaf, leafKey := mk("ali", issuer, issuerKey, false)

	raw, err := encodeP12(leafKey, encode(leaf), encode(issuer)+encode(ca), "p")
	if err != nil {
		t.Fatal(err)
	}
	_, _, chain, err := pkcs12.DecodeChain(raw, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || !chain[0].Equal(issuer) || !chain[1].Equal(ca) {
		t.Fatalf("chain has %d certificates, want the issuer then the CA", len(chain))
	}
}
