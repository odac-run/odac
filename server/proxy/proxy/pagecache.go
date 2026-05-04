package proxy

import (
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// ODAC Page Cache — App-Controlled HTML Response Cache
//
// Apps opt-in by sending "X-Odac-Cache: <seconds>" in their response.
// Without this header, pages are NEVER cached. This gives developers
// explicit control while keeping the zero-config philosophy.
//
// Supports Vary-based variants (whitelisted headers only) so the same
// URL can serve different cached responses for HTML vs AJAX requests.
//
// Uses stale-while-revalidate: cached response is served instantly,
// backend is checked async (throttled to 1 req/s per entry).
// ============================================================================

const (
	// pageMaxBodySize is the maximum HTML response size that can be cached (2MB).
	pageMaxBodySize = 2 * 1024 * 1024

	// pageCacheHeader is the response header apps use to opt-in to page caching.
	pageCacheHeader = "X-Odac-Cache"
)

// varyWhitelist defines which Vary header fields are safe to use as cache
// key variants. Fields not in this list cause the response to be uncacheable.
// "Accept-Encoding" is always ignored (we cache raw body, compress on serve).
var varyWhitelist = map[string]bool{
	"accept":             true,
	"x-odac":             true,
	"x-requested-with":   true,
}

// pageEntry represents a cached HTML page response.
type pageEntry struct {
	body       []byte
	bodyHash   uint64 // FNV-1a hash of body for stability detection
	headers    http.Header
	etag       string
	lastMod    string
	size       int
	statusCode int
	stable     bool          // True after two consecutive identical responses
	ttl        time.Duration // App-specified TTL from X-Odac-Cache
	createdAt  int64         // Unix nano

	lastAccess      atomic.Int64
	hitCount        atomic.Int64
	revalidating    atomic.Bool
	lastRevalidated atomic.Int64
}

// expired returns true if the entry has exceeded its app-specified TTL.
func (pe *pageEntry) expired() bool {
	return time.Since(time.Unix(0, pe.createdAt)) > pe.ttl
}

// ShouldRevalidate checks if a background revalidation should be triggered.
func (pe *pageEntry) ShouldRevalidate() bool {
	now := time.Now().UnixNano()
	if now-pe.lastRevalidated.Load() < cacheRevalidateInterval.Nanoseconds() {
		return false
	}
	return pe.revalidating.CompareAndSwap(false, true)
}

// FinishRevalidation releases the revalidation lock.
func (pe *pageEntry) FinishRevalidation() {
	pe.lastRevalidated.Store(time.Now().UnixNano())
	pe.revalidating.Store(false)
}

// PageCache manages app-controlled HTML page caching.
type PageCache struct {
	entries   sync.Map     // key -> *pageEntry
	varyMap   sync.Map     // "host+path" -> vary header string (learned from backend)
	totalSize atomic.Int64 // Shared memory accounting with CacheManager
	cache     *CacheManager // Reference for memory limit checks
}

// NewPageCache creates a page cache that shares memory limits with the asset cache.
func NewPageCache(cm *CacheManager) *PageCache {
	return &PageCache{cache: cm}
}

// pageKey builds a cache key from host + path + vary variant.
func pageKey(host, urlPath, variant string) string {
	if variant == "" {
		return "page:" + host + urlPath
	}
	return "page:" + host + urlPath + "|" + variant
}

// buildVariant extracts the vary-based cache key suffix from the request.
// Returns ("", true) if no variant needed, (variant, true) if safe,
// or ("", false) if the Vary header contains unsafe fields.
func buildVariant(r *http.Request, varyHeader string) (string, bool) {
	if varyHeader == "" {
		return "", true
	}

	var parts []string
	for _, field := range strings.Split(varyHeader, ",") {
		field = strings.TrimSpace(strings.ToLower(field))
		if field == "" || field == "accept-encoding" {
			continue // Ignored — we cache raw body
		}
		if !varyWhitelist[field] {
			return "", false // Unsafe field — don't cache
		}
		// Use the request's value for this header as part of the key
		val := r.Header.Get(field)
		parts = append(parts, field+"="+val)
	}

	if len(parts) == 0 {
		return "", true
	}

	// Sort for deterministic key
	for i := 1; i < len(parts); i++ {
		j := i
		for j > 0 && parts[j] < parts[j-1] {
			parts[j], parts[j-1] = parts[j-1], parts[j]
			j--
		}
	}
	return strings.Join(parts, "&"), true
}

// ParseTTL extracts the cache TTL from the X-Odac-Cache header.
// Returns 0 if the header is missing, invalid, or explicitly 0.
func ParseTTL(resp *http.Response) time.Duration {
	val := resp.Header.Get(pageCacheHeader)
	if val == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// hashBody returns a fast FNV-1a hash of the response body.
// Used for stability detection — if two consecutive responses produce
// the same hash, the page content is static and safe to cache.
func hashBody(body []byte) uint64 {
	h := fnv.New64a()
	h.Write(body)
	return h.Sum64()
}

// IsPageCacheAllowed checks if a response is safe to cache as a page.
// Note: Set-Cookie is intentionally NOT checked here. When an app sends
// X-Odac-Cache, it explicitly opts in to caching. Session cookies are
// common on landing pages but don't affect page content. The Set-Cookie
// header is stripped from the cached entry (never served from cache).
func IsPageCacheAllowed(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") {
		return false
	}
	return true
}

// Get retrieves a cached page. Returns nil on miss or if expired.
func (pc *PageCache) Get(host, urlPath string, r *http.Request) *pageEntry {
	if !pc.cache.enabled.Load() {
		return nil
	}

	// Only GET requests
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil
	}

	// No query strings
	if r.URL.RawQuery != "" {
		return nil
	}

	// No Authorization header
	if r.Header.Get("Authorization") != "" {
		return nil
	}

	// Look up the learned Vary header for this URL
	varyHeader := ""
	if v, ok := pc.varyMap.Load(host + urlPath); ok {
		varyHeader = v.(string)
	}

	variant, safe := buildVariant(r, varyHeader)
	if !safe {
		return nil
	}

	key := pageKey(host, urlPath, variant)
	val, ok := pc.entries.Load(key)
	if !ok {
		return nil
	}

	entry := val.(*pageEntry)

	// Hard TTL expiration — entry is stale, remove it
	if entry.expired() {
		pc.entries.Delete(key)
		pc.totalSize.Add(-int64(entry.size))
		pc.cache.totalSize.Add(-int64(entry.size))
		return nil
	}

	// Only serve confirmed-stable entries (two consecutive identical responses seen)
	if !entry.stable {
		return nil
	}

	entry.lastAccess.Store(time.Now().UnixNano())
	entry.hitCount.Add(1)
	return entry
}

