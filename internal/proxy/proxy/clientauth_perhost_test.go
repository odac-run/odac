package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"odac/internal/mtls"
	"odac/internal/proxy/config"
)

// hostIssuer signs an issuer for host under ca, the way a server of a per-host
// record does: the shared name, a key of its own.
func (ca *testCA) hostIssuer(t *testing.T, host string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		RawSubject:            mtls.IssuerSubject(host, ca.cert),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// withChain is a client certificate that sends its issuer along, as a p12 does.
func withChain(leaf tls.Certificate, issuer *testCA) tls.Certificate {
	leaf.Certificate = append(leaf.Certificate, issuer.cert.Raw)
	return leaf
}

func perHostFixture(t *testing.T, ca *testCA, mode string, certs map[string][]string, issuers ...config.MTLSIssuer) *authFixture {
	t.Helper()
	fx := newAuthFixture(t, ca, certs, func(s *config.Website) {
		s.MTLS.Mode = mode
		s.MTLS.Issuer = mtls.IssuerPerHost
	})
	fx.store.Issuers = issuers
	fx.sync()
	return fx
}

func subjectsOf(conf *tls.Config) [][]byte {
	//lint:ignore SA1019 the pool holds only certificates added here, never system roots
	return conf.ClientCAs.Subjects()
}

func TestSilentHostAsksNothingAndKeepsHTTP3(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, map[string][]string{strings.Repeat("a", 64): {"auth.x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })

	if conf, err := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "public.x.com"}); conf != nil || err != nil {
		t.Fatalf("a host no certificate opens asked for one: conf=%v err=%v", conf != nil, err)
	}
	if conf, err := fx.proxy.GetHTTP3ConfigForClient(&tls.ClientHelloInfo{ServerName: "public.x.com"}); conf != nil || err != nil {
		t.Fatalf("a silent host lost HTTP/3: %v", err)
	}
	if _, err := fx.proxy.GetHTTP3ConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"}); err == nil {
		t.Fatal("a host a certificate opens accepted QUIC")
	}
	if conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"}); conf == nil || conf.ClientAuth != tls.RequestClientCert {
		t.Fatal("a host a certificate opens did not ask")
	}

	resp, _, err := fx.get(t, authClient{sni: "public.x.com"}, "public.x.com", "/", nil)
	wantStatus(t, "silent host", resp, err, http.StatusOK)
	if resp.Header.Get("Alt-Svc") == "" {
		t.Fatal("a silent host does not advertise HTTP/3")
	}
	if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != mtls.VerifyNone {
		t.Fatalf("verify = %q, the host still has a policy", got)
	}

	// Early data on a silent host proves nothing, and asks for nothing to prove.
	req := httptest.NewRequest(http.MethodGet, "https://public.x.com/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "public.x.com"}
	rec := httptest.NewRecorder()
	fx.proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("0-RTT GET on a silent host: %d", rec.Code)
	}
	// Unless it carries a resumed identity.
	leaf := parsedLeaf(t, ca.issue(t, "alice", time.Now().Add(time.Hour)))
	req.TLS = &tls.ConnectionState{ServerName: "public.x.com", PeerCertificates: []*x509.Certificate{leaf}}
	rec = httptest.NewRecorder()
	fx.proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooEarly {
		t.Fatalf("0-RTT with a certificate on a silent host: %d", rec.Code)
	}
}

func TestRequiredAndExternalHostsAlwaysAsk(t *testing.T) {
	ca := newTestCA(t, "ca")
	fx := newAuthFixture(t, ca, nil, nil)
	if conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"}); conf == nil || conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("a required host with no certificate must still ask")
	}

	fx = newAuthFixture(t, ca, nil, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	fx.store.CAs["main"] = config.MTLSCA{Cert: ca.pem, Managed: false}
	fx.sync()
	if conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"}); conf == nil {
		t.Fatal("an external CA's certificates are not counted, so its hosts must ask")
	}
}

func TestSilentConnectionCannotCarryAHostThatAsks(t *testing.T) {
	ca := newTestCA(t, "ca")
	alice := ca.issue(t, "alice", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(alice): {"auth.x.com"}}, func(s *config.Website) { s.MTLS.Mode = mtls.ModeOptional })
	// The connection for public.x.com never asked, so alice had no chance to show hers.
	resp, _, err := fx.get(t, authClient{sni: "public.x.com", cert: &alice}, "auth.x.com", "/", nil)
	wantStatus(t, "coalesced onto a silent host", resp, err, http.StatusMisdirectedRequest)
	resp, _, err = fx.get(t, authClient{sni: "auth.x.com", cert: &alice}, "auth.x.com", "/", nil)
	wantStatus(t, "own connection", resp, err, http.StatusOK)
	if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != mtls.VerifySuccess {
		t.Fatalf("verify = %q", got)
	}
}

