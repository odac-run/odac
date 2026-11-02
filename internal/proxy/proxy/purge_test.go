package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"odac/internal/proxy/config"
)

func TestKeyBelongsToHost(t *testing.T) {
	tests := []struct {
		key, host string
		want      bool
	}{
		{"example.com/app.js", "example.com", true},
		{"example.com", "example.com", true},
		{"blog.example.com/app.js", "example.com", true},
		{"a.b.example.com/app.js", "example.com", true},
		{"notexample.com/app.js", "example.com", false},
		{"example.co/app.js", "example.com", false},
		{"example.com.evil.test/app.js", "example.com", false},
		{"other.test/blog.example.com/app.js", "example.com", false},
		{"example.com/app.js", "blog.example.com", false},
		{"shop.example.com/app.js", "blog.example.com", false},
		{"a.blog.example.com/app.js", "blog.example.com", true},
	}
	for _, tc := range tests {
		if got := keyBelongsToHost(tc.key, tc.host); got != tc.want {
			t.Errorf("keyBelongsToHost(%q, %q) = %v, want %v", tc.key, tc.host, got, tc.want)
		}
	}
}

func TestPurgeReachesSubdomainsInEveryStore(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	parent := altSvcTestHost
	other := "not" + altSvcTestHost
	hosts := []string{parent, "blog." + parent, "a.b." + parent, other}

	asset := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/css"}}}
	page := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type": {"text/html"},
		"Vary":         {"X-Requested-With"},
	}}
	hint := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type": {"text/html"},
		"Link":         {"</a.css>; rel=preload"},
	}}
	for _, host := range hosts {
		for range 10 {
			p.Cache().recordFrequency(cacheKey(host, "/a.css"))
		}
		p.Cache().Put(host, "/a.css", asset, []byte("data"))
		for client := range 2 {
			p.Pages().Put(host, "/page", integrityRequest("/page", "", client), page, []byte("page"), time.Hour)
			p.Hints().Learn(host, "/page", hint)
		}
	}

	for name, purge := range map[string]func(string) int{
		"assets": p.Cache().Purge,
		"pages":  p.Pages().Purge,
		"hints":  p.Hints().Purge,
	} {
		if n := purge(parent); n != 3 {
			t.Errorf("%s: purged %d entries, want 3", name, n)
		}
	}

	for _, host := range hosts {
		kept := host == other
		// Get records a hit, so the frequency table has to be read first.
		if _, got := p.Cache().frequency.Load(cacheKey(host, "/a.css")); got != kept {
			t.Errorf("%s: frequency kept = %v, want %v", host, got, kept)
		}
		if got := p.Cache().Get(host, "/a.css") != nil; got != kept {
			t.Errorf("%s: asset kept = %v, want %v", host, got, kept)
		}
		if got := p.Pages().Get(host, "/page", integrityRequest("/page", "", 9)) != nil; got != kept {
			t.Errorf("%s: page kept = %v, want %v", host, got, kept)
		}
		if _, got := p.Pages().varyMap.Load(host + "/page"); got != kept {
			t.Errorf("%s: vary mapping kept = %v, want %v", host, got, kept)
		}
		if _, got := p.Hints().entries.Load(host + "/page"); got != kept {
			t.Errorf("%s: hint kept = %v, want %v", host, got, kept)
		}
	}

	p.Cache().Purge(other)
	p.Pages().Purge(other)
	if pages, shared := pageBudgetUsed(p); pages != 0 || shared != 0 {
		t.Fatalf("budget after purging everything: pages=%d shared=%d", pages, shared)
	}
}

func revalidationInFlight(p *Proxy, host, path string) bool {
	if v, ok := p.Cache().entries.Load(cacheKey(host, path)); ok && v.(*cacheEntry).revalidating.Load() {
		return true
	}
	if v, ok := p.Pages().entries.Load(pageKey(host, path, "")); ok && v.(*pageEntry).revalidating.Load() {
		return true
	}
	return false
}

func TestDeployRemovesSubdomainContent(t *testing.T) {
	sub := "blog." + altSvcTestHost
	for _, path := range []string{"/page", "/app.js"} {
		for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s then %d", path, status), func(t *testing.T) {
				var deployed atomic.Bool
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if path == "/app.js" {
						w.Header().Set("Content-Type", "application/javascript")
					} else {
						w.Header().Set("Content-Type", "text/html")
						w.Header().Set(pageCacheHeader, "3600")
					}
					if !deployed.Load() {
						w.Write([]byte("OLD"))
						return
					}
					w.WriteHeader(status)
					w.Write([]byte("NEW"))
				}))
				t.Cleanup(backend.Close)

				p := NewProxy()
				t.Cleanup(p.Cache().Stop)
				p.UpdateConfig(map[string]config.Website{altSvcTestHost: {
					Domain: altSvcTestHost,
					Port:   backend.Listener.Addr().(*net.TCPAddr).Port,
				}}, nil, nil, nil)
				var handled atomic.Int64
				front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer handled.Add(1)
					r.RemoteAddr = r.Header.Get("X-Test-Addr")
					p.ServeHTTP(w, r)
				}))
				t.Cleanup(front.Close)

				do := func(client int) (string, bool) {
					req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
					req.Host = sub
					req.Header.Set("X-Test-Addr", fmt.Sprintf("192.0.2.%d:1234", client+1))
					resp, err := front.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					return string(body), resp.Header.Get("X-Odac-Cache") == "HIT"
				}

				hit := false
				for client := range 12 {
					_, cached := do(client)
					hit = hit || cached
				}
				if !hit {
					t.Fatal("subdomain content never reached the cache, so this test proved nothing")
				}

				// This test is about what the purge finds, so nothing may still be writing.
				deadline := time.Now().Add(5 * time.Second)
				for handled.Load() < 12 || revalidationInFlight(p, sub, path) {
					if time.Now().After(deadline) {
						t.Fatal("background revalidation did not finish")
					}
					time.Sleep(time.Millisecond)
				}

				deployed.Store(true)
				p.Cache().Purge(altSvcTestHost)
				p.Pages().Purge(altSvcTestHost)
				p.Hints().Purge(altSvcTestHost)

				for client := range 10 {
					if body, _ := do(client); body == "OLD" {
						t.Fatalf("request %d after the deploy got the old content", client)
					}
				}
			})
		}
	}
}
