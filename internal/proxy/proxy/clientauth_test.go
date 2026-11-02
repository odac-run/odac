package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"odac/internal/mtls"
	"odac/internal/proxy/config"
)

const authTestKey = "app-signing-key"

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca *testCA) issue(t *testing.T, cn string, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func fingerprintOf(cert tls.Certificate) string { return mtls.Fingerprint(cert.Certificate[0]) }

// authFixture runs the proxy behind a real TLS listener, so handshake behavior
// is what crypto/tls actually does rather than what a fake ConnectionState claims.
type authFixture struct {
	backendHits atomic.Int64
	domains     map[string]config.Website
	front       *httptest.Server
	mu          sync.Mutex
	proxy       *Proxy
	seen        http.Header
	seenReq     *http.Request
	store       *config.MTLS
	upstream    func(w http.ResponseWriter, r *http.Request)
}

func newAuthFixture(t *testing.T, ca *testCA, certs map[string][]string, site func(*config.Website)) *authFixture {
	t.Helper()
	fx := &authFixture{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.backendHits.Add(1)
		fx.mu.Lock()
		fx.seen = r.Header.Clone()
		fx.seenReq = r
		upstream := fx.upstream
		fx.mu.Unlock()
		if upstream != nil {
			upstream(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write([]byte("body{color:red;" + strings.Repeat(" ", 16) + "}"))
	}))
	t.Cleanup(backend.Close)
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portText)

	x := config.Website{Domain: "x.com", Port: port, ContainerIP: host, Subdomains: []string{"www", "auth", "admin", "public", "*"}}
	x.MTLS = &config.WebsiteMTLS{CA: []string{"main"}, Key: authTestKey, Mode: mtls.ModeRequired}
	if site != nil {
		site(&x)
	}
	fx.domains = map[string]config.Website{
		"x.com":   x,
		"pub.com": {Domain: "pub.com", Port: port, ContainerIP: host},
	}
	fx.store = &config.MTLS{CAs: map[string]config.MTLSCA{"main": {Cert: ca.pem, Managed: true}}, Certs: certs}

	fx.proxy = NewProxy()
	fx.proxy.SetHTTP3Available(true)
	fx.sync()

	serverCert := selfSignedServerCert(t)
	base := &tls.Config{
		GetCertificate:     func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serverCert, nil },
		GetConfigForClient: fx.proxy.GetConfigForClient,
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS12,
	}
	fx.proxy.SetTLSBase(base)

	fx.front = httptest.NewUnstartedServer(fx.proxy)
	fx.front.EnableHTTP2 = true
	fx.front.TLS = base
	fx.front.StartTLS()
	t.Cleanup(fx.front.Close)
	return fx
}

func (fx *authFixture) sync() {
	fx.proxy.UpdateClientAuth(fx.domains, fx.store)
	fx.proxy.UpdateConfig(fx.domains, nil, nil, nil)
}

func (fx *authFixture) lastHeaders() http.Header {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.seen
}

func selfSignedServerCert(t *testing.T) tls.Certificate {
	t.Helper()
	ca := newTestCA(t, "server")
	return tls.Certificate{Certificate: [][]byte{ca.cert.Raw}, PrivateKey: ca.key}
}

type authClient struct {
	cert     *tls.Certificate
	h2       bool
	sessions tls.ClientSessionCache
	sni      string
}

func (fx *authFixture) transport(c authClient) *http.Transport {
	conf := &tls.Config{InsecureSkipVerify: true, ServerName: c.sni, ClientSessionCache: c.sessions}
	if c.cert != nil {
		conf.Certificates = []tls.Certificate{*c.cert}
	}
	addr := fx.front.Listener.Addr().String()
	return &http.Transport{
		TLSClientConfig:   conf,
		ForceAttemptHTTP2: c.h2,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

func (fx *authFixture) get(t *testing.T, c authClient, host, path string, headers map[string]string) (*http.Response, string, error) {
	t.Helper()
	tr := fx.transport(c)
	defer tr.CloseIdleConnections()
	return roundTrip(t, tr, host, path, headers)
}

func roundTrip(t *testing.T, tr *http.Transport, host, path string, headers map[string]string) (*http.Response, string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body), nil
}

func wantStatus(t *testing.T, label string, resp *http.Response, err error, want int) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s: status = %d, want %d", label, resp.StatusCode, want)
	}
}

func TestRequiredRefusesAHandshakeWithoutCertificate(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	if _, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/", nil); err == nil {
		t.Fatal("a required host must fail the handshake without a certificate")
	}
	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached")
	}
}

