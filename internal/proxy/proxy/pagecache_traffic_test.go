package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func optInPage(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set(pageCacheHeader, "60")
		w.Write([]byte(body))
	}
}

func seedStablePage(p *Proxy, path string, headers http.Header, body string) *pageEntry {
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Content-Type", "text/html")
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers}
	for client := range 2 {
		p.Pages().Put(altSvcTestHost, path, integrityRequest(path, "", client), resp, []byte(body), time.Hour)
	}
	return pageLookup(p, path)
}

func pageLookup(p *Proxy, path string) *pageEntry {
	return p.Pages().Get(altSvcTestHost, path, integrityRequest(path, "", 9))
}

func pageBudgetUsed(p *Proxy) (pages, shared int64) {
	return p.Pages().totalSize.Load(), p.Cache().totalSize.Load()
}

func TestPageCacheServesOptedInPageAfterTwoClients(t *testing.T) {
	var renders atomic.Int64
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "" {
			renders.Add(1)
		}
		optInPage("<p>page</p>")(w, r)
	})

	for client := range 2 {
		rec := serveAltSvc(p, integrityRequest("/page", "", client))
		if got := rec.Header().Get(pageCacheHeader); got != "" {
			t.Fatalf("client %d: %s = %q on a miss", client, pageCacheHeader, got)
		}
		if rec.Body.String() != "<p>page</p>" {
			t.Fatalf("client %d: body = %q", client, rec.Body.String())
		}
	}
	if renders.Load() != 2 {
		t.Fatalf("renders = %d, want 2", renders.Load())
	}

	rec := serveAltSvc(p, integrityRequest("/page", "", 3))
	if !isCacheHit(rec) || rec.Body.String() != "<p>page</p>" {
		t.Fatalf("hit = %v, body = %q", isCacheHit(rec), rec.Body.String())
	}
}

func TestPageCacheSameClientNeverConfirmsItsOwnPage(t *testing.T) {
	p, _ := newAltSvcProxy(t, optInPage("<p>mine</p>"))
	for i := range 5 {
		if rec := serveAltSvc(p, integrityRequest("/page", "", 0)); isCacheHit(rec) {
			t.Fatalf("request %d: one client confirmed its own page", i)
		}
	}
}

func TestPageCacheHeaderNeverReachesClients(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set(pageCacheHeader, "60")
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
		}
		if r.URL.Path == "/hinted" {
			w.Header().Set("Link", "</a.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set(pageCacheHeader, "60")
		}
		w.Write([]byte(strings.Repeat("<p>page</p>", 200)))
	})
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	tests := []struct {
		name, method, path, encoding, auth string
	}{
		{name: "page", method: http.MethodGet, path: "/page"},
		{name: "page compressed", method: http.MethodGet, path: "/page", encoding: "gzip"},
		{name: "query", method: http.MethodGet, path: "/page?a=1"},
		{name: "query compressed", method: http.MethodGet, path: "/page?a=1", encoding: "gzip"},
		{name: "post", method: http.MethodPost, path: "/page"},
		{name: "authorization", method: http.MethodGet, path: "/page", auth: "Bearer x"},
		{name: "not found", method: http.MethodGet, path: "/missing"},
		{name: "early hints", method: http.MethodGet, path: "/hinted"},
		{name: "asset", method: http.MethodGet, path: "/page.js"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, front.URL+tt.path, nil)
			req.Host = altSvcTestHost
			if tt.encoding != "" {
				req.Header.Set("Accept-Encoding", tt.encoding)
			}
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if got := resp.Header.Get(pageCacheHeader); got != "" {
				t.Fatalf("%s = %q reached the client", pageCacheHeader, got)
			}
		})
	}
}

// Node's res.writeEarlyHints produces this sequence.
func TestEarlyHintsFromBackendKeepTheFinalStatus(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</a.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(strings.Repeat("gone", 500)))
	})
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	for _, path := range []string{"/page", "/app.js", "/page?a=1"} {
		for _, encoding := range []string{"", "gzip"} {
			req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
			req.Host = altSvcTestHost
			if encoding != "" {
				req.Header.Set("Accept-Encoding", encoding)
			}
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s encoding=%q: status = %d, want 404", path, encoding, resp.StatusCode)
			}
		}
	}
}

