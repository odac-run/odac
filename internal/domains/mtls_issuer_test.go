package domains

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/mtls"
)

func pemCerts(t *testing.T, raw string) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	rest := []byte(raw)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cert)
	}
}

func perHostNode(t *testing.T) *mtlsFixture {
	t.Helper()
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": mtls.IssuerPerHost}))
	return fx
}

func (fx *mtlsFixture) caOf(t *testing.T, record string) *x509.Certificate {
	t.Helper()
	id := stringList(fx.record(record)["ca"])[0]
	return pemCerts(t, fx.m.caPEM(id))[0]
}

func (fx *mtlsFixture) issuerRows(t *testing.T) []mtls.HostIssuer {
	t.Helper()
	store, err := fx.m.certs()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.issuers()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestIssuerSettingIsValidatedAndListed(t *testing.T) {
	fx := perHostNode(t)
	if fx.record("x.com")["issuer"] != mtls.IssuerPerHost {
		t.Fatalf("record = %v", fx.record("x.com"))
	}
	var view map[string]any
	fx.cfg.View(func() { view, _ = mtlsListView(fx.record("x.com"), fx.m.casLocked(false)).(map[string]any) })
	if view["issuer"] != mtls.IssuerPerHost {
		t.Fatalf("domain.list view = %v", view)
	}

	mustFail(t, fx.m.Set("auth.x.com", "required", map[string]any{"issuer": mtls.IssuerPerHost}), "whole record")
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": "chain"}), "record or per-host")
	mustFail(t, fx.m.Set("x.com", "off", map[string]any{"issuer": mtls.IssuerRecord}), "together with")

	// Leaving it out keeps it; off keeps it too, next to the CA it belongs to.
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	mustOK(t, fx.m.Set("x.com", "off", nil))
	if fx.record("x.com")["issuer"] != mtls.IssuerPerHost {
		t.Fatalf("issuer lost: %v", fx.record("x.com"))
	}
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": mtls.IssuerRecord}))
	if _, has := fx.record("x.com")["issuer"]; has {
		t.Fatal("the default must be stored as its absence")
	}

	// A record left at the default never grows the member.
	mustOK(t, fx.m.Set("y.com", "optional", nil))
	if _, has := fx.record("y.com")["issuer"]; has {
		t.Fatal("a record-mode record grew an issuer member")
	}
}

func TestPerHostNeedsAManagedCA(t *testing.T) {
	fx := newMTLSFixture(t)
	casBefore := 0
	fx.cfg.View(func() { casBefore = len(fx.m.casLocked(false)) })
	mustFail(t, fx.m.Set("y.com", "optional", map[string]any{"issuer": mtls.IssuerPerHost, "caPem": newExternalCA(t, true)}), "per-host")
	fx.cfg.View(func() {
		if len(fx.m.casLocked(false)) != casBefore {
			t.Fatal("a refused per-host set left a CA behind")
		}
	})

	px := perHostNode(t)
	mustFail(t, px.m.Set("auth.x.com", "required", map[string]any{"caPem": newExternalCA(t, true)}), "per-host")

	ex := newMTLSFixture(t)
	mustOK(t, ex.m.Set("y.com", "optional", map[string]any{"caPem": newExternalCA(t, true)}))
	mustFail(t, ex.m.Set("y.com", "optional", map[string]any{"issuer": mtls.IssuerPerHost}), "is external")
}

func TestPerHostIssueSignsThroughTheHostsIssuer(t *testing.T) {
	logs := captureLogs(t)
	fx := perHostNode(t)
	ca := fx.caOf(t, "x.com")
	csr := newCSR(t)
	reply := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"auth.x.com"}, "csr": csr})).Data.(map[string]any)

	leaf := parseIssued(t, reply)
	chain := pemCerts(t, reply["chain"].(string))
	if len(chain) != 2 || !chain[1].Equal(ca) {
		t.Fatalf("chain has %d certificates, want the issuer then the CA", len(chain))
	}
	issuer := chain[0]
	if !bytes.Equal(issuer.RawSubject, mtls.IssuerSubject("auth.x.com", ca)) || !bytes.Equal(leaf.RawIssuer, issuer.RawSubject) {
		t.Fatalf("issuer subject = %s", issuer.Subject)
	}
	if !issuer.IsCA || !issuer.MaxPathLenZero || issuer.MaxPathLen != 0 || issuer.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("the issuer must be a CA limited to signing leaves")
	}
	if _, _, status := mtls.VerifyPerHost([]*x509.Certificate{leaf, issuer}, nil, []*x509.Certificate{ca}, "auth.x.com", time.Now()); status != mtls.VerifySuccess {
		t.Fatalf("the proxy would say %s", status)
	}

	// A retry hands back the same chain; a second certificate reuses the issuer.
	again := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"auth.x.com"}, "csr": csr})).Data.(map[string]any)
	if again["fingerprint"] != reply["fingerprint"] || again["chain"] != reply["chain"] {
		t.Fatal("a retry returned a different chain")
	}
	other := fx.issue(t, "x.com", "veli", "auth.x.com")
	if other["chain"] != reply["chain"] || len(fx.issuerRows(t)) != 1 {
		t.Fatal("a second certificate for the host must reuse its issuer")
	}

	// Wider than one host stays with the CA.
	for _, hosts := range [][]string{{"auth.x.com", "admin.x.com"}, {"*.x.com"}} {
		wide := fx.issue(t, "x.com", "wide-"+hosts[0], hosts...)
		if got := pemCerts(t, wide["chain"].(string)); len(got) != 1 || !got[0].Equal(ca) {
			t.Fatalf("%v: chain has %d certificates", hosts, len(got))
		}
		if !bytes.Equal(parseIssued(t, wide).RawIssuer, ca.RawSubject) {
			t.Fatalf("%v: not signed by the CA", hosts)
		}
	}

	// The proxy gets the issuer, and is told the CA has a live certificate there too.
	snap, err := fx.m.CertSnapshot(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Issuers) != 1 || snap.Issuers[0].Host != "auth.x.com" || !snap.Issuers[0].Direct || snap.Issuers[0].Cert != string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw})) {
		t.Fatalf("issuers = %+v", snap.Issuers)
	}

	// The key is a 0600 file, never in config, never in a log.
	info, err := os.Stat(fx.m.issuerKeyPath(stringList(fx.record("x.com")["ca"])[0], "auth.x.com"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("issuer key: %v %v", info, err)
	}
	raw, err := os.ReadFile(filepath.Join(fx.base, "config", "mtls.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"PRIVATE KEY", "auth.x.com"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("config carries %q", leaked)
		}
	}
	if strings.Contains(logs.String(), "BEGIN") {
		t.Fatal("a PEM reached the log")
	}

	// A record left at the default is untouched.
	plain := fx.issue(t, "y.com", "ali", "y.com")
	if got := pemCerts(t, plain["chain"].(string)); len(got) != 1 {
		t.Fatalf("record mode chain has %d certificates", len(got))
	}
}