func TestRequiredAcceptsScopedCertificateAndSignsIdentity(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fp := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fp: {"auth.x.com"}}, nil)

	resp, _, err := fx.get(t, authClient{sni: "auth.x.com", cert: &cert}, "auth.x.com", "/page?a=1", nil)
	wantStatus(t, "scoped", resp, err, http.StatusOK)

	h := fx.lastHeaders()
	if h.Get(mtls.HeaderVerify) != mtls.VerifySuccess || h.Get(mtls.HeaderFingerprint) != fp {
		t.Fatalf("headers = %v", h)
	}
	if h.Get(mtls.HeaderIssuer) != mtls.Fingerprint(ca.cert.Raw) {
		t.Fatalf("issuer = %q", h.Get(mtls.HeaderIssuer))
	}
	subject, _ := url.PathUnescape(h.Get(mtls.HeaderSubject))
	if subject != "CN=ali" {
		t.Fatalf("subject = %q", subject)
	}

	ts, _ := strconv.ParseInt(strings.TrimPrefix(strings.Split(h.Get(mtls.HeaderSignature), ",")[0], "t="), 10, 64)
	want := mtls.Assertion{
		Timestamp: ts, Method: "GET", Host: "auth.x.com", URI: "/page?a=1", Verify: mtls.VerifySuccess,
		Subject: h.Get(mtls.HeaderSubject), Issuer: h.Get(mtls.HeaderIssuer), Fingerprint: fp,
		Serial: h.Get(mtls.HeaderSerial), NotAfter: h.Get(mtls.HeaderNotAfter),
	}.Sign(authTestKey)
	if h.Get(mtls.HeaderSignature) != want {
		t.Fatalf("signature = %q, want %q", h.Get(mtls.HeaderSignature), want)
	}
}

func TestScopeKeepsACertificateToItsOwnHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(auth): {"auth.x.com"}}, nil)
	c := authClient{cert: &auth}

	c.sni = "admin.x.com"
	resp, _, err := fx.get(t, c, "admin.x.com", "/", nil)
	wantStatus(t, "same SNI", resp, err, http.StatusForbidden)

	// A coalesced connection gets 421, which makes a browser retry with its own SNI.
	c.sni = "auth.x.com"
	resp, _, err = fx.get(t, c, "admin.x.com", "/", nil)
	wantStatus(t, "coalesced", resp, err, http.StatusMisdirectedRequest)

	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached")
	}
}

func TestWildcardScopeCoversOneLabel(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"*.x.com"}}, nil)

	resp, _, err := fx.get(t, authClient{sni: "foo.x.com", cert: &cert}, "foo.x.com", "/", nil)
	wantStatus(t, "one label", resp, err, http.StatusOK)
	resp, _, err = fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil)
	wantStatus(t, "apex", resp, err, http.StatusForbidden)
}

func TestManagedCACertificateWithoutRecordIsRefused(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, nil, nil)
	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil)
	wantStatus(t, "no record", resp, err, http.StatusForbidden)
}

func TestExternalCANeedsNoRecord(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, nil, nil)
	fx.store.CAs["main"] = config.MTLSCA{Cert: ca.pem}
	fx.sync()
	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil)
	wantStatus(t, "external", resp, err, http.StatusOK)
}

func TestPublicSNICannotReachAProtectedHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	resp, _, err := fx.get(t, authClient{sni: "pub.com"}, "x.com", "/", nil)
	wantStatus(t, "fronted", resp, err, http.StatusMisdirectedRequest)
	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached")
	}
}

func TestProtectedSNICanStillReachAPublicHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, nil)
	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "pub.com", "/", nil)
	wantStatus(t, "public host", resp, err, http.StatusOK)
	if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != "" {
		t.Fatalf("a public host must not receive identity headers, got verify %q", got)
	}
}

func TestMixedCaseHostHitsTheHostOverride(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"auth.x.com"}}, func(s *config.Website) {
		s.MTLS.Mode = mtls.ModeOptional
		s.MTLS.Hosts = map[string]config.HostMTLS{"admin.x.com": {Mode: mtls.ModeRequired}}
	})
	resp, _, err := fx.get(t, authClient{sni: "x.com"}, "ADMIN.x.com", "/", nil)
	wantStatus(t, "mixed case", resp, err, http.StatusMisdirectedRequest)
	resp, _, err = fx.get(t, authClient{sni: "x.com"}, "x.com", "/", nil)
	wantStatus(t, "apex stays optional", resp, err, http.StatusOK)
}

