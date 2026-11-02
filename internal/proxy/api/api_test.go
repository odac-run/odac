package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"odac/internal/proxy/config"
	"odac/internal/proxy/proxy"
)

// The wire shape the server sends. A field the decoder does not map would leave
// the host silently public, so this goes through real JSON rather than structs.
const clientAuthPush = `{
  "domains": {"x.com": {"domain": "x.com", "port": 1, "subdomain": ["auth"], "cert": {}, "containerIP": "127.0.0.1",
    "mtls": {"mode": "required", "ca": ["c1"], "key": "k",
      "hosts": {"auth.x.com": {"mode": "optional"}, "open.x.com": {"mode": "off"}}}}},
  "firewall": {"enabled": false},
  "mtls": {"cas": {"c1": {"cert": "", "managed": true}}, "certs": {"fp": ["auth.x.com"]}, "revoked": ["gone"]},
  "ssl": null,
  "tunnels": []
}`

func TestConfigPushInstallsClientAuthBeforeServing(t *testing.T) {
	p := proxy.NewProxy()
	p.SetTLSBase(&tls.Config{})
	s := NewServer(p, proxy.NewFirewall(config.Firewall{}), &Readiness{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(clientAuthPush)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	cases := map[string]tls.ClientAuthType{
		"x.com":      tls.RequireAndVerifyClientCert,
		"auth.x.com": tls.RequestClientCert,
	}
	for host, want := range cases {
		conf, err := p.GetConfigForClient(&tls.ClientHelloInfo{ServerName: host})
		if err != nil || conf == nil {
			t.Fatalf("%s: conf=%v err=%v", host, conf, err)
		}
		if conf.ClientAuth != want {
			t.Errorf("%s: ClientAuth = %v, want %v", host, conf.ClientAuth, want)
		}
		if conf.ClientCAs == nil {
			t.Errorf("%s: nil ClientCAs would trust the host's roots", host)
		}
	}
	if conf, err := p.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "open.x.com"}); err != nil || conf != nil {
		t.Errorf("open.x.com: conf=%v err=%v", conf, err)
	}

	var decoded config.Config
	if err := json.Unmarshal([]byte(clientAuthPush), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.MTLS == nil || decoded.MTLS.Certs["fp"][0] != "auth.x.com" || decoded.MTLS.Revoked[0] != "gone" {
		t.Fatalf("store = %+v", decoded.MTLS)
	}
}

func TestClientCertPushChangesOnlyTheCertificates(t *testing.T) {
	p := proxy.NewProxy()
	p.SetTLSBase(&tls.Config{})
	s := NewServer(p, proxy.NewFirewall(config.Firewall{}), &Readiness{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(clientAuthPush)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	for body, want := range map[string]int{
		`{"cas": {"c1": {"cert": "", "managed": true}}, "certs": {}, "revoked": ["fp"]}`: http.StatusOK,
		`null`:    http.StatusOK,
		`{"cas":`: http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/client-certs", strings.NewReader(body)))
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d", body, rec.Code, want)
		}
		conf, err := p.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "x.com"})
		if err != nil || conf == nil || conf.ClientAuth != tls.RequireAndVerifyClientCert {
			t.Errorf("%s: a certificate change dropped the host's policy: conf=%v err=%v", body, conf, err)
		}
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/client-certs", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}
}
