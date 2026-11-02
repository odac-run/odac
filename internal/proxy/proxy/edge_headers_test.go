package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"odac/internal/proxy/config"
)

const hstsValue = "max-age=63072000; includeSubDomains; preload"

func assertEdgeHeaders(t *testing.T, label string, h http.Header) {
	t.Helper()
	if got := h.Values("Strict-Transport-Security"); len(got) != 1 || got[0] != hstsValue {
		t.Errorf("%s: HSTS = %q, want only %q", label, got, hstsValue)
	}
	if got := h.Values("Alt-Svc"); len(got) != 1 || got[0] != altSvcHTTP3 {
		t.Errorf("%s: Alt-Svc = %q, want only %q", label, got, altSvcHTTP3)
	}
}

func newEdgeFront(t *testing.T, upstream http.HandlerFunc) (*Proxy, *httptest.Server) {
	t.Helper()
	p, _ := newAltSvcProxy(t, upstream)
	p.SetHTTP3Available(true)
	front := httptest.NewTLSServer(p)
	t.Cleanup(front.Close)
	return p, front
}

func edgeDo(t *testing.T, front *httptest.Server, req *http.Request) *http.Response {
	t.Helper()
	req.Host = altSvcTestHost
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func sendEarlyHints(w http.ResponseWriter) {
	w.Header().Set("Link", "</a.css>; rel=preload")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Link")
}

func TestEdgeHeadersSurviveBackendInterimResponses(t *testing.T) {
	_, front := newEdgeFront(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Hints") != "" {
			sendEarlyHints(w)
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("hello"))
	})

	tests := []struct {
		name, method, path, encoding string
		hints, expect                bool
	}{
		{name: "103 on a page", method: http.MethodGet, path: "/page", hints: true},
		{name: "103 on a compressed page", method: http.MethodGet, path: "/page", encoding: "gzip", hints: true},
		{name: "103 on an asset", method: http.MethodGet, path: "/app.js", hints: true},
		{name: "103 on a query", method: http.MethodGet, path: "/page?a=1", hints: true},
		{name: "103 on a post", method: http.MethodPost, path: "/submit", hints: true},
		{name: "100 continue", method: http.MethodPost, path: "/submit", expect: true},
		{name: "100 continue compressed", method: http.MethodPost, path: "/submit", encoding: "gzip", expect: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader("x=1")
			}
			req, _ := http.NewRequest(tc.method, front.URL+tc.path, body)
			if tc.hints {
				req.Header.Set("X-Test-Hints", "1")
			}
			if tc.encoding != "" {
				req.Header.Set("Accept-Encoding", tc.encoding)
			}
			if tc.expect {
				req.Header.Set("Expect", "100-continue")
			}
			assertEdgeHeaders(t, tc.name, edgeDo(t, front, req).Header)
		})
	}
}

func TestAppCannotReplaceHSTS(t *testing.T) {
	_, front := newEdgeFront(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Hints") != "" {
			sendEarlyHints(w)
		}
		w.Header().Set("Strict-Transport-Security", "max-age=1")
		w.Header().Set("Alt-Svc", `h2="upstream.test:8443"`)
		w.Write([]byte("hello"))
	})

	for _, hints := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/page", nil)
		if hints {
			req.Header.Set("X-Test-Hints", "1")
		}
		assertEdgeHeaders(t, "app sends its own", edgeDo(t, front, req).Header)
	}
}

func TestBadGatewayAfterInterimKeepsEdgeHeaders(t *testing.T) {
	_, front := newEdgeFront(t, func(w http.ResponseWriter, _ *http.Request) {
		sendEarlyHints(w)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/page", nil)
	resp := edgeDo(t, front, req)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	assertEdgeHeaders(t, "502", resp.Header)
}

func TestLearnedHintsKeepEdgeHeadersOnTheFinalResponse(t *testing.T) {
	_, front := newEdgeFront(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</a.css>; rel=preload")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("hello"))
	})

	var interim atomic.Int64
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				interim.Add(1)
			}
			return nil
		},
	})
	for _, target := range []string{"/page", "/page", "/page", "/page?a=1"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+target, nil)
		assertEdgeHeaders(t, "learned hints", edgeDo(t, front, req).Header)
	}
	if interim.Load() == 0 {
		t.Fatal("no request received the learned 103, so this test proved nothing")
	}
}

func TestEdgeHeadersOnCacheHits(t *testing.T) {
	p, _ := newEdgeFront(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("body"))
	})
	seedStablePage(p, "/page", nil, "<p>page</p>")

	for _, path := range []string{"/app.js", "/page"} {
		serveAltSvc(p, integrityRequest(path, "", 3))
		rec := serveAltSvc(p, integrityRequest(path, "", 3))
		if !isCacheHit(rec) {
			t.Fatalf("%s: second request did not hit the cache", path)
		}
		assertEdgeHeaders(t, path, rec.Result().Header)
	}
}

func TestHSTSOnlyWhereABrowserCanKeepIt(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port

	p := NewProxy()
	t.Cleanup(p.Cache().Stop)
	sites := map[string]config.Website{}
	for _, host := range []string{"site.test", "localhost", "127.0.0.1"} {
		sites[host] = config.Website{Domain: host, Port: port}
	}
	p.UpdateConfig(sites, nil, nil, nil)

	tests := []struct {
		host   string
		secure bool
		want   bool
	}{
		{"site.test", true, true},
		{"site.test:8443", true, true},
		{"site.test", false, false},
		{"localhost", true, false},
		{"localhost:8443", true, false},
		{"127.0.0.1", true, false},
		{"127.0.0.1:8443", true, false},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/page", nil)
		if tc.secure {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
		}
		got := serveAltSvc(p, r).Result().Header.Get("Strict-Transport-Security") != ""
		if got != tc.want {
			t.Errorf("host=%s secure=%v: HSTS sent = %v, want %v", tc.host, tc.secure, got, tc.want)
		}
	}
}

func TestHTTP3InterimResponseKeepsEdgeHeaders(t *testing.T) {
	rig := newH3Rig(t, func(w http.ResponseWriter, _ *http.Request) {
		sendEarlyHints(w)
		w.Write([]byte("hello"))
	})
	rig.proxy.SetHTTP3Available(true)

	resp, err := rig.client.Get(rig.baseURL + "/page")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	assertEdgeHeaders(t, "http/3", resp.Header)
}