func TestPageRecorderBuffersOnlyOptedInResponses(t *testing.T) {
	tests := []struct {
		name, contentType, ttl string
		want                   bool
	}{
		{"opted in", "text/html", "60", true},
		{"api response", "application/json", "", false},
		{"zero ttl", "text/html", "0", false},
		{"invalid ttl", "text/html", "soon", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crw := newCacheRecordWriter(httptest.NewRecorder(), "", pageMaxBodySize)
			crw.pageOnly = true
			crw.Header().Set("Content-Type", tt.contentType)
			if tt.ttl != "" {
				crw.Header().Set(pageCacheHeader, tt.ttl)
			}
			crw.WriteHeader(http.StatusOK)
			if crw.shouldBuffer != tt.want {
				t.Fatalf("shouldBuffer = %v, want %v", crw.shouldBuffer, tt.want)
			}
		})
	}
}

func TestBareNotModifiedKeepsThePage(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	entry := seedStablePage(p, "/one", http.Header{"Etag": {`"v1"`}}, "<p>one</p>")
	p.revalidatePageCache(altSvcTestHost, integrityRequest("/one", "", 0), entry)
	if pageLookup(p, "/one") == nil {
		t.Fatal("a 304 without the opt-in header removed the page")
	}
}

func TestWithdrawalRemovesOnlyThatPage(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
	}{
		{"header gone", http.Header{}},
		{"no-store", http.Header{pageCacheHeader: {"60"}, "Cache-Control": {"no-store"}}},
		{"mixed case private", http.Header{pageCacheHeader: {"60"}, "Cache-Control": {"Private"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header()[k] = v
				}
				w.Write([]byte("<p>one</p>"))
			})
			entry := seedStablePage(p, "/one", nil, "<p>one</p>")
			seedStablePage(p, "/two", nil, "<p>two</p>")
			p.revalidatePageCache(altSvcTestHost, integrityRequest("/one", "", 0), entry)
			if pageLookup(p, "/one") != nil {
				t.Fatal("withdrawn page is still served")
			}
			if pageLookup(p, "/two") == nil {
				t.Fatal("withdrawing one page removed another")
			}
		})
	}
}

func TestRevalidationIsForwardedLikeAClientRequest(t *testing.T) {
	var got atomic.Pointer[http.Request]
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		got.Store(r)
		w.WriteHeader(http.StatusNotModified)
	})
	original := integrityRequest("/one", "", 0)
	original.Host = "www." + altSvcTestHost

	assertForwarded := func(t *testing.T) {
		t.Helper()
		r := got.Load()
		if r == nil {
			t.Fatal("backend was not asked")
		}
		if r.Host != original.Host {
			t.Errorf("Host = %q, want %q", r.Host, original.Host)
		}
		if r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("X-Forwarded-Proto = %q", r.Header.Get("X-Forwarded-Proto"))
		}
		if r.Header.Get("X-Forwarded-Host") != original.Host {
			t.Errorf("X-Forwarded-Host = %q", r.Header.Get("X-Forwarded-Host"))
		}
		if r.Header.Get("X-Odac-Connection-Ssl") != "true" {
			t.Errorf("X-Odac-Connection-Ssl = %q", r.Header.Get("X-Odac-Connection-Ssl"))
		}
	}

	t.Run("page", func(t *testing.T) {
		entry := seedStablePage(p, "/one", nil, "<p>one</p>")
		p.revalidatePageCache(altSvcTestHost, original, entry)
		assertForwarded(t)
	})
	t.Run("asset", func(t *testing.T) {
		got.Store(nil)
		p.revalidateCache(altSvcTestHost, original, &cacheEntry{etag: `"v1"`})
		assertForwarded(t)
	})
}

func TestRevalidationDoesNotStoreARedirectTarget(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/one" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		optInPage("<p>login</p>")(w, r)
	})
	entry := seedStablePage(p, "/one", nil, "<p>one</p>")
	p.revalidatePageCache(altSvcTestHost, integrityRequest("/one", "", 0), entry)
	if kept := pageLookup(p, "/one"); kept == nil || string(kept.body) != "<p>one</p>" {
		t.Fatalf("entry after redirect = %+v", kept)
	}
}

func TestRevalidationStoresRawBodyWhenVaryingOnEncoding(t *testing.T) {
	changed := []byte("<p>changed</p>")
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set(pageCacheHeader, "60")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gzipBytes(t, changed))
	})
	entry := seedStablePage(p, "/one", http.Header{"Vary": {"Accept-Encoding"}}, "<p>one</p>")
	original := integrityRequest("/one", "gzip", 0)
	p.revalidatePageCache(altSvcTestHost, original, entry)

	stored, ok := p.Pages().entries.Load(pageKey(altSvcTestHost, "/one", ""))
	if !ok {
		t.Fatal("no entry after revalidation")
	}
	if stored.(*pageEntry).bodyHash != hashBody(changed) {
		t.Fatal("the changed page was not picked up")
	}
}