func TestPerHostCertificateStaysOnItsHost(t *testing.T) {
	fx := perHostNode(t)
	fp := fx.issue(t, "x.com", "ali", "auth.x.com")["fingerprint"]
	mustFail(t, fx.m.Scope(fp, []any{"admin.x.com"}), "signed for auth.x.com alone")
	mustFail(t, fx.m.Scope(fp, []any{"auth.x.com", "admin.x.com"}), "signed for auth.x.com alone")
	mustOK(t, fx.m.Scope(fp, []any{"auth.x.com"}))

	wide := fx.issue(t, "x.com", "veli", "auth.x.com", "admin.x.com")["fingerprint"]
	mustOK(t, fx.m.Scope(wide, []any{"admin.x.com"}))
}

func TestLeavingPerHostWaitsForItsCertificates(t *testing.T) {
	fx := perHostNode(t)
	caID := stringList(fx.record("x.com")["ca"])[0]
	fp := fx.issue(t, "x.com", "ali", "auth.x.com")["fingerprint"]
	mustFail(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": mtls.IssuerRecord}), "auth.x.com")

	mustOK(t, fx.m.Revoke(api.Operator, fp))
	if rows := fx.issuerRows(t); len(rows) != 0 {
		t.Fatalf("the issuer outlived its last certificate: %+v", rows)
	}
	if _, err := os.Stat(fx.m.issuerKeyPath(caID, "auth.x.com")); !os.IsNotExist(err) {
		t.Fatalf("the issuer key is still on disk: %v", err)
	}
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": mtls.IssuerRecord}))
}

func TestIdleIssuersLeaveTheSnapshotAndTheDisk(t *testing.T) {
	fx := perHostNode(t)
	fx.issue(t, "x.com", "ali", "auth.x.com")
	future := time.Now().Add(2 * 365 * 24 * time.Hour)
	snap, err := fx.m.CertSnapshot(future)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Issuers) != 0 || string(snap.Scopes) != "{}" {
		t.Fatalf("an expired certificate still makes its host ask: issuers=%d scopes=%s", len(snap.Issuers), snap.Scopes)
	}

	// The domain goes: Prune takes the certificate and then the issuer.
	fx.cfg.Mutate(func() { delete(fx.cfg.Map("domains"), "x.com") })
	fx.m.Prune()
	if rows := fx.issuerRows(t); len(rows) != 0 {
		t.Fatalf("a deleted domain left issuers: %+v", rows)
	}
}