func TestUnregisteredDeepHostIsDropped(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) {
		s.MTLS.Mode = mtls.ModeOptional
		s.MTLS.Hosts = map[string]config.HostMTLS{"admin.x.com": {Mode: mtls.ModeRequired}}
	})
	if _, _, err := fx.get(t, authClient{sni: "x.com"}, "admin.foo.x.com", "/", nil); err == nil {
		t.Fatal("admin.foo.x.com must not reach the app past admin.x.com's policy")
	}
	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached")
	}
}

func TestStrictRecordWithoutWildcardRefusesUnlistedSubdomain(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, func(s *config.Website) {
		s.MTLS.Mode = mtls.ModeOptional
		s.Subdomains = []string{"www", "auth"}
	})
	if _, _, err := fx.get(t, authClient{sni: "x.com"}, "foo.x.com", "/", nil); err == nil {
		t.Fatal("an unlisted subdomain must be dropped without a wildcard")
	}
	resp, _, err := fx.get(t, authClient{sni: "auth.x.com"}, "auth.x.com", "/", nil)
	wantStatus(t, "listed", resp, err, http.StatusOK)
	resp, _, err = fx.get(t, authClient{sni: "www.x.com"}, "www.x.com", "/", nil)
	wantStatus(t, "www", resp, err, http.StatusOK)
}

func TestOptionalReportsEachOutcomeToTheApp(t *testing.T) {
	ca := newTestCA(t, "ca")
	good := ca.issue(t, "good", time.Now().Add(time.Hour))
	scoped := ca.issue(t, "scoped", time.Now().Add(time.Hour))
	expired := ca.issue(t, "expired", time.Now().Add(-time.Hour))
	revoked := ca.issue(t, "revoked", time.Now().Add(time.Hour))
	foreign := newTestCA(t, "other").issue(t, "foreign", time.Now().Add(time.Hour))

	fx := newAuthFixture(t, ca, map[string][]string{
		fingerprintOf(good):    {"x.com"},
		fingerprintOf(scoped):  {"auth.x.com"},
		fingerprintOf(expired): {"x.com"},
		fingerprintOf(revoked): {"x.com"},
	}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	fx.store.Revoked = []string{fingerprintOf(revoked)}
	fx.sync()

	cases := []struct {
		cert *tls.Certificate
		want string
	}{
		{nil, mtls.VerifyNone},
		{&good, mtls.VerifySuccess},
		{&scoped, mtls.VerifyOutOfScope},
		{&expired, mtls.VerifyExpired},
		{&revoked, mtls.VerifyRevoked},
		{&foreign, mtls.VerifyUntrusted},
	}
	for _, tc := range cases {
		c := authClient{sni: "x.com", cert: tc.cert}
		// The client only sends a certificate the server's CA list matches, unless forced.
		tr := fx.transport(c)
		if tc.cert != nil {
			forced := *tc.cert
			tr.TLSClientConfig.Certificates = nil
			tr.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &forced, nil }
		}
		resp, _, err := roundTrip(t, tr, "x.com", "/page", nil)
		tr.CloseIdleConnections()
		wantStatus(t, tc.want, resp, err, http.StatusOK)
		h := fx.lastHeaders()
		if got := h.Get(mtls.HeaderVerify); got != tc.want {
			t.Errorf("verify = %q, want %q", got, tc.want)
		}
		if tc.want != mtls.VerifySuccess && h.Get(mtls.HeaderFingerprint) != "" {
			t.Errorf("%s: identity leaked: %v", tc.want, h)
		}
		if h.Get(mtls.HeaderSignature) == "" {
			t.Errorf("%s: verdict is unsigned", tc.want)
		}
	}
}

func TestClientCannotForgeIdentityHeaders(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	forged := map[string]string{
		mtls.HeaderVerify: mtls.VerifySuccess, mtls.HeaderSubject: "CN=root", mtls.HeaderFingerprint: "aa",
		mtls.HeaderSignature: "t=1,v1=aa", "X-Odac-Connection-Ssl": "true",
	}
	resp, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/page", forged)
	wantStatus(t, "optional", resp, err, http.StatusOK)
	h := fx.lastHeaders()
	if h.Get(mtls.HeaderVerify) != mtls.VerifyNone || h.Get(mtls.HeaderSubject) != "" || h.Get(mtls.HeaderFingerprint) != "" {
		t.Fatalf("forged headers survived: %v", h)
	}
	if strings.Contains(h.Get(mtls.HeaderSignature), "t=1,") {
		t.Fatalf("forged signature survived: %v", h)
	}

	// On a host with no policy too, or an app could be fooled through a sibling domain.
	resp, _, err = fx.get(t, authClient{sni: "pub.com"}, "pub.com", "/page", forged)
	wantStatus(t, "public", resp, err, http.StatusOK)
	h = fx.lastHeaders()
	for _, header := range mtls.Headers {
		if h.Get(header) != "" {
			t.Fatalf("%s survived on a public host", header)
		}
	}
}