func TestPageCandidateHoldsNoBody(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}}
	body := make([]byte, 1024*1024)

	const scannedURLs = 2000
	for i := range scannedURLs {
		path := fmt.Sprintf("/scan/%d", i)
		p.Pages().Put(altSvcTestHost, path, integrityRequest(path, "", 0), resp, body, time.Hour)
	}

	pages, shared := pageBudgetUsed(p)
	if pages > scannedURLs*1024 || pages != shared {
		t.Fatalf("%d one-off URLs charged pages=%d shared=%d", scannedURLs, pages, shared)
	}
	entry := seedStablePage(p, "/real", nil, "<p>real</p>")
	if entry == nil {
		t.Fatal("a real page could not be cached after the scan")
	}
}

func TestPageCandidateExpiresWithinItsWindow(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}}
	p.Pages().Put(altSvcTestHost, "/one", integrityRequest("/one", "", 0), resp, []byte("x"), 24*time.Hour)
	stored, _ := p.Pages().entries.Load(pageKey(altSvcTestHost, "/one", ""))
	if ttl := stored.(*pageEntry).ttl; ttl != pageCandidateWindow {
		t.Fatalf("candidate ttl = %v, want %v", ttl, pageCandidateWindow)
	}
	if entry := seedStablePage(p, "/one", nil, "x"); entry == nil || entry.ttl != time.Hour {
		t.Fatalf("confirmed entry = %+v, want the app's ttl", entry)
	}
}

func TestPagesCannotTakeTheAssetCacheBudget(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	p.Cache().maxSize.Store(4 * pageMaxBodySize)
	body := strings.Repeat("x", pageMaxBodySize/2)
	for i := range 8 {
		seedStablePage(p, fmt.Sprintf("/page/%d", i), nil, body+fmt.Sprint(i))
	}
	pages, _ := pageBudgetUsed(p)
	if limit := p.Cache().maxSize.Load() / 2; pages > limit {
		t.Fatalf("pages use %d bytes, cap is %d", pages, limit)
	}
	if pageLookup(p, "/page/0") == nil {
		t.Fatal("pages under the cap should be admitted")
	}
}

func TestExpiredPagesAreSweptWithoutARequest(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	headers := http.Header{"Content-Type": {"text/html"}, "Vary": {"X-Requested-With"}}
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers}
	for client := range 2 {
		r := integrityRequest("/gone", "", client)
		p.Pages().Put(altSvcTestHost, "/gone", r, resp, []byte("gone"), time.Nanosecond)
	}
	time.Sleep(time.Millisecond)
	seedStablePage(p, "/kept", http.Header{"Vary": {"X-Requested-With"}}, "kept")

	p.Cache().runMaintenance()

	if _, ok := p.Pages().varyMap.Load(altSvcTestHost + "/gone"); ok {
		t.Error("vary mapping of an expired page was kept")
	}
	if _, ok := p.Pages().varyMap.Load(altSvcTestHost + "/kept"); !ok {
		t.Error("vary mapping of a live page was dropped")
	}
	entries := 0
	p.Pages().entries.Range(func(_, _ any) bool { entries++; return true })
	if entries != 1 {
		t.Errorf("%d entries left, want only the live page", entries)
	}
}

func TestCriticalMemoryPurgesPages(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	seedStablePage(p, "/one", nil, "<p>one</p>")
	p.Cache().UpdateMemory(100, 90)
	if pages, shared := pageBudgetUsed(p); pages != 0 || shared != 0 {
		t.Fatalf("budget after purge: pages=%d shared=%d", pages, shared)
	}
	p.Cache().UpdateMemory(100, 10)
	if pageLookup(p, "/one") != nil {
		t.Fatal("page survived the critical memory purge")
	}
}

func TestPagesAreNotAdmittedUnderMemoryPressure(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	p.Cache().underPressure.Store(true)
	if seedStablePage(p, "/one", nil, "<p>one</p>") != nil {
		t.Fatal("page was admitted under memory pressure")
	}
}

func TestPurgeStopsAtTheHostBoundary(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	seedStablePage(p, "/one", http.Header{"Vary": {"X-Requested-With"}}, "<p>one</p>")
	shorter := altSvcTestHost[:len(altSvcTestHost)-1]
	if n := p.Pages().Purge(shorter); n != 0 {
		t.Fatalf("purging %s removed %d entries of %s", shorter, n, altSvcTestHost)
	}
	if _, ok := p.Pages().varyMap.Load(altSvcTestHost + "/one"); !ok {
		t.Fatal("purging a shorter host dropped a vary mapping")
	}
	if n := p.Pages().Purge(altSvcTestHost); n != 1 {
		t.Fatalf("purge removed %d entries, want 1", n)
	}
}