// verifyHost is proxyEdge.verify for a host other than x.com.
func (e *proxyEdge) verifyHost(t *testing.T, cert tls.Certificate, host string) string {
	t.Helper()
	addr := e.front.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: host, Certificates: []tls.Certificate{cert}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func clientCert(t *testing.T, reply map[string]any, key *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	cert := tls.Certificate{Certificate: [][]byte{parseIssued(t, reply).Raw}, PrivateKey: key}
	for _, c := range pemCerts(t, reply["chain"].(string)) {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return cert
}

// K18 with per-host issuers: each node signs with an issuer of its own under the
// same name, an import needs the chain, and a certificate made on either node
// (renewals included) opens the host on both.
func TestPerHostCertificatesWorkOnEveryNode(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	for _, fx := range []*mtlsFixture{a, b} {
		mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{ca.id()}, "issuer": mtls.IssuerPerHost}))
	}
	csr, key := csrWithKey(t)
	reply := mustOK(t, a.m.Issue(api.Operator, "x.com", map[string]any{"name": "dev1", "hosts": []any{"auth.x.com"}, "csr": csr})).Data.(map[string]any)
	client := clientCert(t, reply, key)

	mustFail(t, b.m.Import(asApp, importOpts(reply["cert"].(string), "auth.x.com")), "together with its chain")
	mustOK(t, b.m.Import(asApp, importOpts(reply["cert"].(string)+reply["chain"].(string), "auth.x.com")))
	aIssuer, bIssuer := a.issuerRows(t), b.issuerRows(t)
	if len(bIssuer) != 1 || bIssuer[0].Cert == aIssuer[0].Cert {
		t.Fatal("the import must leave b with an issuer of its own")
	}
	if !bytes.Equal(pemCerts(t, aIssuer[0].Cert)[0].RawSubject, pemCerts(t, bIssuer[0].Cert)[0].RawSubject) {
		t.Fatal("the two nodes' issuers must share their name")
	}

	edgeA, edgeB := newProxyEdge(t, a), newProxyEdge(t, b)
	for name, edge := range map[string]*proxyEdge{"a": edgeA, "b": edgeB} {
		if got := edge.verifyHost(t, client, "auth.x.com"); got != mtls.VerifySuccess {
			t.Fatalf("edge %s says %q", name, got)
		}
	}

	// Renewed on b, signed by b's issuer, imported back to a.
	csr2, key2 := csrWithKey(t)
	renewed := mustOK(t, b.m.Issue(api.Operator, "x.com", map[string]any{"renews": reply["fingerprint"], "csr": csr2})).Data.(map[string]any)
	if pemCerts(t, renewed["chain"].(string))[0].Equal(pemCerts(t, reply["chain"].(string))[0]) {
		t.Fatal("b must sign the renewal with its own issuer")
	}
	mustOK(t, a.m.Import(asApp, importOpts(renewed["cert"].(string)+renewed["chain"].(string), "auth.x.com")))
	edgeA.sync(t)
	edgeB.sync(t)
	second := clientCert(t, renewed, key2)
	for name, edge := range map[string]*proxyEdge{"a": edgeA, "b": edgeB} {
		for label, cert := range map[string]tls.Certificate{"original": client, "renewal": second} {
			if got := edge.verifyHost(t, cert, "auth.x.com"); got != mtls.VerifySuccess {
				t.Fatalf("edge %s, %s: %q", name, label, got)
			}
		}
	}

	// The host's certificate does not open another host, wherever it was made.
	mustOK(t, a.m.Issue(api.Operator, "x.com", map[string]any{"name": "dev2", "hosts": []any{"admin.x.com"}, "csr": newCSR(t)}))
	edgeA.sync(t)
	if got := edgeA.verifyHost(t, client, "admin.x.com"); got == mtls.VerifySuccess {
		t.Fatal("auth.x.com's certificate opened admin.x.com")
	}
}