func TestPlainConnectionSslHeaderIsNotForgeable(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "http://pub.com/", nil)
	req.Header.Set("X-Odac-Connection-Ssl", "true")
	rec := httptest.NewRecorder()
	fx.proxy.ServeHTTP(rec, req)
	if got := fx.lastHeaders().Get("X-Odac-Connection-Ssl"); got != "" {
		t.Fatalf("X-Odac-Connection-Ssl = %q on plain HTTP", got)
	}
}

func TestCertificateHolderBypassesSharedCaches(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })

	// Counted by verdict: background revalidation also reaches the backend, and
	// it carries no identity.
	var anonymous, holders atomic.Int64
	fx.mu.Lock()
	fx.upstream = func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(mtls.HeaderVerify) {
		case mtls.VerifyNone:
			anonymous.Add(1)
		case mtls.VerifySuccess:
			holders.Add(1)
		}
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write([]byte("body{}"))
	}
	fx.mu.Unlock()

	for i := 0; i < 2; i++ {
		resp, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/a.css", nil)
		wantStatus(t, "anonymous", resp, err, http.StatusOK)
	}
	if got := anonymous.Load(); got != 1 {
		t.Fatalf("anonymous traffic should still cache: %d backend hits", got)
	}

	for i := 0; i < 2; i++ {
		resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/a.css", nil)
		wantStatus(t, "holder", resp, err, http.StatusOK)
		if resp.Header.Get("X-Odac-Cache") == "HIT" {
			t.Fatal("a certificate holder was answered from the shared cache")
		}
	}
	if got := holders.Load(); got != 2 {
		t.Fatalf("holder requests must reach the backend: %d hits", got)
	}
}

func TestCertificateHolderResponseIsNeverStored(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })

	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/secret.css", nil)
	wantStatus(t, "holder", resp, err, http.StatusOK)

	resp, _, err = fx.get(t, authClient{sni: "x.com"}, "x.com", "/secret.css", nil)
	wantStatus(t, "anonymous", resp, err, http.StatusOK)
	if fx.backendHits.Load() != 2 {
		t.Fatal("an anonymous visitor was served a certificate holder's stored response")
	}
}

func TestHintsAreNotLearnedFromCertificateHolders(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	fx.mu.Lock()
	fx.upstream = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Link", "</private.css>; rel=preload; as=style")
		w.Write([]byte("<html></html>"))
	}
	fx.mu.Unlock()

	for i := 0; i < 5; i++ {
		resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil)
		wantStatus(t, "holder", resp, err, http.StatusOK)
	}
	rec := httptest.NewRecorder()
	fx.proxy.hints.Send(rec, "x.com", "/")
	if rec.Header().Get("Link") != "" || rec.Code == http.StatusEarlyHints {
		t.Fatal("hints were learned from a certificate holder's response")
	}
}

func TestAltSvcIsHiddenOnlyOnPolicyHosts(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })

	resp, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/", nil)
	wantStatus(t, "policy host", resp, err, http.StatusOK)
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("Alt-Svc = %q on a policy host", got)
	}
	resp, _, err = fx.get(t, authClient{sni: "pub.com"}, "pub.com", "/", nil)
	wantStatus(t, "public host", resp, err, http.StatusOK)
	if resp.Header.Get("Alt-Svc") == "" {
		t.Fatal("a public host lost Alt-Svc")
	}
}

func TestHTTP3RefusesPolicyHosts(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	if _, err := fx.proxy.GetHTTP3ConfigForClient(&tls.ClientHelloInfo{ServerName: "x.com"}); err == nil {
		t.Fatal("a policy host must not complete a QUIC handshake")
	}
	conf, err := fx.proxy.GetHTTP3ConfigForClient(&tls.ClientHelloInfo{ServerName: "pub.com"})
	if err != nil || conf != nil {
		t.Fatalf("public host: conf=%v err=%v", conf, err)
	}
}

