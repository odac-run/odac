package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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
	hint := &http.Response{StatusCode: http.StatusOK, Request: anonymousPageRequest(), Header: http.Header{
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
				front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// heldBackend answers OLD before a deploy and NEW after it, and can keep OLD responses
// waiting so a purge lands while they are on their way back.
type heldBackend struct {
	p        *Proxy
	deployed atomic.Bool
	holding  atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newHeldBackend(t *testing.T, contentType, ttl string) *heldBackend {
	t.Helper()
	b := &heldBackend{entered: make(chan struct{}, 8), release: make(chan struct{})}
	b.p, _ = newAltSvcProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if ttl != "" {
			w.Header().Set(pageCacheHeader, ttl)
		}
		if b.deployed.Load() {
			w.Write([]byte("NEW"))
			return
		}
		if b.holding.Load() {
			b.entered <- struct{}{}
			<-b.release
		}
		w.Write([]byte("OLD"))
	})
	t.Cleanup(b.let)
	return b
}

func (b *heldBackend) let() { b.once.Do(func() { close(b.release) }) }

func (b *heldBackend) get(path string, client int) *httptest.ResponseRecorder {
	return serveAltSvc(b.p, integrityRequest(path, "", client))
}

func (b *heldBackend) awaitHeld(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-b.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the backend never received the held request")
		}
	}
}

func (b *heldBackend) deploy() {
	b.deployed.Store(true)
	b.p.Cache().Purge(altSvcTestHost)
	b.p.Pages().Purge(altSvcTestHost)
	b.p.Hints().Purge(altSvcTestHost)
}

func awaitFalse(t *testing.T, flag *atomic.Bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for flag.Load() {
		if time.Now().After(deadline) {
			t.Fatal("background revalidation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPurgeDiscardsAssetFetchedBeforeIt(t *testing.T) {
	b := newHeldBackend(t, "application/javascript", "")
	b.holding.Store(true)

	inFlight := make(chan string, 1)
	go func() { inFlight <- b.get("/app.js", 0).Body.String() }()
	b.awaitHeld(t, 1)
	b.deploy()

	// A request after the purge restores the frequency record the old fetch would pass
	// admission with.
	if body := b.get("/app.js", 1).Body.String(); body != "NEW" {
		t.Fatalf("request after the deploy got %q", body)
	}
	b.let()
	if body := <-inFlight; body != "OLD" {
		t.Fatalf("the held request got %q, so it did not start before the deploy", body)
	}
	if body := b.get("/app.js", 2).Body.String(); body != "NEW" {
		t.Fatalf("an old fetch overwrote the new asset: got %q", body)
	}
}

func TestPurgeDiscardsAssetRevalidatedBeforeIt(t *testing.T) {
	b := newHeldBackend(t, "application/javascript", "")
	b.get("/app.js", 0)
	v, ok := b.p.Cache().entries.Load(cacheKey(altSvcTestHost, "/app.js"))
	if !ok {
		t.Fatal("the asset never reached the cache, so this test proved nothing")
	}

	b.holding.Store(true)
	if rec := b.get("/app.js", 1); !isCacheHit(rec) {
		t.Fatal("second request did not hit the cache")
	}
	b.awaitHeld(t, 1)
	b.deploy()
	if body := b.get("/app.js", 2).Body.String(); body != "NEW" {
		t.Fatalf("request after the deploy got %q", body)
	}
	b.let()
	awaitFalse(t, &v.(*cacheEntry).revalidating)

	if body := b.get("/app.js", 3).Body.String(); body != "NEW" {
		t.Fatalf("an old revalidation overwrote the new asset: got %q", body)
	}
}

func TestPurgeDiscardsPagesFetchedBeforeIt(t *testing.T) {
	b := newHeldBackend(t, "text/html", "3600")
	b.holding.Store(true)

	var wg sync.WaitGroup
	for client := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.get("/page", client)
		}()
	}
	b.awaitHeld(t, 2)
	b.deploy()
	b.let()
	wg.Wait()

	for client := 2; client < 6; client++ {
		if body := b.get("/page", client).Body.String(); body != "NEW" {
			t.Fatalf("client %d got %q: two old fetches stabilized a removed page", client, body)
		}
	}
}

func TestPurgeDiscardsPageRevalidatedBeforeIt(t *testing.T) {
	b := newHeldBackend(t, "text/html", "3600")
	b.get("/page", 0)
	b.get("/page", 1)
	key := pageKey(altSvcTestHost, "/page", "")
	v, ok := b.p.Pages().entries.Load(key)
	if !ok || !v.(*pageEntry).stable {
		t.Fatal("the page never stabilized, so this test proved nothing")
	}

	b.holding.Store(true)
	if rec := b.get("/page", 2); !isCacheHit(rec) {
		t.Fatal("third request did not hit the cache")
	}
	b.awaitHeld(t, 1)
	b.deploy()
	b.let()
	awaitFalse(t, &v.(*pageEntry).revalidating)

	if _, stored := b.p.Pages().entries.Load(key); stored {
		t.Fatal("an old revalidation recorded the removed page again")
	}
}

func TestEveryPurgeInvalidatesFetches(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	for name, purge := range map[string]func(){
		"assets":     func() { p.Cache().Purge(altSvcTestHost) },
		"all assets": func() { p.Cache().PurgeAll() },
		"pages":      func() { p.Pages().Purge(altSvcTestHost) },
		"all pages":  func() { p.Pages().PurgeAll() },
	} {
		gen := p.Cache().generation()
		purge()
		stored := false
		p.Cache().storeUnlessPurged(gen, func() { stored = true })
		if stored {
			t.Errorf("%s: a fetch from before the purge could still store", name)
		}
	}
}

// A store that passed its check before the purge has to be visible to that purge, or it
// survives with nothing left to remove it.
func TestPurgeCannotMissAStoreThatPassedItsCheck(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	cm := p.Cache()
	key := cacheKey(altSvcTestHost, "/a.css")
	asset := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/css"}}}

	var version atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				gen := cm.generation()
				body := []byte(strconv.FormatInt(version.Load(), 10))
				cm.recordFrequency(key)
				cm.storeUnlessPurged(gen, func() {
					cm.Put(altSvcTestHost, "/a.css", asset, body)
				})
			}
		}()
	}

	for range 3000 {
		deployed := version.Add(1)
		cm.Purge(altSvcTestHost)
		if v, ok := cm.entries.Load(key); ok {
			if stored, _ := strconv.ParseInt(string(v.(*cacheEntry).body), 10, 64); stored < deployed {
				t.Errorf("version %d survived the purge of version %d", stored, deployed)
				break
			}
		}
	}
	close(stop)
	wg.Wait()
}
