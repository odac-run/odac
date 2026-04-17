package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestPageCache() (*PageCache, *CacheManager) {
	cm := NewCacheManager()
	cm.maxSize.Store(64 * 1024 * 1024)
	pc := NewPageCache(cm)
	return pc, cm
}

func TestParseTTL(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"3600", 3600 * time.Second},
		{"60", 60 * time.Second},
		{"0", 0},
		{"-1", 0},
		{"", 0},
		{"abc", 0},
		{" 120 ", 120 * time.Second},
	}

	for _, tt := range tests {
		resp := &http.Response{Header: http.Header{}}
		if tt.header != "" {
			resp.Header.Set("X-Odac-Cache", tt.header)
		}
		got := ParseTTL(resp)
		if got != tt.want {
			t.Errorf("ParseTTL(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestIsPageCacheAllowed(t *testing.T) {
	tests := []struct {
		name   string
		resp   *http.Response
		want   bool
	}{
		{"200 OK", &http.Response{StatusCode: 200, Header: http.Header{}}, true},
		{"404", &http.Response{StatusCode: 404, Header: http.Header{}}, false},
		{"302", &http.Response{StatusCode: 302, Header: http.Header{}}, false},
		{"Set-Cookie", &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"a=b"}}}, true},
		{"no-store", &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": []string{"no-store"}}}, false},
		{"private", &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": []string{"private"}}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPageCacheAllowed(tt.resp); got != tt.want {
				t.Errorf("IsPageCacheAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildVariant(t *testing.T) {
	tests := []struct {
		name       string
		vary       string
		reqHeaders map[string]string
		wantOK     bool
		wantEmpty  bool
	}{
		{"empty vary", "", nil, true, true},
		{"accept-encoding only", "Accept-Encoding", nil, true, true},
		{"x-odac", "X-Odac", map[string]string{"X-Odac": "ajax"}, true, false},
		{"x-requested-with", "X-Requested-With", map[string]string{"X-Requested-With": "XMLHttpRequest"}, true, false},
		{"accept", "Accept", map[string]string{"Accept": "application/json"}, true, false},
		{"cookie unsafe", "Cookie", nil, false, false},
		{"authorization unsafe", "Authorization", nil, false, false},
		{"star unsafe", "*", nil, false, false},
		{"mixed safe+unsafe", "X-Odac, Cookie", nil, false, false},
		{"mixed safe+encoding", "Accept-Encoding, X-Odac", map[string]string{"X-Odac": "1"}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://example.com/", nil)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			variant, ok := buildVariant(req, tt.vary)
			if ok != tt.wantOK {
				t.Errorf("buildVariant() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && tt.wantEmpty && variant != "" {
				t.Errorf("buildVariant() variant = %q, want empty", variant)
			}
			if tt.wantOK && !tt.wantEmpty && variant == "" {
				t.Errorf("buildVariant() variant is empty, want non-empty")
			}
		})
	}
}

func TestPageCache_PutAndGet(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/landing", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"X-Odac-Cache":  []string{"3600"},
		},
	}
	body := []byte("<html><body>Landing</body></html>")

	// First Put — pending (not stable yet)
	pc.Put("example.com", "/landing", req, resp, body, 3600*time.Second)
	if pc.Get("example.com", "/landing", req) != nil {
		t.Error("Should not serve after first Put (not stable)")
	}

	// Second Put with same body — confirmed stable
	pc.Put("example.com", "/landing", req, resp, body, 3600*time.Second)

	entry := pc.Get("example.com", "/landing", req)
	if entry == nil {
		t.Fatal("Expected page cache hit after stability confirmed")
	}
	if string(entry.body) != string(body) {
		t.Errorf("Body mismatch: %q", entry.body)
	}
	if entry.ttl != 3600*time.Second {
		t.Errorf("TTL mismatch: %v", entry.ttl)
	}
}

func TestPageCache_DynamicContentNotCached(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/csrf-page", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"X-Odac-Cache": []string{"3600"},
		},
	}

	// Two different bodies (e.g. CSRF token changes) — should NOT become stable
	pc.Put("example.com", "/csrf-page", req, resp, []byte("<html>token=abc123</html>"), 3600*time.Second)
	pc.Put("example.com", "/csrf-page", req, resp, []byte("<html>token=def456</html>"), 3600*time.Second)

	if pc.Get("example.com", "/csrf-page", req) != nil {
		t.Error("Dynamic content (different bodies) should not be cached")
	}
}

