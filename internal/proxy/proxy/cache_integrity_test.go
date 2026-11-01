package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const integrityMaxTries = 30

func integrityRequest(path, acceptEncoding string, client int) *http.Request {
	r := altSvcRequest(path, true)
	r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", client+1)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	return r
}

func isCacheHit(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get("X-Odac-Cache") == "HIT"
}

func serveUntilHit(t *testing.T, p *Proxy, path, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	for i := range integrityMaxTries {
		if rec := serveAltSvc(p, integrityRequest(path, acceptEncoding, i)); isCacheHit(rec) {
			return rec
		}
	}
	t.Fatalf("%s never became a cache hit", path)
	return nil
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodedBody(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	if rec.Header().Get("Content-Encoding") != "gzip" {
		return rec.Body.Bytes()
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeInChunks(w http.ResponseWriter, body []byte) {
	const chunk = 64 * 1024
	for len(body) > 0 {
		n := min(chunk, len(body))
		w.Write(body[:n])
		w.(http.Flusher).Flush()
		body = body[n:]
	}
}

func TestAssetCacheRefusesBodyCompressedByBackend(t *testing.T) {
	js := []byte(strings.Repeat("console.log('hello');\n", 500))
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(gzipBytes(t, js))
			return
		}
		w.Write(js)
	})

	for i := range integrityMaxTries {
		rec := serveAltSvc(p, integrityRequest("/app.js", "gzip", i))
		if isCacheHit(rec) {
			t.Fatalf("request %d: a compressed body was served from the cache", i)
		}
		if !bytes.Equal(decodedBody(t, rec), js) {
			t.Fatalf("request %d: client could not recover the original body", i)
		}
	}

	hit := serveUntilHit(t, p, "/app.js", "")
	if !bytes.Equal(hit.Body.Bytes(), js) {
		t.Fatal("raw body from a client without Accept-Encoding did not round-trip")
	}
	if hit = serveAltSvc(p, integrityRequest("/app.js", "gzip", 0)); !isCacheHit(hit) {
		t.Fatal("raw entry was not served to a client accepting gzip")
	}
	if !bytes.Equal(decodedBody(t, hit), js) {
		t.Fatal("raw entry did not survive compression on serve")
	}
}

// Revalidation reaches Put without going through cacheRecordWriter, and the transport
// only decodes gzip, so a backend sending another encoding arrives here still encoded.
func TestAssetCachePutRefusesCompressedBody(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	cm := p.Cache()
	put := func(path string, headers http.Header) *cacheEntry {
		cm.Get(altSvcTestHost, path)
		cm.Put(altSvcTestHost, path, &http.Response{StatusCode: http.StatusOK, Header: headers}, []byte("body"))
		return cm.Get(altSvcTestHost, path)
	}
	if put("/plain.js", http.Header{"Content-Type": {"application/javascript"}}) == nil {
		t.Fatal("control: an uncompressed body should be stored")
	}
	if put("/encoded.js", http.Header{
		"Content-Encoding": {"br"},
		"Content-Type":     {"application/javascript"},
	}) != nil {
		t.Fatal("a compressed body entered the asset cache")
	}
}

func TestPageCacheRefusesBodyCompressedByBackend(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(http.ResponseWriter, *http.Request) {})
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Encoding": {"gzip"},
		"Content-Type":     {"text/html"},
	}}
	for client := range 2 {
		r := integrityRequest("/page", "gzip", client)
		p.Pages().Put(altSvcTestHost, "/page", r, resp, []byte("compressed"), time.Hour)
	}
	if entry := p.Pages().Get(altSvcTestHost, "/page", integrityRequest("/page", "", 3)); entry != nil {
		t.Fatal("a compressed body entered the page cache")
	}
	if _, stored := p.Pages().entries.Load(pageKey(altSvcTestHost, "/page", "")); stored {
		t.Fatal("a compressed body was kept as a stability candidate")
	}
}

func TestAssetCacheStoresWholeBodyOrNothing(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantHit bool
	}{
		{"above the page limit", 3 * 1024 * 1024, true},
		{"above the asset limit", cacheMaxFileSize + 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.Repeat([]byte{0xAB}, tt.size)
			p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				writeInChunks(w, body)
			})
			hits := 0
			for i := range integrityMaxTries {
				rec := serveAltSvc(p, integrityRequest("/big.png", "", i))
				if !bytes.Equal(rec.Body.Bytes(), body) {
					t.Fatalf("request %d (hit=%v): got %d of %d bytes", i, isCacheHit(rec), rec.Body.Len(), len(body))
				}
				if isCacheHit(rec) {
					hits++
				}
			}
			if (hits > 0) != tt.wantHit {
				t.Fatalf("%d cache hits, want any = %v", hits, tt.wantHit)
			}
		})
	}
}