func TestEarlyDataIsRefusedForEveryMethod(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	req := httptest.NewRequest(http.MethodGet, "https://x.com/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "x.com", HandshakeComplete: false}
	rec := httptest.NewRecorder()
	fx.proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooEarly {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPlainHTTP(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)

	serve := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		fx.proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	if rec := serve("http://x.com/"); rec.Code != http.StatusForbidden {
		t.Fatalf("required without a server certificate: status = %d", rec.Code)
	}
	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached over plain HTTP")
	}

	fx.proxy.SetACMEChallenge("tok", "auth-value")
	if rec := serve("http://x.com/.well-known/acme-challenge/tok"); rec.Code != http.StatusOK || rec.Body.String() != "auth-value" {
		t.Fatalf("ACME must stay reachable: %d %q", rec.Code, rec.Body.String())
	}

	site := fx.domains["x.com"]
	site.Cert = config.Cert{SSL: config.SSL{Key: "k", Cert: "c"}}
	fx.domains["x.com"] = site
	fx.sync()
	if rec := serve("http://x.com/"); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("required with a server certificate: status = %d", rec.Code)
	}
}

func TestRevocationReachesAnOpenConnection(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fp := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fp: {"x.com"}}, nil)

	var dials atomic.Int64
	tr := fx.transport(authClient{sni: "x.com", cert: &cert, h2: true})
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, addr)
	}
	defer tr.CloseIdleConnections()

	resp, _, err := roundTrip(t, tr, "x.com", "/", nil)
	wantStatus(t, "before", resp, err, http.StatusOK)

	fx.store.Revoked = []string{fp}
	fx.sync()

	resp, _, err = roundTrip(t, tr, "x.com", "/", nil)
	wantStatus(t, "after", resp, err, http.StatusForbidden)
	if dials.Load() != 1 {
		t.Fatalf("test needs a reused connection, dialed %d times", dials.Load())
	}
}

func TestScopeChangeReachesAnOpenConnection(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fp := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fp: {"x.com"}}, nil)
	tr := fx.transport(authClient{sni: "x.com", cert: &cert, h2: true})
	defer tr.CloseIdleConnections()

	resp, _, err := roundTrip(t, tr, "x.com", "/", nil)
	wantStatus(t, "before", resp, err, http.StatusOK)
	fx.store.Certs[fp] = []string{"auth.x.com"}
	fx.sync()
	resp, _, err = roundTrip(t, tr, "x.com", "/", nil)
	wantStatus(t, "after", resp, err, http.StatusForbidden)
}

func TestRevocationCutsAnInFlightStream(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fp := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fp: {"x.com"}}, nil)
	fx.mu.Lock()
	fx.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("hello\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	fx.mu.Unlock()

	tr := fx.transport(authClient{sni: "x.com", cert: &cert})
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "https://x.com/stream", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	fx.store.Revoked = []string{fp}
	fx.sync()

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its certificate's revocation")
	}
}

func TestReleaseForgetsLiveRequests(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, nil)
	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil)
	wantStatus(t, "request", resp, err, http.StatusOK)
	fx.proxy.liveAuthMu.Lock()
	defer fx.proxy.liveAuthMu.Unlock()
	if len(fx.proxy.liveAuth) != 0 {
		t.Fatalf("%d live entries leaked", len(fx.proxy.liveAuth))
	}
}

// singleSessionCache hands the same ticket to every server name, the way an
// attacker would replay one against another virtual host.
type singleSessionCache struct {
	mu    sync.Mutex
	state *tls.ClientSessionState
}

func (c *singleSessionCache) Get(string) (*tls.ClientSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.state != nil
}

func (c *singleSessionCache) Put(_ string, cs *tls.ClientSessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cs != nil {
		c.state = cs
	}
}

func TestTicketFromOneHostDoesNotAuthorizeAnother(t *testing.T) {
	caA := newTestCA(t, "a")
	caB := newTestCA(t, "b")
	cert := caA.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, caA, map[string][]string{fingerprintOf(cert): {"x.com"}}, nil)
	fx.store.CAs["other"] = config.MTLSCA{Cert: caB.pem, Managed: true}
	site := fx.domains["pub.com"]
	site.MTLS = &config.WebsiteMTLS{CA: []string{"other"}, Key: "k", Mode: mtls.ModeRequired}
	fx.domains["pub.com"] = site
	fx.sync()

	cache := &singleSessionCache{}
	resp, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert, sessions: cache}, "x.com", "/", nil)
	wantStatus(t, "first host", resp, err, http.StatusOK)
	if cache.state == nil {
		t.Fatal("no ticket was issued, so this test proved nothing")
	}
	before := fx.backendHits.Load()

	resp, _, err = fx.get(t, authClient{sni: "pub.com", cert: &cert, sessions: cache}, "pub.com", "/", nil)
	if err == nil && resp.StatusCode == http.StatusOK {
		t.Fatal("a ticket from x.com opened a host that trusts another CA")
	}
	if fx.backendHits.Load() != before {
		t.Fatal("backend was reached")
	}
}