func TestImportRefusesAPerHostLeafWhereItCannotWork(t *testing.T) {
	ca := newSharedCA(t, x509.KeyUsageCertSign)
	a, b := edgeNode(t, ca), edgeNode(t, ca)
	mustOK(t, a.m.Set("x.com", "optional", map[string]any{"ca": []any{ca.id()}, "issuer": mtls.IssuerPerHost}))
	reply := a.issue(t, "x.com", "dev1", "auth.x.com")
	full := reply["cert"].(string) + reply["chain"].(string)

	// b's record is not per-host: nothing there would accept the chain.
	mustFail(t, b.m.Import(asApp, importOpts(full, "auth.x.com")), "not signed by a client CA")
	mustOK(t, b.m.Set("x.com", "optional", map[string]any{"ca": []any{ca.id()}, "issuer": mtls.IssuerPerHost}))
	// It opens its issuer's host and nothing else.
	mustFail(t, b.m.Import(asApp, importOpts(full, "admin.x.com")), "not signed by a client CA")
	mustFail(t, b.m.Import(asApp, importOpts(full, "auth.x.com", "admin.x.com")), "not signed by a client CA")
	mustOK(t, b.m.Import(asApp, importOpts(full, "auth.x.com")))
}

func TestCertStoreGainsTheIssuerColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "certs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE certs (fingerprint TEXT PRIMARY KEY, ca TEXT, cert TEXT, created INTEGER, hosts TEXT,
		name TEXT, not_after INTEGER, renewed_from TEXT, revoked INTEGER, serial TEXT, spki TEXT);
		INSERT INTO certs (fingerprint, ca, hosts, name, not_after) VALUES ('fp', 'c1', '["a.x.com"]', 'ali', 99999999999999)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := openCertStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	rec, err := store.get("fp")
	if err != nil || rec["name"] != "ali" {
		t.Fatalf("old row: %v %v", rec, err)
	}
	if _, has := rec["issuerHost"]; has {
		t.Fatal("an old row grew an issuer")
	}
	snap, err := store.snapshot(time.Now())
	if err != nil || string(snap.Scopes) != `{"fp":["a.x.com"]}` || snap.Issuers != nil {
		t.Fatalf("snapshot = %s %+v %v", snap.Scopes, snap.Issuers, err)
	}
}
