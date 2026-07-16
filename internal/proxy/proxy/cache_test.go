package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsCacheable(t *testing.T) {
	tests := []struct {
		method string
		path   string
		auth   string
		want   bool
	}{
		{"GET", "/assets/style.css", "", true},
		{"GET", "/app.js", "", true},
		{"GET", "/logo.png", "", true},
		{"GET", "/font.woff2", "", true},
		{"GET", "/image.webp", "", true},
		{"GET", "/icon.svg", "", true},
		{"GET", "/photo.jpg", "", true},
		{"GET", "/photo.jpeg", "", true},
		{"GET", "/anim.gif", "", true},
		{"GET", "/favicon.ico", "", true},
		{"GET", "/bundle.mjs", "", true},
		{"GET", "/bundle.js.map", "", true},
		{"GET", "/font.woff", "", true},
		{"GET", "/font.ttf", "", true},
		{"GET", "/font.eot", "", true},
		{"GET", "/font.otf", "", true},
		{"GET", "/image.avif", "", true},
		{"GET", "/image.bmp", "", true},
		{"HEAD", "/style.css", "", true},
		{"POST", "/style.css", "", false},
		{"PUT", "/style.css", "", false},
		{"DELETE", "/style.css", "", false},
		{"GET", "/api/users", "", false},
		{"GET", "/page", "", false},
		{"GET", "/index.html", "", false},
		{"GET", "/data.xml", "", false},
		{"GET", "/style.css", "Bearer token", false},
		{"GET", "/style.css?v=123", "", false},
		{"GET", "/image.jpg?token=secret", "", false},
		{"GET", "/app.js?utm_source=google", "", false},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, "http://example.com"+tt.path, nil)
		if tt.auth != "" {
			req.Header.Set("Authorization", tt.auth)
		}
		got := IsCacheable(req)
		if got != tt.want {
			t.Errorf("IsCacheable(%s %s auth=%q) = %v, want %v", tt.method, tt.path, tt.auth, got, tt.want)
		}
	}
}

func TestCacheableContentType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"text/css", true},
		{"text/css; charset=utf-8", true},
		{"text/javascript", true},
		{"application/javascript", true},
		{"image/png", true},
		{"image/webp", true},
		{"image/svg+xml", true},
		{"font/woff2", true},
		{"application/font-woff", true},
		{"application/wasm", true},
		{"application/json", true},
		{"text/html", false},
		{"text/plain", false},
		{"application/pdf", false},
		{"multipart/form-data", false},
	}

	for _, tt := range tests {
		got := cacheableContentType(tt.ct)
		if got != tt.want {
			t.Errorf("cacheableContentType(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}

func TestCacheManager_PutAndGet(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/assets/style.css"
	body := []byte("body { color: red; }")

	// Simulate enough frequency for admission
	key := cacheKey(host, urlPath)
	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/css; charset=utf-8"},
			"Etag":         []string{`"abc123"`},
		},
	}

	cm.Put(host, urlPath, resp, body)

	entry := cm.Get(host, urlPath)
	if entry == nil {
		t.Fatal("Expected cache hit, got miss")
	}

	if string(entry.body) != string(body) {
		t.Errorf("Body mismatch: got %q, want %q", entry.body, body)
	}
	if entry.etag != `"abc123"` {
		t.Errorf("ETag mismatch: got %q, want %q", entry.etag, `"abc123"`)
	}
	if entry.contentType != "text/css; charset=utf-8" {
		t.Errorf("Content-Type mismatch: got %q", entry.contentType)
	}
}

func TestCacheManager_Miss(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	entry := cm.Get("example.com", "/nonexistent.css")
	if entry != nil {
		t.Error("Expected cache miss, got hit")
	}
}

func TestCacheManager_RejectsNonCacheableStatus(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/error.css"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 404,
		Header:     http.Header{"Content-Type": []string{"text/css"}},
	}

	cm.Put(host, urlPath, resp, []byte("not found"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache non-200 responses")
	}
}

func TestCacheManager_RejectsNoStore(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/private.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":  []string{"application/javascript"},
			"Cache-Control": []string{"no-store"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("secret()"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache no-store responses")
	}
}

func TestCacheManager_RejectsPrivate(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/user.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":  []string{"application/javascript"},
			"Cache-Control": []string{"private, max-age=3600"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("private()"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache private responses")
	}
}

func TestCacheManager_RejectsSetCookie(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/tracked.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/javascript"},
			"Set-Cookie":   []string{"session=abc123; Path=/"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("tracked()"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache responses with Set-Cookie")
	}
}

func TestCacheManager_RejectsVaryCookie(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/personalized.css"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/css"},
			"Vary":         []string{"Accept-Encoding, Cookie"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("body{}"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache responses with Vary: Cookie")
	}
}

func TestCacheManager_RejectsVaryStar(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/dynamic.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/javascript"},
			"Vary":         []string{"*"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("dynamic()"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache responses with Vary: *")
	}
}

func TestCacheManager_RejectsVaryAuthorization(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/auth.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/javascript"},
			"Vary":         []string{"Authorization"},
		},
	}

	cm.Put(host, urlPath, resp, []byte("auth()"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache responses with Vary: Authorization")
	}
}

func TestCacheManager_RejectsOversizedBody(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/huge.js"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	bigBody := make([]byte, cacheMaxFileSize+1)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/javascript"}},
	}

	cm.Put(host, urlPath, resp, bigBody)

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache oversized responses")
	}
}

