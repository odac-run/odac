package mtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

type signer struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func sign(t *testing.T, tmpl *x509.Certificate, parent *signer) *signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl.SerialNumber = serial
	parentCert, parentKey := tmpl, key
	if parent != nil {
		parentCert, parentKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{cert: cert, key: key}
}

// newRootCA is a CA the way ODAC creates a managed one: pathlen 1.
func newRootCA(t *testing.T, name string) *signer {
	return newRootCAWithPathLen(t, name, 1)
}

// newRootCAWithPathLen takes -1 for no path length constraint.
func newRootCAWithPathLen(t *testing.T, name string, pathLen int) *signer {
	return sign(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: pathLen, MaxPathLenZero: pathLen == 0, KeyUsage: x509.KeyUsageCertSign,
	}, nil)
}

func issuerTemplate(host string, ca *x509.Certificate) *x509.Certificate {
	return &x509.Certificate{
		RawSubject: IssuerSubject(host, ca), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

func leafTemplate(cn string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

func verifyStatus(chain, extra []*x509.Certificate, ca *signer, host string) string {
	_, _, status := VerifyPerHost(chain, extra, []*x509.Certificate{ca.cert}, host, time.Now())
	return status
}

func TestIssuerNameIsDeterministicAndCarriesTheCA(t *testing.T) {
	ca := newRootCA(t, "ODAC Client CA x.com")
	a, b := IssuerSubject("a.x.com", ca.cert), IssuerSubject("a.x.com", ca.cert)
	if !bytes.Equal(a, b) || bytes.Equal(a, IssuerSubject("b.x.com", ca.cert)) {
		t.Fatal("issuer names must be per host and identical on every call")
	}
	issuer := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
	if issuer.cert.Subject.CommonName != "a.x.com" || len(issuer.cert.Subject.Organization) != 1 || issuer.cert.Subject.Organization[0] != "ODAC Client CA x.com" {
		t.Fatalf("subject = %s", issuer.cert.Subject)
	}
	nameless := sign(t, &x509.Certificate{NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil)
	if org := IssuerName("a.x.com", nameless.cert).Organization[0]; org != "Client CA "+Fingerprint(nameless.cert.Raw)[:16] {
		t.Fatalf("organization of a CA with no name = %q", org)
	}
}

func TestPerHostIssuable(t *testing.T) {
	for host, want := range map[string]bool{
		"a.x.com": true, "x.com": true, "*.x.com": false, "localhost": false, "a..x.com": false,
		"-a.x.com": false, "A.x.com": false, string(make([]byte, 70)): false,
		"abcdefghij.abcdefghij.abcdefghij.abcdefghij.abcdefghij.abcdefghi":  true, // 64
		"abcdefghij.abcdefghij.abcdefghij.abcdefghij.abcdefghij.abcdefghij": false,
	} {
		if got := PerHostIssuable(host); got != want {
			t.Errorf("PerHostIssuable(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestVerifyPerHostAcceptsTheHostsIssuer(t *testing.T) {
	ca := newRootCA(t, "ca")
	issuer := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
	leaf := sign(t, leafTemplate("alice"), issuer)
	got, until, status := VerifyPerHost([]*x509.Certificate{leaf.cert, issuer.cert}, nil, []*x509.Certificate{ca.cert}, "a.x.com", time.Now())
	if status != VerifySuccess || got != ca.cert {
		t.Fatalf("status = %s", status)
	}
	if !until.Equal(leaf.cert.NotAfter) {
		t.Fatalf("until = %v, want the leaf's end %v", until, leaf.cert.NotAfter)
	}
	// A client that sends its leaf alone is checked against this server's issuer.
	if s := verifyStatus([]*x509.Certificate{leaf.cert}, []*x509.Certificate{issuer.cert}, ca, "a.x.com"); s != VerifySuccess {
		t.Fatalf("leaf alone with the local issuer: %s", s)
	}
	if s := verifyStatus([]*x509.Certificate{leaf.cert}, nil, ca, "a.x.com"); s != VerifyUntrusted {
		t.Fatalf("leaf alone with no issuer anywhere: %s", s)
	}
}

func TestVerifyPerHostAcceptsAnotherServersIssuerOfTheSameName(t *testing.T) {
	ca := newRootCA(t, "ca")
	ours := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
	theirs := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
	if !bytes.Equal(ours.cert.RawSubject, theirs.cert.RawSubject) || ours.key.Equal(theirs.key) {
		t.Fatal("fixture: two servers' issuers must share the name and not the key")
	}
	leaf := sign(t, leafTemplate("alice"), theirs)
	if s := verifyStatus([]*x509.Certificate{leaf.cert, theirs.cert}, []*x509.Certificate{ours.cert}, ca, "a.x.com"); s != VerifySuccess {
		t.Fatalf("status = %s", s)
	}
}

func TestVerifyPerHostRefusesAnotherHostsIssuer(t *testing.T) {
	ca := newRootCA(t, "ca")
	other := sign(t, issuerTemplate("b.x.com", ca.cert), ca)
	leaf := sign(t, leafTemplate("alice"), other)
	if s := verifyStatus([]*x509.Certificate{leaf.cert, other.cert}, nil, ca, "a.x.com"); s != VerifyUntrusted {
		t.Fatalf("a b.x.com issuer opened a.x.com: %s", s)
	}
	if s := verifyStatus([]*x509.Certificate{leaf.cert, other.cert}, nil, ca, "b.x.com"); s != VerifySuccess {
		t.Fatalf("on its own host: %s", s)
	}
}

func TestVerifyPerHostEnforcesTheIssuersShape(t *testing.T) {
	ca := newRootCA(t, "ca")
	cases := map[string]func(*x509.Certificate){
		"no pathlen":   func(c *x509.Certificate) { c.MaxPathLenZero = false; c.MaxPathLen = -1 },
		"pathlen 1":    func(c *x509.Certificate) { c.MaxPathLenZero = false; c.MaxPathLen = 1 },
		"server eku":   func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"no cert sign": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature },
		"expired": func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(-3 * time.Hour)
			c.NotAfter = time.Now().Add(-time.Hour)
		},
		"other org name": func(c *x509.Certificate) {
			c.RawSubject = nil
			c.Subject = pkix.Name{CommonName: "a.x.com", Organization: []string{"someone"}}
		},
	}
	for name, mutate := range cases {
		tmpl := issuerTemplate("a.x.com", ca.cert)
		mutate(tmpl)
		issuer := sign(t, tmpl, ca)
		leafTmpl := leafTemplate("alice")
		leafTmpl.NotBefore = issuer.cert.NotBefore
		leaf := sign(t, leafTmpl, issuer)
		want := VerifyUntrusted
		if name == "expired" {
			want = VerifyExpired
		}
		if s := verifyStatus([]*x509.Certificate{leaf.cert, issuer.cert}, nil, ca, "a.x.com"); s != want {
			t.Errorf("%s: status = %s, want %s", name, s, want)
		}
	}

	// Not a CA at all: an ordinary certificate with the right name cannot sign.
	plain := sign(t, &x509.Certificate{RawSubject: IssuerSubject("a.x.com", ca.cert), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}, ca)
	leaf := sign(t, leafTemplate("alice"), plain)
	if s := verifyStatus([]*x509.Certificate{leaf.cert, plain.cert}, nil, ca, "a.x.com"); s != VerifyUntrusted {
		t.Errorf("non-CA issuer: %s", s)
	}

	// The right name, signed by someone else's CA.
	stranger := newRootCA(t, "ca")
	forged := sign(t, issuerTemplate("a.x.com", ca.cert), stranger)
	leaf = sign(t, leafTemplate("alice"), forged)
	if s := verifyStatus([]*x509.Certificate{leaf.cert, forged.cert}, nil, ca, "a.x.com"); s != VerifyUntrusted {
		t.Errorf("issuer signed by another CA: %s", s)
	}
}

func TestVerifyPerHostEnforcesTheLeaf(t *testing.T) {
	ca := newRootCA(t, "ca")
	issuer := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
	check := func(label, want string, mutate func(*x509.Certificate)) {
		t.Helper()
		tmpl := leafTemplate("alice")
		mutate(tmpl)
		leaf := sign(t, tmpl, issuer)
		if s := verifyStatus([]*x509.Certificate{leaf.cert, issuer.cert}, nil, ca, "a.x.com"); s != want {
			t.Errorf("%s: status = %s, want %s", label, s, want)
		}
	}
	check("server only", VerifyUntrusted, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} })
	check("no eku", VerifyUntrusted, func(c *x509.Certificate) { c.ExtKeyUsage = nil })
	check("ca leaf", VerifyUntrusted, func(c *x509.Certificate) {
		c.IsCA, c.BasicConstraintsValid, c.KeyUsage = true, true, x509.KeyUsageCertSign
	})
	check("expired", VerifyExpired, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) })
	check("not yet valid", VerifyExpired, func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) })
}

