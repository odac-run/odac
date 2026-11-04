package domains

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/logx"
	"odac/internal/mtls"
	proxyconfig "odac/internal/proxy/config"
	"odac/internal/proxy/proxy"
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

// edgeNode is one server holding the shared CA, bound to x.com.
func edgeNode(t *testing.T, ca *sharedCA) *mtlsFixture {
	t.Helper()
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM))
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{ca.id()}}))
	return fx
}

func csrWithKey(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), key
}

func importOpts(cert string, hosts ...string) map[string]any {
	return map[string]any{"cert": cert, "hosts": anyList(hosts)}
}

func TestImportRecordsWhatAnotherNodeSigned(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	issued := a.issue(t, "x.com", "dev1", "x.com")

	syncs := b.proxy.certSyncs
	r := mustOK(t, b.m.Import(asApp, map[string]any{"cert": issued["cert"], "hosts": []any{"x.com"}, "name": "dev1"}))
	if b.proxy.certSyncs != syncs+1 {
		t.Fatal("an import must reach the proxy through the certificate-only push")
	}
	row := r.Data.(map[string]any)
	rows := mustOK(t, b.m.List(api.Operator, nil)).Data.([]any)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], row) {
		t.Fatalf("reply %v is not the list row %v", row, rows)
	}
	for key, want := range map[string]any{"fingerprint": issued["fingerprint"], "ca": ca.id(), "caName": "Edge Client CA", "name": "dev1", "serial": issued["serial"], "notAfter": issued["notAfter"]} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
	for _, hidden := range []string{"cert", "spki", "renewedFrom"} {
		if _, ok := row[hidden]; ok {
			t.Errorf("row carries %s", hidden)
		}
	}

	// Again: the same row, no new push. Without a name, the subject's is used.
	again := mustOK(t, b.m.Import(asApp, importOpts(issued["cert"].(string), "x.com")))
	if !reflect.DeepEqual(again.Data, row) || b.proxy.certSyncs != syncs+1 {
		t.Fatalf("second import = %v", again.Data)
	}

	// The record counts as a way in on this node too.
	mustOK(t, b.m.Set("x.com", "required", nil))
}

func TestImportNeverRevivesARevokedCertificate(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	issued := a.issue(t, "x.com", "dev1", "x.com")
	mustOK(t, b.m.Import(api.Operator, importOpts(issued["cert"].(string), "x.com")))
	mustOK(t, b.m.Revoke(api.Operator, issued["fingerprint"]))
	mustFail(t, b.m.Import(api.Operator, importOpts(issued["cert"].(string), "x.com")), "revoked")

	// A revocation that reached this node first, as the operator's tombstone,
	// holds the same way.
	other := a.issue(t, "x.com", "dev2", "x.com")
	mustOK(t, b.m.Revoke(api.Operator, other["fingerprint"]))
	mustFail(t, b.m.Import(api.Operator, importOpts(other["cert"].(string), "x.com")), "revoked")
}

func TestImportFollowsTheIssueRules(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	mustOK(t, a.m.Set("z.com", "optional", map[string]any{"ca": []any{ca.id()}}))
	mustOK(t, b.m.Set("z.com", "optional", map[string]any{"ca": []any{ca.id()}}))
	cert := a.issue(t, "x.com", "dev1", "x.com")["cert"].(string)

	mustFail(t, b.m.Import(api.Operator, importOpts(cert)), "At least one host")
	mustFail(t, b.m.Import(api.Operator, importOpts(cert, "nope.com")), "not registered")
	mustFail(t, b.m.Import(api.Operator, importOpts(cert, "y.com")), "not signed by a client CA its hosts trust")
	mustFail(t, b.m.Import(api.Operator, map[string]any{"cert": cert, "hosts": []any{"x.com"}, "name": "dev2"}), "does not match")
	mustFail(t, b.m.Import(api.Operator, importOpts(cert+ca.keyPEM, "x.com")), "private key")
	mustFail(t, b.m.Import(api.Operator, importOpts(ca.certPEM, "x.com")), "CA certificate")
	mustFail(t, b.m.Import(api.Operator, importOpts("junk", "x.com")), "Invalid certificate")

	// App reach: its own hosts only, and one foreign host refuses everything.
	mustFail(t, b.m.Import(asOther, importOpts(cert, "x.com")), "Host x.com is not registered")
	mustFail(t, b.m.Import(asApp, importOpts(cert, "x.com", "z.com")), "Host z.com is not registered")
	mustFail(t, b.m.Import(api.Caller{}, importOpts(cert, "x.com")), "permission_denied")
	if rows := mustOK(t, b.m.List(api.Operator, nil)).Data.([]any); len(rows) != 0 {
		t.Fatalf("refused imports left %v", rows)
	}

	// The operator may scope it to both, as Issue would allow.
	mustOK(t, b.m.Import(api.Operator, importOpts(cert, "x.com", "z.com")))
}