func TestCacheManager_RejectsEmptyBody(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/empty.css"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/css"}},
	}

	cm.Put(host, urlPath, resp, []byte{})

	if cm.Get(host, urlPath) != nil {
		t.Error("Should not cache empty responses")
	}
}

func TestCacheManager_Purge(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	// Add entries for two domains
	for _, host := range []string{"a.com", "b.com"} {
		for _, p := range []string{"/style.css", "/app.js"} {
			key := cacheKey(host, p)
			for i := 0; i < 10; i++ {
				cm.recordFrequency(key)
			}
			resp := &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/css"}},
			}
			cm.Put(host, p, resp, []byte("data"))
		}
	}

	// Purge only a.com
	count := cm.Purge("a.com")
	if count != 2 {
		t.Errorf("Expected 2 purged entries, got %d", count)
	}

	// a.com should be gone
	if cm.Get("a.com", "/style.css") != nil {
		t.Error("a.com entries should be purged")
	}

	// b.com should remain
	if cm.Get("b.com", "/style.css") == nil {
		t.Error("b.com entries should still exist")
	}
}

func TestCacheManager_PurgeAll(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	for _, host := range []string{"a.com", "b.com"} {
		key := cacheKey(host, "/style.css")
		for i := 0; i < 10; i++ {
			cm.recordFrequency(key)
		}
		resp := &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/css"}},
		}
		cm.Put(host, "/style.css", resp, []byte("data"))
	}

	count := cm.PurgeAll()
	if count != 2 {
		t.Errorf("Expected 2 purged entries, got %d", count)
	}

	if cm.Get("a.com", "/style.css") != nil || cm.Get("b.com", "/style.css") != nil {
		t.Error("All entries should be purged")
	}
}

func TestCacheManager_Stats(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	stats := cm.Stats()
	if !stats["enabled"].(bool) {
		t.Error("Cache should be enabled by default")
	}
	if stats["entries"].(int) != 0 {
		t.Error("Should start with 0 entries")
	}
}

