package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"odac/internal/proxy/config"
	"odac/internal/proxysig"
)

func TestClientCannotStripForwardingHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	})

	r := altSvcRequest("/dynamic", false)
	r.RemoteAddr = "203.0.113.9:4321"
	r.Header.Set("Connection", "X-Real-IP, X-Odac-Connection-RemoteAddress, X-Forwarded-Proto, X-Forwarded-For, X-Forwarded-Host")
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	serveAltSvc(p, r)

	h := <-seen
	for name, want := range map[string]string{
		"X-Real-IP":                       "203.0.113.9",
		"X-Odac-Connection-RemoteAddress": "203.0.113.9",
		"X-Forwarded-Proto":               "http",
		"X-Forwarded-Host":                altSvcTestHost,
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := h.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want exactly the client address once", got)
	}
}

func newSignedProxy(t *testing.T, key string, upstream http.HandlerFunc) *Proxy {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	p := NewProxy()
	t.Cleanup(p.Cache().Stop)
	p.UpdateConfig(map[string]config.Website{altSvcTestHost: {
		Domain: altSvcTestHost,
		Port:   backend.Listener.Addr().(*net.TCPAddr).Port,
		Key:    key,
	}}, nil, nil, nil)
	return p
}

// The app rebuilds the signed string from what it receives, so the proxy must
// sign the Host and URI exactly as they leave it, not as the client sent them.
func TestConnectionHeadersAreSignedAsTheAppReceivesThem(t *testing.T) {
	type seen struct {
		header      http.Header
		host, uri   string
		method      string
		receivedSec int64
	}
	got := make(chan seen, 1)
	p := newSignedProxy(t, "app-key", func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Clone(), r.Host, r.RequestURI, r.Method, time.Now().Unix()}
	})

	r := altSvcRequest("/a%2Fb/c?x=1;y=2&z", true)
	r.Method = http.MethodPost
	r.RemoteAddr = "203.0.113.9:4321"
	r.Header.Set(proxysig.HeaderSignature, "t=1,v1=forged")
	serveAltSvc(p, r)

	s := <-got
	sig := s.header.Get(proxysig.HeaderSignature)
	ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || s.receivedSec-unix > 1 || unix > s.receivedSec {
		t.Fatalf("signature %q has no fresh timestamp", sig)
	}
	want := proxysig.Connection{
		Host: s.host, Method: s.method, RemoteAddress: s.header.Get(proxysig.HeaderRemoteAddress),
		SSL: s.header.Get(proxysig.HeaderSSL) == "true", Timestamp: unix, URI: s.uri,
	}.Sign("app-key")
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
	if s.header.Get(proxysig.HeaderRemoteAddress) != "203.0.113.9" || s.header.Get(proxysig.HeaderSSL) != "true" {
		t.Fatalf("connection headers = %v", s.header)
	}
	if n := len(s.header.Values(proxysig.HeaderSignature)); n != 1 {
		t.Fatalf("%d signatures reached the app", n)
	}
}

func TestConnectionHeadersGoUnsignedWithoutAKey(t *testing.T) {
	got := make(chan http.Header, 1)
	p := newSignedProxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	})
	r := altSvcRequest("/", false)
	r.Header.Set(proxysig.HeaderSignature, "t=1,v1=forged")
	r.Header.Set(proxysig.HeaderSSL, "true")
	serveAltSvc(p, r)

	h := <-got
	if v, ok := h[proxysig.HeaderSignature]; ok {
		t.Fatalf("client signature reached the app: %q", v)
	}
	if v, ok := h[proxysig.HeaderSSL]; ok {
		t.Fatalf("client ssl header reached the app: %q", v)
	}
}
