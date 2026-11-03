package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/apiproto"
	"odac/internal/config"
	"odac/internal/domains"
)

// mtlsServer runs the real API server with the real action table, so an app
// token's reach is checked through the wiring production uses rather than
// through the domains package alone.
type mtlsServer struct {
	client *apiproto.Client
	mtls   *domains.MTLS
	root   string
	app    string
}

func newMTLSServer(t *testing.T, grant any) *mtlsServer {
	t.Helper()
	// Short: the API also binds a unix socket under the base dir.
	base, err := os.MkdirTemp("", "odacsrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	cfg, err := config.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mutate(func() {
		cfg.Set("apps", []any{
			map[string]any{"name": "edge", "active": true, "api": grant},
			map[string]any{"name": "shop", "active": true},
		})
		cfg.Set("domains", map[string]any{
			"edge.test": map[string]any{"appId": "edge", "subdomain": []any{"*"}},
			"shop.test": map[string]any{"appId": "shop", "subdomain": []any{"www"}},
		})
	})

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	srv := api.NewServer(cfg)
	srv.Addr = addr
	srv.Init()
	mtlsSvc := domains.NewMTLS(cfg, nil)
	t.Cleanup(mtlsSvc.Stop)
	registerActions(srv, nil, nil, nil, nil, nil, nil, mtlsSvc, nil, nil)
	srv.Start()
	t.Cleanup(srv.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for !apiproto.Ping(addr, 100*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("API never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var root string
	cfg.View(func() { root, _ = cfg.Map("api")["auth"].(string) })
	for _, domain := range []string{"edge.test", "shop.test"} {
		if r := mtlsSvc.Set(domain, "optional", nil); !r.Status {
			t.Fatalf("set %s: %v", domain, r.Message)
		}
	}
	return &mtlsServer{
		client: &apiproto.Client{Addr: addr},
		mtls:   mtlsSvc,
		root:   root,
		app:    srv.GenerateAppToken("edge", nil),
	}
}

func (s *mtlsServer) call(t *testing.T, token, action string, data ...any) *apiproto.Response {
	t.Helper()
	resp, err := s.client.Call(apiproto.Request{Auth: token, Action: action, Data: data}, nil)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return resp
}

func serverCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func issueArgs(t *testing.T, domain, name string, hosts ...string) []any {
	list := make([]any, len(hosts))
	for i, h := range hosts {
		list[i] = h
	}
	return []any{domain, map[string]any{"name": name, "hosts": list, "csr": serverCSR(t)}}
}

func TestAppTokenReachesOnlyItsOwnClientCertificates(t *testing.T) {
	s := newMTLSServer(t, []any{"domain.mtls.cert.issue", "domain.mtls.cert.revoke", "domain.mtls.cert.list"})

	mine := s.call(t, s.app, "domain.mtls.cert.issue", issueArgs(t, "edge.test", "dev1", "a.edge.test")...)
	if !mine.Result {
		t.Fatalf("own record: %s", mine.Message)
	}
	if r := s.call(t, s.app, "domain.mtls.cert.issue", issueArgs(t, "shop.test", "dev2", "shop.test")...); r.Result {
		t.Fatal("an app issued on another app's domain")
	}
	if r := s.call(t, s.app, "domain.mtls.cert.issue", issueArgs(t, "edge.test", "dev3", "a.edge.test", "shop.test")...); r.Result {
		t.Fatal("one foreign host in the list must refuse the whole request")
	}

	theirs := s.call(t, s.root, "domain.mtls.cert.issue", issueArgs(t, "shop.test", "staff", "shop.test")...)
	if !theirs.Result {
		t.Fatalf("root issue: %s", theirs.Message)
	}
	var issued struct {
		Fingerprint string `json:"fingerprint"`
	}
	json.Unmarshal(theirs.Data, &issued)
	r := s.call(t, s.app, "domain.mtls.cert.revoke", issued.Fingerprint)
	if r.Result || !strings.Contains(string(r.Data), `"code":"not_found"`) {
		t.Fatalf("revoking another app's certificate = %+v", r)
	}

	r = s.call(t, s.app, "domain.mtls.cert.list")
	if !r.Result || !strings.Contains(string(r.Data), "dev1") || strings.Contains(string(r.Data), "staff") {
		t.Fatalf("list = %+v", r)
	}
	if r = s.call(t, s.root, "domain.mtls.cert.list"); !strings.Contains(string(r.Data), "staff") {
		t.Fatalf("root list = %+v", r)
	}
}

// A3 through the real table: a grant naming set or scope, even one hand-edited
// past SetAPI, gets permission_denied before any handler runs. app.api needs an
// app manager to be registered at all; internal/api pins it the same way.
func TestAppTokenCannotChangePolicyWhateverItsGrant(t *testing.T) {
	for _, grant := range []any{true, []any{"*"}, []any{"domain.mtls.set", "domain.mtls.cert.scope", "app.api"}} {
		s := newMTLSServer(t, grant)
		for _, req := range []struct {
			action string
			data   []any
		}{
			{"domain.mtls.set", []any{"shop.test", "off"}},
			{"domain.mtls.set", []any{"edge.test", "off"}},
			{"domain.mtls.cert.scope", []any{strings.Repeat("ab", 32), []any{"shop.test"}}},
		} {
			if r := s.call(t, s.app, req.action, req.data...); r.Result || r.Message != "permission_denied" {
				t.Errorf("%s with grant %v = %+v", req.action, grant, r)
			}
		}
		if r := s.call(t, s.root, "domain.mtls.set", "shop.test", "off"); !r.Result {
			t.Fatalf("root set: %s", r.Message)
		}
	}
}
