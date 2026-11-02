package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"odac/internal/proxy/config"
)

const altSvcTestHost = "altsvc.test"

func newAltSvcProxy(t *testing.T, upstream http.HandlerFunc) (*Proxy, *httptest.Server) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	p := NewProxy()
	t.Cleanup(p.Cache().Stop)
	p.UpdateConfig(map[string]config.Website{altSvcTestHost: {
		Domain: altSvcTestHost,
		Port:   backend.Listener.Addr().(*net.TCPAddr).Port,
	}}, nil, nil, nil)
	return p, backend
}

func altSvcRequest(path string, secure bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+altSvcTestHost+path, nil)
	if secure {
		r.TLS = &tls.ConnectionState{HandshakeComplete: true}
	}
	return r
}

func serveAltSvc(p *Proxy, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

func assertAltSvc(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	got := rec.Result().Header.Values("Alt-Svc")
	if len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) {
		t.Fatalf("status %d: Alt-Svc = %q, want %q", rec.Code, got, want)
	}
}

func TestAltSvcCoversEveryResponsePath(t *testing.T) {
	p, backend := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h2="upstream.test:8443"`)
		switch r.URL.Path {
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("ETag", `"v1"`)
		}
		w.Write([]byte("body"))
	})
	p.SetHTTP3Available(true)

	t.Run("proxied response replaces the upstream value", func(t *testing.T) {
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/dynamic", true)), altSvcHTTP3)
	})

	t.Run("asset cache hit", func(t *testing.T) {
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/app.js", true)), altSvcHTTP3)
		rec := serveAltSvc(p, altSvcRequest("/app.js", true))
		if rec.Header().Get("X-Odac-Cache") != "HIT" {
			t.Fatal("second request did not hit the asset cache")
		}
		assertAltSvc(t, rec, altSvcHTTP3)
	})

	t.Run("asset cache revalidation", func(t *testing.T) {
		r := altSvcRequest("/app.js", true)
		r.Header.Set("If-None-Match", `"v1"`)
		rec := serveAltSvc(p, r)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status %d, want 304", rec.Code)
		}
		assertAltSvc(t, rec, altSvcHTTP3)
	})

	t.Run("page cache hit", func(t *testing.T) {
		page := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}}
		for _, r := range []*http.Request{altSvcRequest("/page", true), withIP(altSvcRequest("/page", true), secondClient)} {
			p.Pages().Put(altSvcTestHost, "/page", r, page, []byte("page"), time.Hour)
		}
		rec := serveAltSvc(p, altSvcRequest("/page", true))
		if rec.Header().Get("X-Odac-Cache") != "HIT" {
			t.Fatal("request did not hit the page cache")
		}
		assertAltSvc(t, rec, altSvcHTTP3)
	})

	t.Run("upstream failure", func(t *testing.T) {
		backend.Close()
		rec := serveAltSvc(p, altSvcRequest("/down", true))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", rec.Code)
		}
		assertAltSvc(t, rec, altSvcHTTP3)
	})
}

func TestAltSvcNotSentWhenHTTP3CannotBeUsed(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Alt-Svc", `h2="upstream.test:8443"`)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("body"))
	})

	t.Run("listener not bound", func(t *testing.T) {
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/app.js", true)))
		rec := serveAltSvc(p, altSvcRequest("/app.js", true))
		if rec.Header().Get("X-Odac-Cache") != "HIT" {
			t.Fatal("second request did not hit the asset cache")
		}
		assertAltSvc(t, rec)
	})

	t.Run("cleartext request", func(t *testing.T) {
		p.SetHTTP3Available(true)
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/plain", false)))
	})

	t.Run("listener gone again", func(t *testing.T) {
		p.SetHTTP3Available(true)
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/app.js", true)), altSvcHTTP3)
		p.SetHTTP3Available(false)
		assertAltSvc(t, serveAltSvc(p, altSvcRequest("/app.js", true)))
	})
}

func TestHTTP3ServiceReportsAvailability(t *testing.T) {
	holder := holdUDPPort(t)
	srv := h3TestServer(t, http.NewServeMux())
	srv.Addr = holder.LocalAddr().String()

	var available atomic.Bool
	svc := NewHTTP3Service(srv, nil, available.Store)
	svc.Start()
	time.Sleep(3 * udpBindRetryInterval)
	if available.Load() {
		t.Fatal("reported available while another socket held the port")
	}

	holder.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !available.Load() {
		if time.Now().After(deadline) {
			t.Fatal("not reported available after taking the port")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopH3Service(t, svc)
	if available.Load() {
		t.Fatal("still reported available after Stop")
	}
}

func TestHTTP3ServiceStaysUnavailableOnPermanentBindError(t *testing.T) {
	srv := h3TestServer(t, http.NewServeMux())
	srv.Addr = "127.0.0.1:99999"
	var available atomic.Bool
	svc := NewHTTP3Service(srv, nil, available.Store)
	svc.Start()
	defer stopH3Service(t, svc)

	<-svc.done
	if available.Load() {
		t.Fatal("reported available without a socket")
	}
}