func TestVerifyPerHostAcceptsALeafStraightFromTheCA(t *testing.T) {
	ca := newRootCA(t, "ca")
	leaf := sign(t, leafTemplate("alice"), ca)
	if s := verifyStatus([]*x509.Certificate{leaf.cert}, nil, ca, "a.x.com"); s != VerifySuccess {
		t.Fatalf("direct leaf: %s", s)
	}
	stranger := newRootCA(t, "ca")
	foreign := sign(t, leafTemplate("alice"), stranger)
	if s := verifyStatus([]*x509.Certificate{foreign.cert}, nil, ca, "a.x.com"); s != VerifyUntrusted {
		t.Fatalf("leaf of a same-named CA: %s", s)
	}
}

// The CA's own path length is applied like any chain builder applies it: a
// pathlen-0 CA can sign leaves only, so no chain through an issuer under it holds.
func TestVerifyPerHostEnforcesTheCAsPathLength(t *testing.T) {
	for _, tc := range []struct {
		pathLen int
		want    string
	}{{0, VerifyUntrusted}, {1, VerifySuccess}, {-1, VerifySuccess}} {
		ca := newRootCAWithPathLen(t, "ca", tc.pathLen)
		if got := AllowsHostIssuer(ca.cert); got != (tc.want == VerifySuccess) {
			t.Errorf("pathlen %d: AllowsHostIssuer = %v", tc.pathLen, got)
		}
		issuer := sign(t, issuerTemplate("a.x.com", ca.cert), ca)
		leaf := sign(t, leafTemplate("alice"), issuer)
		if s := verifyStatus([]*x509.Certificate{leaf.cert, issuer.cert}, nil, ca, "a.x.com"); s != tc.want {
			t.Errorf("pathlen %d via issuer: %s, want %s", tc.pathLen, s, tc.want)
		}
		// A leaf straight from the CA is a path with no intermediate, valid under any pathlen.
		direct := sign(t, leafTemplate("bob"), ca)
		if s := verifyStatus([]*x509.Certificate{direct.cert}, nil, ca, "a.x.com"); s != VerifySuccess {
			t.Errorf("pathlen %d direct: %s", tc.pathLen, s)
		}
	}
}