func TestPerHostNamesOnlyTheHostsIssuer(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.hostIssuer(t, "auth.x.com")
	admin := ca.hostIssuer(t, "admin.x.com")
	fx := perHostFixture(t, ca, mtls.ModeOptional,
		map[string][]string{strings.Repeat("a", 64): {"auth.x.com"}, strings.Repeat("b", 64): {"admin.x.com"}},
		config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: auth.pem},
		config.MTLSIssuer{CA: "main", Host: "admin.x.com", Cert: admin.pem, Direct: true})

	conf, err := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"})
	if err != nil || conf == nil {
		t.Fatalf("auth.x.com did not ask: %v", err)
	}
	if got := subjectsOf(conf); len(got) != 1 || !bytes.Equal(got[0], auth.cert.RawSubject) {
		t.Fatalf("auth.x.com names %d CAs, want only its issuer", len(got))
	}
	if conf.ClientAuth != tls.RequestClientCert {
		t.Fatalf("client auth = %v", conf.ClientAuth)
	}

	// admin.x.com also has a certificate straight from the CA, so it names both.
	conf, _ = fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "admin.x.com"})
	if got := subjectsOf(conf); len(got) != 2 {
		t.Fatalf("admin.x.com names %d CAs, want its issuer and the CA", len(got))
	}

	if conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "public.x.com"}); conf != nil {
		t.Fatal("a host with no certificate asked for one")
	}
	if _, err := fx.proxy.GetHTTP3ConfigForClient(&tls.ClientHelloInfo{ServerName: "public.x.com"}); err != nil {
		t.Fatal("a host with no certificate lost HTTP/3")
	}
}

func TestPerHostRequiredLeavesTheChainToTheGate(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.hostIssuer(t, "auth.x.com")
	fx := perHostFixture(t, ca, mtls.ModeRequired, nil, config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: auth.pem})
	conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"})
	if conf == nil || conf.ClientAuth != tls.RequireAnyClientCert {
		t.Fatal("a required per-host host must demand a certificate without crypto/tls verifying it")
	}
	conf, _ = fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "public.x.com"})
	if conf == nil || conf.ClientAuth != tls.RequireAnyClientCert {
		t.Fatal("a required per-host host with no issuer must still ask, and still check per request")
	}
	resp, _, err := fx.get(t, authClient{sni: "auth.x.com"}, "auth.x.com", "/", nil)
	if err == nil {
		t.Fatalf("a required host answered %d without a certificate", resp.StatusCode)
	}
}

func TestPerHostAcceptsItsIssuerAndRefusesAnotherHosts(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.hostIssuer(t, "auth.x.com")
	admin := ca.hostIssuer(t, "admin.x.com")
	alice := withChain(auth.issue(t, "alice", time.Now().Add(time.Hour)), auth)
	bob := withChain(admin.issue(t, "bob", time.Now().Add(time.Hour)), admin)
	// bob's scope even names auth.x.com: the issuer alone must keep him out.
	fx := perHostFixture(t, ca, mtls.ModeRequired,
		map[string][]string{fingerprintOf(alice): {"auth.x.com"}, fingerprintOf(bob): {"admin.x.com", "auth.x.com"}},
		config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: auth.pem},
		config.MTLSIssuer{CA: "main", Host: "admin.x.com", Cert: admin.pem})

	resp, _, err := fx.get(t, authClient{sni: "auth.x.com", cert: &alice}, "auth.x.com", "/", nil)
	wantStatus(t, "alice on auth", resp, err, http.StatusOK)
	h := fx.lastHeaders()
	if h.Get(mtls.HeaderVerify) != mtls.VerifySuccess || h.Get(mtls.HeaderIssuer) != mtls.Fingerprint(ca.cert.Raw) {
		t.Fatalf("verify=%q issuer=%q, want success under the record's CA", h.Get(mtls.HeaderVerify), h.Get(mtls.HeaderIssuer))
	}

	// A client filters by the names the handshake gives, so bob's is not even offered.
	if _, _, err = fx.get(t, authClient{sni: "auth.x.com", cert: &bob}, "auth.x.com", "/", nil); err == nil {
		t.Fatal("a client offered a certificate whose issuer auth.x.com does not name")
	}
	resp, _, err = fx.get(t, authClient{sni: "auth.x.com", cert: &bob, force: true}, "auth.x.com", "/", nil)
	wantStatus(t, "bob on auth", resp, err, http.StatusForbidden)
	resp, _, err = fx.get(t, authClient{sni: "admin.x.com", cert: &bob}, "admin.x.com", "/", nil)
	wantStatus(t, "bob on admin", resp, err, http.StatusOK)

	// One connection per host: each names its own issuer.
	resp, _, err = fx.get(t, authClient{sni: "admin.x.com", cert: &bob, force: true}, "auth.x.com", "/", nil)
	wantStatus(t, "admin connection used for auth", resp, err, http.StatusMisdirectedRequest)
}