func TestCacheManager_FrequencyAdmission(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/cold.css"
	key := cacheKey(host, urlPath)

	// Simulate a single request that happened 30 seconds ago — frequency = 1/30 ≈ 0.03 req/s
	// This is below cacheMinFrequency (0.2), so it should NOT be admitted
	cm.frequency.Store(key, &frequencyRecord{
		firstSeen: time.Now().Add(-30 * time.Second).UnixNano(),
	})
	val, _ := cm.frequency.Load(key)
	val.(*frequencyRecord).count.Store(1)

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/css"}},
	}

	cm.Put(host, urlPath, resp, []byte("cold asset"))

	if cm.Get(host, urlPath) != nil {
		t.Error("Cold asset should not be admitted to cache")
	}
}

func TestCacheManager_UpdatesExistingEntry(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	host := "example.com"
	urlPath := "/style.css"
	key := cacheKey(host, urlPath)

	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp1 := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/css"},
			"Etag":         []string{`"v1"`},
		},
	}
	cm.Put(host, urlPath, resp1, []byte("v1"))

	resp2 := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/css"},
			"Etag":         []string{`"v2"`},
		},
	}
	cm.Put(host, urlPath, resp2, []byte("v2"))

	entry := cm.Get(host, urlPath)
	if entry == nil {
		t.Fatal("Expected cache hit")
	}
	if string(entry.body) != "v2" {
		t.Errorf("Expected updated body 'v2', got %q", entry.body)
	}
	if entry.etag != `"v2"` {
		t.Errorf("Expected updated etag, got %q", entry.etag)
	}
}

func TestServeFromCache_Hit(t *testing.T) {
	entry := &cacheEntry{
		body:        []byte("body { color: red; }"),
		contentType: "text/css",
		etag:        `"abc"`,
		headers: http.Header{
			"Content-Type": []string{"text/css"},
			"ETag":         []string{`"abc"`},
		},
		size:       20,
		statusCode: 200,
	}

	req := httptest.NewRequest("GET", "http://example.com/style.css", nil)
	w := httptest.NewRecorder()

	ServeFromCache(w, req, entry)

	if w.Code != 200 {
		t.Errorf("Expected 200, got %d", w.Code)
	}
	if w.Header().Get("X-Odac-Cache") != "HIT" {
		t.Error("Expected X-Odac-Cache: HIT header")
	}
	if w.Header().Get("Server") != "ODAC" {
		t.Error("Expected Server: ODAC header")
	}
	if w.Body.String() != "body { color: red; }" {
		t.Errorf("Body mismatch: %q", w.Body.String())
	}
}

func TestServeFromCache_ConditionalETag(t *testing.T) {
	entry := &cacheEntry{
		body:        []byte("body { color: red; }"),
		contentType: "text/css",
		etag:        `"abc"`,
		headers: http.Header{
			"Content-Type": []string{"text/css"},
			"ETag":         []string{`"abc"`},
		},
		size:       20,
		statusCode: 200,
	}

	req := httptest.NewRequest("GET", "http://example.com/style.css", nil)
	req.Header.Set("If-None-Match", `"abc"`)
	w := httptest.NewRecorder()

	ServeFromCache(w, req, entry)

	if w.Code != 304 {
		t.Errorf("Expected 304, got %d", w.Code)
	}
}