func TestSecondVaryLineStillPreventsCaching(t *testing.T) {
	headers := http.Header{"Content-Type": {"text/css"}, "Vary": {"Accept-Encoding", "Cookie"}}
	if !varyPreventsAssetCache(headers) {
		t.Error("asset cache ignored a second Vary line")
	}
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	if seedStablePage(p, "/one", http.Header{"Vary": {"Accept-Encoding", "Cookie"}}, "<p>one</p>") != nil {
		t.Error("page cache ignored a second Vary line")
	}
}

func TestCompressionKeepsTheAppsVary(t *testing.T) {
	body := strings.Repeat("<p>page</p>", 200)
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	seedStablePage(p, "/one", http.Header{"Vary": {"X-Requested-With"}}, body)
	r := integrityRequest("/one", "gzip", 3)
	cw := httptest.NewRecorder()
	p.ServeHTTP(cw, r)
	if !isCacheHit(cw) {
		t.Fatal("expected a hit")
	}
	vary := varyFields(cw.Result().Header)
	if !strings.Contains(vary, "X-Requested-With") || !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary = %q, a browser would mix up the variants", vary)
	}
	zr, err := gzip.NewReader(bytes.NewReader(cw.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != body {
		t.Fatal("compressed hit did not decode to the page")
	}

	headers := http.Header{"Vary": {"accept-encoding"}, "Content-Type": {"text/html"}}
	rec := httptest.NewRecorder()
	for k, v := range headers {
		rec.Header()[k] = v
	}
	comp := newCompressionResponseWriter(rec, "gzip")
	comp.WriteHeader(http.StatusOK)
	comp.Close()
	if got := rec.Result().Header.Values("Vary"); len(got) != 1 {
		t.Fatalf("Vary = %q, Accept-Encoding was added twice", got)
	}
}

func TestConditionalHitIsNotLabeledAsCompressed(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	seedStablePage(p, "/one", http.Header{"Etag": {`"v1"`}}, strings.Repeat("<p>page</p>", 200))
	r := integrityRequest("/one", "gzip", 3)
	r.Header.Set("If-None-Match", `"v1"`)
	rec := serveAltSvc(p, r)
	if rec.Code != http.StatusNotModified || !isCacheHit(rec) {
		t.Fatalf("status = %d, hit = %v", rec.Code, isCacheHit(rec))
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a 304", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried %d body bytes", rec.Body.Len())
	}
	if !strings.Contains(varyFields(rec.Header()), "Accept-Encoding") {
		t.Errorf("Vary = %q, the 304 must keep what the 200 sends", varyFields(rec.Header()))
	}
}

func TestPageCacheAccountingStaysBalancedUnderRaces(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}}
	body := []byte("<p>page</p>")

	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				path := fmt.Sprintf("/p/%d", i%5)
				r := integrityRequest(path, "", worker)
				ttl := time.Hour
				if i%3 == 0 {
					ttl = time.Nanosecond
				}
				p.Pages().Put(altSvcTestHost, path, r, resp, body, ttl)
				p.Pages().Get(altSvcTestHost, path, r)
				if i%50 == 0 {
					p.Pages().evictExpired()
				}
			}
		}()
	}
	wg.Wait()

	p.Pages().PurgeAll()
	if pages, shared := pageBudgetUsed(p); pages != 0 || shared != 0 {
		t.Fatalf("budget after purging everything: pages=%d shared=%d", pages, shared)
	}
}

func TestCookiedClientDoesNotPopulateButReceivesTheSharedPage(t *testing.T) {
	var renders atomic.Int64
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		renders.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set(pageCacheHeader, "60")
		if r.Header.Get("Cookie") != "" {
			w.Write([]byte("<p>private</p>"))
			return
		}
		w.Write([]byte("<p>public</p>"))
	})
	for client := range 3 {
		r := integrityRequest("/page", "", client)
		r.Header.Set("Cookie", "session=secret")
		if got := serveAltSvc(p, r).Body.String(); got != "<p>private</p>" {
			t.Fatalf("client %d: body = %q", client, got)
		}
	}
	if entry := pageLookup(p, "/page"); entry != nil {
		t.Fatal("a cookied response entered the cache")
	}
	for client := range 2 {
		serveAltSvc(p, integrityRequest("/page", "", client))
	}
	r := integrityRequest("/page", "", 5)
	r.Header.Set("Cookie", "session=secret")
	rec := serveAltSvc(p, r)
	if !isCacheHit(rec) || rec.Body.String() != "<p>public</p>" {
		t.Fatalf("hit = %v, body = %q", isCacheHit(rec), rec.Body.String())
	}
}