func TestResumedSessionIsStillChecked(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fp := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fp: {"x.com"}}, nil)
	cache := &singleSessionCache{}
	client := authClient{sni: "x.com", cert: &cert, sessions: cache}

	resp, _, err := fx.get(t, client, "x.com", "/", nil)
	wantStatus(t, "first", resp, err, http.StatusOK)

	resp, _, err = fx.get(t, client, "x.com", "/", nil)
	wantStatus(t, "resumed", resp, err, http.StatusOK)
	if !resp.TLS.DidResume {
		t.Fatal("the second connection did not resume, so this test proved nothing")
	}

	fx.store.Revoked = []string{fp}
	fx.sync()
	resp, _, err = fx.get(t, client, "x.com", "/", nil)
	wantStatus(t, "revoked", resp, err, http.StatusForbidden)
}

func TestOptionalResumptionKeepsIdentityOnItsHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	cache := &singleSessionCache{}
	client := authClient{sni: "x.com", cert: &cert, sessions: cache}

	resp, _, err := fx.get(t, client, "x.com", "/page", nil)
	wantStatus(t, "first", resp, err, http.StatusOK)
	resp, _, err = fx.get(t, client, "x.com", "/page", nil)
	wantStatus(t, "resumed", resp, err, http.StatusOK)
	if !resp.TLS.DidResume {
		t.Fatal("the second connection did not resume, so this test proved nothing")
	}
	if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != mtls.VerifySuccess {
		t.Fatalf("verify = %q on a resumed session", got)
	}
}

func TestUnknownCAFailsClosed(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(cert): {"x.com"}}, func(s *config.Website) { s.MTLS.CA = []string{"missing"} })
	if _, _, err := fx.get(t, authClient{sni: "x.com", cert: &cert}, "x.com", "/", nil); err == nil {
		t.Fatal("a policy with no usable CA must trust nothing")
	}
	if fx.backendHits.Load() != 0 {
		t.Fatal("backend was reached")
	}
}

func TestConfigWithoutBaseFailsClosed(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	p := NewProxy()
	p.UpdateClientAuth(fx.domains, fx.store)
	p.UpdateConfig(fx.domains, nil, nil, nil)
	if _, err := p.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "x.com"}); err == nil {
		t.Fatal("a policy host must not fall back to a config that asks for no certificate")
	}
}

func TestDisablingEveryPolicyRemovesTheTable(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	site := fx.domains["x.com"]
	site.MTLS = nil
	fx.domains["x.com"] = site
	fx.sync()
	if fx.proxy.clientAuth.Load() != nil {
		t.Fatal("hosts with no policy should pay nothing")
	}
	resp, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/", nil)
	wantStatus(t, "open", resp, err, http.StatusOK)
}

func TestHostOverrideCanOpenOneHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, func(s *config.Website) {
		s.MTLS.Hosts = map[string]config.HostMTLS{"public.x.com": {Mode: mtls.ModeOff}}
	})
	resp, _, err := fx.get(t, authClient{sni: "public.x.com"}, "public.x.com", "/", nil)
	wantStatus(t, "override", resp, err, http.StatusOK)
	if _, _, err := fx.get(t, authClient{sni: "x.com"}, "x.com", "/", nil); err == nil {
		t.Fatal("the rest of the record must stay required")
	}
}

func TestStandaloneWildcardRecordRoutesOneLabel(t *testing.T) {
	p := NewProxy()
	p.UpdateConfig(map[string]config.Website{
		"*.solo.com": {Domain: "*.solo.com"},
		"x.com":      {Domain: "x.com"},
		"*.x.com":    {Domain: "*.x.com"},
	}, nil, nil, nil)
	cases := map[string]string{
		"foo.solo.com": "*.solo.com",
		"a.b.solo.com": "",
		"solo.com":     "",
		"foo.x.com":    "*.x.com",
		"a.b.x.com":    "x.com",
		"x.com":        "x.com",
	}
	for host, want := range cases {
		site, ok := p.resolveDomain(host)
		if ok != (want != "") || site.Domain != want {
			t.Errorf("resolveDomain(%q) = %q, %v, want %q", host, site.Domain, ok, want)
		}
	}
}

func TestWildcardRecordPurgesTheNamesItServes(t *testing.T) {
	if !keyBelongsToHost("foo.solo.com/a.css", "*.solo.com") {
		t.Fatal("a deploy must purge what a wildcard record cached")
	}
	if keyBelongsToHost("foo.other.com/a.css", "*.solo.com") {
		t.Fatal("purge reached another domain")
	}
}