func TestPerHostAcceptsAnotherServersIssuerAndALeafAlone(t *testing.T) {
	ca := newTestCA(t, "ca")
	ours := ca.hostIssuer(t, "auth.x.com")
	theirs := ca.hostIssuer(t, "auth.x.com")
	carol := withChain(theirs.issue(t, "carol", time.Now().Add(time.Hour)), theirs)
	dave := ours.issue(t, "dave", time.Now().Add(time.Hour)) // no chain sent
	fx := perHostFixture(t, ca, mtls.ModeOptional,
		map[string][]string{fingerprintOf(carol): {"auth.x.com"}, fingerprintOf(dave): {"auth.x.com"}},
		config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: ours.pem})

	for label, cert := range map[string]tls.Certificate{"another server's issuer": carol, "leaf alone": dave} {
		resp, _, err := fx.get(t, authClient{sni: "auth.x.com", cert: &cert}, "auth.x.com", "/", nil)
		wantStatus(t, label, resp, err, http.StatusOK)
		if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != mtls.VerifySuccess {
			t.Fatalf("%s: verify = %q", label, got)
		}
	}

	// A leaf alone from another server's issuer cannot be checked here.
	lone := theirs.issue(t, "erin", time.Now().Add(time.Hour))
	fx.store.Certs[fingerprintOf(lone)] = []string{"auth.x.com"}
	fx.sync()
	resp, _, err := fx.get(t, authClient{sni: "auth.x.com", cert: &lone, force: true}, "auth.x.com", "/", nil)
	wantStatus(t, "unverifiable leaf", resp, err, http.StatusOK)
	if got := fx.lastHeaders().Get(mtls.HeaderVerify); got != mtls.VerifyUntrusted {
		t.Fatalf("verify = %q", got)
	}
}

func TestPerHostKeepsCertificatesStraightFromTheCA(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.hostIssuer(t, "auth.x.com")
	multi := ca.issue(t, "frank", time.Now().Add(time.Hour))
	fx := perHostFixture(t, ca, mtls.ModeRequired,
		map[string][]string{fingerprintOf(multi): {"auth.x.com", "admin.x.com"}},
		config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: auth.pem, Direct: true})
	for _, host := range []string{"auth.x.com", "admin.x.com"} {
		resp, _, err := fx.get(t, authClient{sni: host, cert: &multi}, host, "/", nil)
		wantStatus(t, host, resp, err, http.StatusOK)
	}
}

func TestPerHostIsIgnoredOnARecordThatIsNot(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth := ca.hostIssuer(t, "auth.x.com")
	viaIssuer := withChain(auth.issue(t, "alice", time.Now().Add(time.Hour)), auth)
	direct := ca.issue(t, "bob", time.Now().Add(time.Hour))
	fx := newAuthFixture(t, ca, map[string][]string{fingerprintOf(viaIssuer): {"auth.x.com"}, fingerprintOf(direct): {"auth.x.com"}}, nil)
	fx.store.Issuers = []config.MTLSIssuer{{CA: "main", Host: "auth.x.com", Cert: auth.pem}}
	fx.sync()

	conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"})
	if conf == nil || conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("a record-mode host changed how it asks")
	}
	if got := subjectsOf(conf); len(got) != 1 || !bytes.Equal(got[0], ca.cert.RawSubject) {
		t.Fatal("a record-mode host must name its CA and nothing else")
	}
	resp, _, err := fx.get(t, authClient{sni: "auth.x.com", cert: &direct}, "auth.x.com", "/", nil)
	wantStatus(t, "direct", resp, err, http.StatusOK)
}

func TestIssuerOfTheWrongNameIsNotNamed(t *testing.T) {
	ca := newTestCA(t, "ca")
	admin := ca.hostIssuer(t, "admin.x.com")
	fx := perHostFixture(t, ca, mtls.ModeOptional, map[string][]string{strings.Repeat("a", 64): {"auth.x.com"}},
		config.MTLSIssuer{CA: "main", Host: "auth.x.com", Cert: admin.pem})
	conf, _ := fx.proxy.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "auth.x.com"})
	if conf == nil {
		t.Fatal("auth.x.com has a certificate and must ask")
	}
	if got := subjectsOf(conf); len(got) != 1 || !bytes.Equal(got[0], ca.cert.RawSubject) {
		t.Fatal("an issuer named for another host was put in the handshake")
	}
}