func TestCacheRecordWriterDropsBufferPastLimit(t *testing.T) {
	crw := newCacheRecordWriter(httptest.NewRecorder(), "", 100)
	crw.Header().Set("Content-Type", "image/png")
	crw.WriteHeader(http.StatusOK)

	crw.Write(make([]byte, 60))
	crw.Write(make([]byte, 60))
	// Fits on its own, so checking only the remaining room would leave a hole in the body.
	crw.Write(make([]byte, 10))

	if crw.body.Len() != 0 {
		t.Fatalf("kept %d bytes of a body past its limit", crw.body.Len())
	}
}

func TestCacheRecordWriterSkipsBufferingWhatCannotBeStored(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"unknown length", nil, true},
		{"at the limit", map[string]string{"Content-Length": "100"}, true},
		{"past the limit", map[string]string{"Content-Length": "101"}, false},
		{"compressed", map[string]string{"Content-Encoding": "br"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crw := newCacheRecordWriter(httptest.NewRecorder(), "", 100)
			crw.Header().Set("Content-Type", "image/png")
			for k, v := range tt.headers {
				crw.Header().Set(k, v)
			}
			crw.WriteHeader(http.StatusOK)
			if crw.shouldBuffer != tt.want {
				t.Fatalf("shouldBuffer = %v, want %v", crw.shouldBuffer, tt.want)
			}
		})
	}
}

func TestCacheHitsCarryTheSameHeadersAsTheMiss(t *testing.T) {
	cspLines := []string{"default-src 'self'", "frame-ancestors 'none'"}
	appHeaders := func(h http.Header) {
		h["Content-Security-Policy"] = cspLines
		h.Set("X-Frame-Options", "DENY")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("ETag", `"v1"`)
	}
	assertPolicies := func(t *testing.T, got http.Header) {
		t.Helper()
		if !slices.Equal(got.Values("Content-Security-Policy"), cspLines) {
			t.Errorf("Content-Security-Policy = %q, want %q", got.Values("Content-Security-Policy"), cspLines)
		}
		want := map[string]string{
			"X-Frame-Options":              "DENY",
			"Access-Control-Allow-Origin":  "*",
			"Cross-Origin-Resource-Policy": "cross-origin",
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "strict-origin-when-cross-origin",
			"X-Xss-Protection":             "1; mode=block",
		}
		for name, value := range want {
			if got.Get(name) != value {
				t.Errorf("%s = %q, want %q", name, got.Get(name), value)
			}
		}
	}

	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		appHeaders(w.Header())
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Content-Length", strconv.Itoa(len("body{}")))
		w.Write([]byte("body{}"))
	})

	t.Run("asset miss", func(t *testing.T) {
		rec := serveAltSvc(p, integrityRequest("/miss.css", "", 0))
		if isCacheHit(rec) {
			t.Fatal("expected a miss")
		}
		assertPolicies(t, rec.Result().Header)
	})
	t.Run("asset hit", func(t *testing.T) {
		assertPolicies(t, serveUntilHit(t, p, "/hit.css", "").Result().Header)
	})
	t.Run("asset 304", func(t *testing.T) {
		serveUntilHit(t, p, "/not-modified.css", "")
		r := integrityRequest("/not-modified.css", "", 0)
		r.Header.Set("If-None-Match", `"v1"`)
		rec := serveAltSvc(p, r)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		assertPolicies(t, rec.Result().Header)
	})
	t.Run("page hit", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}}
		appHeaders(resp.Header)
		for client := range 2 {
			r := integrityRequest("/page", "", client)
			p.Pages().Put(altSvcTestHost, "/page", r, resp, []byte("<p>page</p>"), time.Hour)
		}
		rec := serveAltSvc(p, integrityRequest("/page", "", 3))
		if !isCacheHit(rec) {
			t.Fatal("expected a page cache hit")
		}
		assertPolicies(t, rec.Result().Header)
	})
}

func TestRestoredHeadersDoNotAliasTheEntry(t *testing.T) {
	stored := http.Header{"Content-Security-Policy": {"default-src 'self'"}}
	dst := make(http.Header)
	restoreHeaders(dst, stored)
	dst["Content-Security-Policy"][0] = "changed"
	if stored.Get("Content-Security-Policy") != "default-src 'self'" {
		t.Fatal("writing a response changed the cached entry")
	}
}

func TestCachedHitNeverReplaysSetCookie(t *testing.T) {
	stored := storedHeaders(http.Header{"Set-Cookie": {"session=secret"}}, replayedHeaders)
	if stored.Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie must stay out of the replayed headers")
	}
}

func TestAssetCacheRefusesVaryOrigin(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Write([]byte("font"))
	})
	for i := range integrityMaxTries {
		r := integrityRequest("/font.woff2", "", i)
		origin := fmt.Sprintf("https://site%d.test", i)
		r.Header.Set("Origin", origin)
		rec := serveAltSvc(p, r)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Fatalf("request %d: Access-Control-Allow-Origin = %q, want %q", i, got, origin)
		}
	}
}