func TestMixedCaseSNIFindsItsCertificate(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	conf, err := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "X.COM"})
	if err != nil || conf == nil || conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("conf=%v err=%v", conf, err)
	}
}

// A real QUIC handshake, since quic-go wraps GetConfigForClient in its own layer
// and only an end-to-end dial shows the refusal survives it.
func TestQUICHandshakeIsRefusedForAPolicyHost(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)

	serverCert := selfSignedServerCert(t)
	conf := &tls.Config{
		GetCertificate:     func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serverCert, nil },
		GetConfigForClient: fx.proxy.GetHTTP3ConfigForClient,
		MinVersion:         tls.VersionTLS13,
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTP3Server(udp.LocalAddr().String(), fx.proxy, conf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeHTTP3(srv, udp, nil)
	}()
	t.Cleanup(func() {
		srv.Close()
		udp.Close()
		<-done
	})

	dial := func(sni string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := dialH3(ctx, t, udp.LocalAddr().String(), &tls.Config{ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"h3"}}, nil)
		if err == nil {
			conn.CloseWithError(0, "")
		}
		return err
	}

	if err := dial("pub.com"); err != nil {
		t.Fatalf("a public host lost HTTP/3: %v", err)
	}
	if err := dial("x.com"); err == nil {
		t.Fatal("a policy host completed a QUIC handshake")
	}
}

func verdictFixture(t *testing.T, ca *testCA) (*Proxy, map[string]config.Website, *config.MTLS) {
	t.Helper()
	domains := map[string]config.Website{"x.com": {
		Domain: "x.com", Subdomains: []string{"auth"},
		MTLS: &config.WebsiteMTLS{CA: []string{"main"}, Key: authTestKey, Mode: mtls.ModeRequired},
	}}
	store := &config.MTLS{CAs: map[string]config.MTLSCA{"main": {Cert: ca.pem, Managed: true}}, Certs: map[string][]string{}}
	p := NewProxy()
	p.UpdateClientAuth(domains, store)
	return p, domains, store
}

func parsedLeaf(t *testing.T, cert tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestVerdictSurvivesAConfigPush(t *testing.T) {
	ca := newTestCA(t, "ca")
	p, domains, store := verdictFixture(t, ca)
	chain := []*x509.Certificate{parsedLeaf(t, ca.issue(t, "ali", time.Now().Add(time.Hour)))}
	now := time.Now()

	if v := p.clientAuth.Load().records["x.com"].policy.trust.verify(chain, now); v.status != mtls.VerifySuccess {
		t.Fatalf("verdict = %+v", v)
	}
	p.verdicts.mu.Lock()
	for key, v := range p.verdicts.recent {
		v.root = "from the cache"
		p.verdicts.recent[key] = v
	}
	p.verdicts.mu.Unlock()

	p.UpdateClientAuth(domains, store)

	if v := p.clientAuth.Load().records["x.com"].policy.trust.verify(chain, now); v.root != "from the cache" {
		t.Fatalf("a push with the same CAs verified again: %+v", v)
	}
}

func TestChangingTheCAsDoesNotReuseAVerdict(t *testing.T) {
	ca := newTestCA(t, "ca")
	p, domains, store := verdictFixture(t, ca)
	chain := []*x509.Certificate{parsedLeaf(t, ca.issue(t, "ali", time.Now().Add(time.Hour)))}
	now := time.Now()
	if v := p.clientAuth.Load().records["x.com"].policy.trust.verify(chain, now); v.status != mtls.VerifySuccess {
		t.Fatalf("verdict = %+v", v)
	}

	store.CAs["main"] = config.MTLSCA{Cert: newTestCA(t, "other").pem, Managed: true}
	p.UpdateClientAuth(domains, store)

	if v := p.clientAuth.Load().records["x.com"].policy.trust.verify(chain, now); v.status != mtls.VerifyUntrusted {
		t.Fatalf("a CA that is gone still opens the door: %+v", v)
	}
}

func TestVerdictExpiresWithTheCertificate(t *testing.T) {
	ca := newTestCA(t, "ca")
	p, _, _ := verdictFixture(t, ca)
	notAfter := time.Now().Add(time.Hour)
	chain := []*x509.Certificate{parsedLeaf(t, ca.issue(t, "ali", notAfter))}
	trust := p.clientAuth.Load().records["x.com"].policy.trust
	if v := trust.verify(chain, time.Now()); v.status != mtls.VerifySuccess {
		t.Fatalf("verdict = %+v", v)
	}
	if v := trust.verify(chain, notAfter.Add(time.Second)); v.status != mtls.VerifyExpired {
		t.Fatalf("verdict past expiry = %+v", v)
	}
}

func TestVerdictCacheKeepsWhatIsInUseAndStaysBounded(t *testing.T) {
	c := newVerdictCache()
	hot := verdictKey{1}
	c.put(hot, certVerdict{status: mtls.VerifySuccess})
	var junk verdictKey
	for i := 0; i < 3*maxCertVerdicts; i++ {
		binary.BigEndian.PutUint64(junk[8:], uint64(i))
		c.put(junk, certVerdict{status: mtls.VerifyUntrusted})
		if i%(maxCertVerdicts/2) == 0 {
			if _, ok := c.get(hot); !ok {
				t.Fatalf("a verdict in use was lost after %d others", i)
			}
		}
		if size := len(c.recent) + len(c.older); size > 2*maxCertVerdicts {
			t.Fatalf("cache grew to %d", size)
		}
	}
	binary.BigEndian.PutUint64(junk[8:], 0)
	if _, ok := c.get(junk); ok {
		t.Fatal("an unused verdict outlived two generations")
	}
}

func liveEntry(leaf *x509.Certificate, cancel func()) *liveClientAuth {
	return &liveClientAuth{
		cancel: cancel,
		conn:   &tls.ConnectionState{HandshakeComplete: true, ServerName: "auth.x.com", PeerCertificates: []*x509.Certificate{leaf}},
		domain: "x.com",
		host:   "auth.x.com",
	}
}

func TestCheckingLiveRequestsDoesNotHoldTheirLock(t *testing.T) {
	ca := newTestCA(t, "ca")
	p, domains, store := verdictFixture(t, ca)
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	store.Revoked = []string{fingerprintOf(cert)}
	p.UpdateClientAuth(domains, store)

	cancelled := make(chan struct{})
	// Registering or releasing a request takes this lock. Getting it from inside the
	// check proves a request would not have waited for the whole push.
	p.liveAuth[liveEntry(parsedLeaf(t, cert), func() {
		p.liveAuthMu.Lock()
		p.liveAuthMu.Unlock()
		close(cancelled)
	})] = struct{}{}

	go p.reauthorizeLive()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the check ran with the lock held")
	}
}