// Put stores a page response in cache.
func (pc *PageCache) Put(host, urlPath string, r *http.Request, resp *http.Response, body []byte, ttl time.Duration) {
	if !pc.cache.enabled.Load() || ttl <= 0 {
		return
	}

	if len(body) > pageMaxBodySize || len(body) == 0 {
		return
	}

	// Check shared memory budget
	totalUsed := pc.cache.totalSize.Load() + pc.totalSize.Load()
	if totalUsed+int64(len(body)) > pc.cache.maxSize.Load() {
		return // No room — don't evict asset cache for page cache
	}

	varyHeader := resp.Header.Get("Vary")
	variant, safe := buildVariant(r, varyHeader)
	if !safe {
		return
	}

	// Store the Vary header for this URL so future Get() calls can build the variant key
	urlKey := host + urlPath
	if varyHeader != "" {
		pc.varyMap.Store(urlKey, varyHeader)
	} else {
		pc.varyMap.Delete(urlKey)
	}

	key := pageKey(host, urlPath, variant)

	newHash := hashBody(body)

	// Stability check: if an entry already exists, compare body hashes.
	// Two consecutive identical responses = stable (safe to serve from cache).
	// Different body = dynamic content (e.g. CSRF tokens), don't promote to stable.
	if existing, loaded := pc.entries.Load(key); loaded {
		entry := existing.(*pageEntry)
		if entry.bodyHash == newHash {
			// Same content confirmed — promote to stable
			if !entry.stable {
				entry.stable = true
				entry.lastAccess.Store(time.Now().UnixNano())
				debugLog("[PageCache] Stability confirmed: %s", key)
			}
			return
		}
		// Content changed — replace entry, reset stability
		pc.entries.Delete(key)
		pc.totalSize.Add(-int64(entry.size))
		pc.cache.totalSize.Add(-int64(entry.size))
		debugLog("[PageCache] Content changed (dynamic?): %s", key)
	}

	// Preserve safe headers
	headers := make(http.Header)
	for _, h := range []string{
		"Cache-Control", "Content-Language", "Content-Type",
		"ETag", "Last-Modified", "Link", "Vary", "X-Robots-Tag",
	} {
		if v := resp.Header.Get(h); v != "" {
			headers.Set(h, v)
		}
	}

	now := time.Now()
	entry := &pageEntry{
		body:       body,
		bodyHash:   newHash,
		createdAt:  now.UnixNano(),
		etag:       resp.Header.Get("ETag"),
		headers:    headers,
		lastMod:    resp.Header.Get("Last-Modified"),
		size:       len(body),
		stable:     false, // Needs second identical response to confirm
		statusCode: resp.StatusCode,
		ttl:        ttl,
	}
	entry.lastAccess.Store(now.UnixNano())
	entry.hitCount.Store(1)

	pc.entries.Store(key, entry)
	pc.totalSize.Add(int64(entry.size))
	pc.cache.totalSize.Add(int64(entry.size))
}