func TestImportNeedsAManagedCAOnThisNode(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a := edgeNode(t, ca)
	cert := a.issue(t, "x.com", "dev1", "x.com")["cert"].(string)

	// A node that never imported the CA has its own, which did not sign this.
	b := newMTLSFixture(t)
	mustOK(t, b.m.Set("x.com", "optional", nil))
	mustFail(t, b.m.Import(api.Operator, importOpts(cert, "x.com")), "not signed by a client CA")

	// An external CA's certificates need no record, so importing one says so.
	c := newMTLSFixture(t)
	mustOK(t, c.m.Set("x.com", "optional", map[string]any{"caPem": ca.certPEM}))
	mustFail(t, c.m.Import(api.Operator, importOpts(cert, "x.com")), "external CA")
}

func TestImportRefusesAnExpiredCertificateButToleratesClockSkew(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	reply := a.issue(t, "x.com", "dev1", "x.com")
	leaf := parseIssued(t, reply)

	b.m.now = func() time.Time { return leaf.NotAfter.Add(time.Minute) }
	mustFail(t, b.m.Import(api.Operator, importOpts(reply["cert"].(string), "x.com")), "expired")

	// This node's clock runs behind the signer's: not valid yet here, but it
	// will be, and the proxy checks the start time on every request.
	b.m.now = func() time.Time { return leaf.NotBefore.Add(-time.Hour) }
	mustOK(t, b.m.Import(api.Operator, importOpts(reply["cert"].(string), "x.com")))
}

// The point of an import: the proxy on the second node refuses the certificate
// before it, as a managed CA's certificate with no record, and accepts it after.
// The proxy is fed through the same payload builders the dataplane uses.
func TestProxyAcceptsAnImportedCertificate(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	csr, key := csrWithKey(t)
	reply := mustOK(t, a.m.Issue(api.Operator, "x.com", map[string]any{"name": "dev1", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	client := tls.Certificate{Certificate: [][]byte{parseIssued(t, reply).Raw}, PrivateKey: key}

	edge := newProxyEdge(t, b)
	if got := edge.verify(t, client); got != mtls.VerifyUntrusted {
		t.Fatalf("before the import the proxy says %q", got)
	}
	mustOK(t, b.m.Import(asApp, map[string]any{"cert": reply["cert"], "hosts": []any{"x.com"}, "name": "dev1"}))
	edge.sync(t)
	if got := edge.verify(t, client); got != mtls.VerifySuccess {
		t.Fatalf("after the import the proxy says %q", got)
	}

	mustOK(t, b.m.Revoke(asApp, reply["fingerprint"]))
	edge.sync(t)
	if got := edge.verify(t, client); got != mtls.VerifyRevoked {
		t.Fatalf("after the revoke the proxy says %q", got)
	}
}

type proxyEdge struct {
	fx      *mtlsFixture
	proxy   *proxy.Proxy
	front   *httptest.Server
	backend string
	port    int
}

func newProxyEdge(t *testing.T, fx *mtlsFixture) *proxyEdge {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get(mtls.HeaderVerify))
	}))
	t.Cleanup(backend.Close)
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portText)

	edge := &proxyEdge{fx: fx, proxy: proxy.NewProxy(), backend: host, port: port}
	edge.sync(t)

	server := newSharedCA(t, x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature)
	serverKey, _ := pem.Decode([]byte(server.keyPEM))
	signer, _ := x509.ParsePKCS8PrivateKey(serverKey.Bytes)
	serverCert := tls.Certificate{Certificate: [][]byte{server.cert.Raw}, PrivateKey: signer}
	base := &tls.Config{
		GetCertificate:     func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serverCert, nil },
		GetConfigForClient: edge.proxy.GetConfigForClient,
		MinVersion:         tls.VersionTLS12,
	}
	edge.proxy.SetTLSBase(base)
	edge.front = httptest.NewUnstartedServer(edge.proxy)
	edge.front.TLS = base
	edge.front.StartTLS()
	t.Cleanup(edge.front.Close)
	return edge
}

// sync hands the proxy this node's state the way a push would: each record's
// policy through mtls.DomainPayload, the CAs and certificates through
// mtls.StorePayload, both decoded into the proxy's own types.
func (e *proxyEdge) sync(t *testing.T) {
	t.Helper()
	certs, err := e.fx.m.CertSnapshot(e.fx.m.now())
	if err != nil {
		t.Fatal(err)
	}
	var sitePayload, storePayload []byte
	var subdomains []string
	e.fx.cfg.View(func() {
		record := e.fx.cfg.Map("domains")["x.com"].(map[string]any)
		subdomains = stringList(record["subdomain"])
		sitePayload, _ = json.Marshal(mtls.DomainPayload(record, "app-key"))
		storePayload, _ = json.Marshal(mtls.StorePayload(e.fx.cfg.Map("mtls"), certs))
	})
	var site proxyconfig.WebsiteMTLS
	var store proxyconfig.MTLS
	if err := json.Unmarshal(sitePayload, &site); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(storePayload, &store); err != nil {
		t.Fatal(err)
	}
	domains := map[string]proxyconfig.Website{
		"x.com": {Domain: "x.com", Port: e.port, ContainerIP: e.backend, Subdomains: subdomains, MTLS: &site},
	}
	e.proxy.UpdateClientAuth(domains, &store)
	e.proxy.UpdateConfig(domains, nil, nil, nil)
}

// verify returns what the proxy told the app about this client certificate.
func (e *proxyEdge) verify(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	addr := e.front.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "x.com", Certificates: []tls.Certificate{cert}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "https://x.com/", nil)
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	return string(body)
}