func TestRequestRegisteredDuringAPushIsCheckedAgain(t *testing.T) {
	ca := newTestCA(t, "ca")
	p, domains, store := verdictFixture(t, ca)
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	store.Certs[fingerprintOf(cert)] = []string{"auth.x.com"}
	p.UpdateClientAuth(domains, store)
	decidedUnder := p.clientAuth.Load()
	leaf := parsedLeaf(t, cert)

	cancelled := false
	p.trackLive(decidedUnder, liveEntry(leaf, func() { cancelled = true }), time.Now())
	if cancelled {
		t.Fatal("an unchanged table has nothing new to say")
	}

	store.Revoked = []string{fingerprintOf(cert)}
	p.UpdateClientAuth(domains, store)
	p.trackLive(decidedUnder, liveEntry(leaf, func() { cancelled = true }), time.Now())
	if !cancelled {
		t.Fatal("a WebSocket opened in that gap would outlive the revocation")
	}
}

func TestCertificateOnlyUpdateRevokesAndLeavesTLSCaches(t *testing.T) {
	ca := newTestCA(t, "ca")
	cert := ca.issue(t, "ali", time.Now().Add(time.Hour))
	fingerprint := fingerprintOf(cert)
	fx := newAuthFixture(t, ca, map[string][]string{fingerprint: {"auth.x.com"}}, nil)

	resp, _, err := fx.get(t, authClient{cert: &cert, sni: "auth.x.com"}, "auth.x.com", "/", nil)
	wantStatus(t, "before", resp, err, http.StatusOK)

	fx.proxy.mu.Lock()
	fx.proxy.sslCache["pub.com"] = &tls.Certificate{}
	fx.proxy.ocspCache["pub.com"] = &ocspCacheEntry{}
	fx.proxy.mu.Unlock()

	fx.proxy.UpdateClientCerts(&config.MTLS{CAs: fx.store.CAs, Certs: map[string][]string{}, Revoked: []string{fingerprint}})

	resp, _, err = fx.get(t, authClient{cert: &cert, sni: "auth.x.com"}, "auth.x.com", "/", nil)
	wantStatus(t, "after", resp, err, http.StatusForbidden)

	fx.proxy.mu.RLock()
	defer fx.proxy.mu.RUnlock()
	if fx.proxy.sslCache["pub.com"] == nil || fx.proxy.ocspCache["pub.com"] == nil {
		t.Fatal("a certificate change must not cost other sites their TLS caches")
	}
}