// ServePageFromCache writes a cached page response to the client.
func ServePageFromCache(w http.ResponseWriter, r *http.Request, entry *pageEntry) {
	for key, vals := range entry.headers {
		for _, v := range vals {
			w.Header().Set(key, v)
		}
	}

	age := int(time.Since(time.Unix(0, entry.createdAt)).Seconds())
	w.Header().Set("Age", strconv.Itoa(age))
	w.Header().Set("X-Odac-Cache", "HIT")
	w.Header().Set("Server", "ODAC")

	// Conditional request support
	if entry.etag != "" && r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if entry.lastMod != "" && r.Header.Get("If-Modified-Since") == entry.lastMod {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(entry.size))

	if r.Method == http.MethodHead {
		w.WriteHeader(entry.statusCode)
		return
	}

	w.WriteHeader(entry.statusCode)
	w.Write(entry.body)
}

// Purge removes all page cache entries for a domain.
func (pc *PageCache) Purge(host string) int {
	prefix := "page:" + host
	count := 0
	pc.entries.Range(func(key, value interface{}) bool {
		k := key.(string)
		if strings.HasPrefix(k, prefix) {
			entry := value.(*pageEntry)
			pc.entries.Delete(key)
			pc.totalSize.Add(-int64(entry.size))
			pc.cache.totalSize.Add(-int64(entry.size))
			count++
		}
		return true
	})

	// Clear vary mappings for this domain
	pc.varyMap.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if strings.HasPrefix(k, host) {
			pc.varyMap.Delete(key)
		}
		return true
	})

	return count
}

// PurgeAll clears all page cache entries.
func (pc *PageCache) PurgeAll() int {
	count := 0
	pc.entries.Range(func(key, value interface{}) bool {
		entry := value.(*pageEntry)
		pc.entries.Delete(key)
		pc.totalSize.Add(-int64(entry.size))
		pc.cache.totalSize.Add(-int64(entry.size))
		count++
		return true
	})

	pc.varyMap.Range(func(key, _ interface{}) bool {
		pc.varyMap.Delete(key)
		return true
	})

	return count
}