func TestServeFromCache_HeadRequest(t *testing.T) {
	entry := &cacheEntry{
		body:        []byte("body { color: red; }"),
		contentType: "text/css",
		headers: http.Header{
			"Content-Type": []string{"text/css"},
		},
		size:       20,
		statusCode: 200,
	}

	req := httptest.NewRequest("HEAD", "http://example.com/style.css", nil)
	w := httptest.NewRecorder()

	ServeFromCache(w, req, entry)

	if w.Code != 200 {
		t.Errorf("Expected 200, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Error("HEAD response should have empty body")
	}
}

func TestSortCandidates(t *testing.T) {
	candidates := []evictCandidate{
		{key: "high-score", score: 10.0, lastAccess: 100},
		{key: "low-score-old", score: 0.1, lastAccess: 50},
		{key: "low-score-new", score: 0.1, lastAccess: 200},
		{key: "mid-score", score: 5.0, lastAccess: 150},
	}

	sortCandidates(candidates)

	// Should be sorted: lowest score first, then oldest access
	expected := []string{"low-score-old", "low-score-new", "mid-score", "high-score"}
	for i, e := range expected {
		if candidates[i].key != e {
			t.Errorf("Position %d: expected %s, got %s", i, e, candidates[i].key)
		}
	}
}

func TestCacheManager_DisabledReturnsNil(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	cm.enabled.Store(false)

	// Get should return nil when disabled
	entry := cm.Get("example.com", "/style.css")
	if entry != nil {
		t.Error("Disabled cache should return nil")
	}

	// Put should be a no-op when disabled
	key := cacheKey("example.com", "/style.css")
	for i := 0; i < 10; i++ {
		cm.recordFrequency(key)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/css"}},
	}
	cm.Put("example.com", "/style.css", resp, []byte("data"))

	cm.enabled.Store(true)
	if cm.Get("example.com", "/style.css") != nil {
		t.Error("Entry should not have been stored while disabled")
	}
}

func TestCacheEntry_Frequency(t *testing.T) {
	entry := &cacheEntry{
		createdAt: time.Now().Add(-10 * time.Second).UnixNano(),
	}
	entry.hitCount.Store(20)

	freq := entry.frequency()
	// ~2 req/s (20 hits / 10 seconds)
	if freq < 1.5 || freq > 2.5 {
		t.Errorf("Expected frequency ~2.0, got %.2f", freq)
	}
}

func TestCacheEntry_RevalidationThrottle(t *testing.T) {
	entry := &cacheEntry{}

	// First call should succeed
	if !entry.ShouldRevalidate() {
		t.Error("First ShouldRevalidate should return true")
	}

	// Second call while first is in-flight should be rejected
	if entry.ShouldRevalidate() {
		t.Error("Concurrent ShouldRevalidate should return false")
	}

	// Finish revalidation
	entry.FinishRevalidation()

	// Immediately after finish — should be rejected (interval not elapsed)
	if entry.ShouldRevalidate() {
		t.Error("ShouldRevalidate should return false within interval")
	}
}

func TestCacheableExtension(t *testing.T) {
	positives := []string{".css", ".js", ".mjs", ".png", ".jpg", ".jpeg", ".gif",
		".webp", ".avif", ".ico", ".svg", ".woff", ".woff2", ".ttf", ".eot", ".otf", ".map", ".bmp"}
	for _, ext := range positives {
		if !cacheableExtension(ext) {
			t.Errorf("Expected %s to be cacheable", ext)
		}
	}

	negatives := []string{".html", ".php", ".json", ".xml", ".pdf", ".zip", ".tar", ""}
	for _, ext := range negatives {
		if cacheableExtension(ext) {
			t.Errorf("Expected %s to NOT be cacheable", ext)
		}
	}
}

func TestAssetPriority(t *testing.T) {
	// Render-blocking assets should have highest priority
	cssPriority := assetPriority("/assets/style.css")
	jsPriority := assetPriority("/assets/app.js")
	fontPriority := assetPriority("/fonts/inter.woff2")
	imgPriority := assetPriority("/images/hero.webp")
	mapPriority := assetPriority("/assets/app.js.map")

	if cssPriority <= imgPriority {
		t.Errorf("CSS (%.1f) should have higher priority than image (%.1f)", cssPriority, imgPriority)
	}
	if jsPriority <= imgPriority {
		t.Errorf("JS (%.1f) should have higher priority than image (%.1f)", jsPriority, imgPriority)
	}
	if fontPriority <= imgPriority {
		t.Errorf("Font (%.1f) should have higher priority than image (%.1f)", fontPriority, imgPriority)
	}
	if mapPriority >= imgPriority {
		t.Errorf("Source map (%.1f) should have lower priority than image (%.1f)", mapPriority, imgPriority)
	}
}