func TestPageCache_VaryVariants(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	htmlBody := []byte("<html>Full Page</html>")
	jsonBody := []byte(`{"page":"data"}`)

	htmlResp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"X-Odac-Cache": []string{"60"},
			"Vary":         []string{"X-Odac"},
		},
	}
	jsonResp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Odac-Cache": []string{"60"},
			"Vary":         []string{"X-Odac"},
		},
	}

	// Store HTML variant (no X-Odac header) — two puts for stability
	htmlReq := httptest.NewRequest("GET", "http://example.com/about", nil)
	pc.Put("example.com", "/about", htmlReq, htmlResp, htmlBody, 60*time.Second)
	pc.Put("example.com", "/about", htmlReq, htmlResp, htmlBody, 60*time.Second)

	// Store JSON variant (X-Odac: ajax) — two puts for stability
	ajaxReq := httptest.NewRequest("GET", "http://example.com/about", nil)
	ajaxReq.Header.Set("X-Odac", "ajax")
	pc.Put("example.com", "/about", ajaxReq, jsonResp, jsonBody, 60*time.Second)
	pc.Put("example.com", "/about", ajaxReq, jsonResp, jsonBody, 60*time.Second)

	// Get HTML variant
	entry := pc.Get("example.com", "/about", htmlReq)
	if entry == nil {
		t.Fatal("Expected HTML variant hit")
	}
	if string(entry.body) != string(htmlBody) {
		t.Errorf("HTML body mismatch: %q", entry.body)
	}

	// Get JSON variant
	entry = pc.Get("example.com", "/about", ajaxReq)
	if entry == nil {
		t.Fatal("Expected JSON variant hit")
	}
	if string(entry.body) != string(jsonBody) {
		t.Errorf("JSON body mismatch: %q", entry.body)
	}
}

func TestPageCache_RejectsQueryString(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/page?token=secret", nil)
	entry := pc.Get("example.com", "/page", req)
	if entry != nil {
		t.Error("Should not return cache for query string requests")
	}
}

func TestPageCache_RejectsAuthorization(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/page", nil)
	req.Header.Set("Authorization", "Bearer token")
	entry := pc.Get("example.com", "/page", req)
	if entry != nil {
		t.Error("Should not return cache for authorized requests")
	}
}

func TestPageCache_TTLExpiration(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/page", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"X-Odac-Cache": []string{"1"},
		},
	}

	body := []byte("<html>test</html>")

	// Put twice for stability, with a real TTL
	pc.Put("example.com", "/page", req, resp, body, 1*time.Second)
	pc.Put("example.com", "/page", req, resp, body, 1*time.Second)

	// Should be stable and serveable now
	entry := pc.Get("example.com", "/page", req)
	if entry == nil {
		t.Fatal("Expected cache hit before expiration")
	}

	// Wait for TTL to expire
	time.Sleep(1100 * time.Millisecond)

	entry = pc.Get("example.com", "/page", req)
	if entry != nil {
		t.Error("Expired entry should not be returned")
	}
}

func TestPageCache_Purge(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/page", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"X-Odac-Cache": []string{"3600"},
		},
	}

	pc.Put("example.com", "/page", req, resp, []byte("<html>test</html>"), 3600*time.Second)
	pc.Put("example.com", "/page", req, resp, []byte("<html>test</html>"), 3600*time.Second)
	pc.Put("other.com", "/page", req, resp, []byte("<html>other</html>"), 3600*time.Second)
	pc.Put("other.com", "/page", req, resp, []byte("<html>other</html>"), 3600*time.Second)

	count := pc.Purge("example.com")
	if count != 1 {
		t.Errorf("Expected 1 purged, got %d", count)
	}

	if pc.Get("example.com", "/page", req) != nil {
		t.Error("example.com should be purged")
	}
	if pc.Get("other.com", "/page", req) == nil {
		t.Error("other.com should still exist")
	}
}

func TestPageCache_UnsafeVaryRejectsCache(t *testing.T) {
	pc, cm := newTestPageCache()
	defer cm.Stop()

	req := httptest.NewRequest("GET", "http://example.com/page", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"X-Odac-Cache": []string{"3600"},
			"Vary":         []string{"Cookie"},
		},
	}

	pc.Put("example.com", "/page", req, resp, []byte("<html>test</html>"), 3600*time.Second)

	if pc.Get("example.com", "/page", req) != nil {
		t.Error("Should not cache with Vary: Cookie")
	}
}

func TestServePageFromCache(t *testing.T) {
	entry := &pageEntry{
		body:       []byte("<html>cached</html>"),
		createdAt:  time.Now().UnixNano(),
		headers:    http.Header{"Content-Type": []string{"text/html"}},
		size:       19,
		statusCode: 200,
	}

	req := httptest.NewRequest("GET", "http://example.com/page", nil)
	w := httptest.NewRecorder()

	ServePageFromCache(w, req, entry)

	if w.Code != 200 {
		t.Errorf("Expected 200, got %d", w.Code)
	}
	if w.Header().Get("X-Odac-Cache") != "HIT" {
		t.Error("Expected X-Odac-Cache: HIT")
	}
	if w.Body.String() != "<html>cached</html>" {
		t.Errorf("Body mismatch: %q", w.Body.String())
	}
}
