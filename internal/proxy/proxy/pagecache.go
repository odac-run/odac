package proxy

import (
	"hash/fnv"
	"net"
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

	// pageCandidateWindow is how long a first observation waits for a second client.
	pageCandidateWindow = 10 * time.Minute

	// pageCandidateOverhead approximates a candidate's memory beyond its key.
	pageCandidateOverhead = 512
)

// varyWhitelist defines which Vary header fields are safe to use as cache
// key variants. Fields not in this list cause the response to be uncacheable.
// "Accept-Encoding" is always ignored (we cache raw body, compress on serve).
var varyWhitelist = map[string]bool{
	"accept":           true,
	"x-odac":           true,
	"x-requested-with": true,
}

// pageEntry represents a cached HTML page response. An entry starts as a candidate that
// holds only the body's hash, and a confirmed copy replaces it, so these fields never
// change once the entry is stored. Keeping no body until then matters because scanners
// request endless URLs: on an app that opts in from a catch-all route, each one would
// otherwise pin a full page in the shared budget for the whole TTL.
type pageEntry struct {
	key        string
	urlKey     string
	body       []byte
	bodyHash   uint64 // FNV-1a hash of body for stability detection
	headers    http.Header
	etag       string
	lastMod    string
	size       int // Bytes charged against the budget
	statusCode int
	stable     bool          // True after two distinct clients saw the same body
	ttl        time.Duration // App-specified TTL from X-Odac-Cache
	createdAt  int64         // Unix nano
	firstIP    string        // Source IP of the first observation (pre-stable)

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
	entries   sync.Map      // key -> *pageEntry
	varyMap   sync.Map      // "host+path" -> vary header string (learned from backend)
	totalSize atomic.Int64  // Shared memory accounting with CacheManager
	cache     *CacheManager // Reference for memory limit checks
}

// NewPageCache creates a page cache that shares memory limits with the asset cache.
func NewPageCache(cm *CacheManager) *PageCache {
	pc := &PageCache{cache: cm}
	cm.pages.Store(pc)
	return pc
}

// hasRoom caps pages at half the shared budget, because the asset cache cannot evict them
// and would otherwise stop admitting anything once they filled it.
func (pc *PageCache) hasRoom(size int64) bool {
	maxSize := pc.cache.maxSize.Load()
	return pc.totalSize.Load()+size <= maxSize/2 && pc.cache.totalSize.Load()+size <= maxSize
}

func (pc *PageCache) charge(size int64) {
	pc.totalSize.Add(size)
	pc.cache.totalSize.Add(size)
}

// remove releases the entry's budget only if this call is the one that removed it, so
// racing requests for an expired page cannot release it twice.
func (pc *PageCache) remove(entry *pageEntry) {
	if pc.entries.CompareAndDelete(entry.key, entry) {
		pc.charge(-int64(entry.size))
	}
}

// evictExpired drops what no request will ever come back for, since Get only notices
// expiry on the URL it was asked about.
func (pc *PageCache) evictExpired() {
	live := make(map[string]struct{})
	pc.entries.Range(func(_, value interface{}) bool {
		entry := value.(*pageEntry)
		if entry.expired() {
			pc.remove(entry)
		} else {
			live[entry.urlKey] = struct{}{}
		}
		return true
	})
	pc.varyMap.Range(func(key, _ interface{}) bool {
		if _, ok := live[key.(string)]; !ok {
			pc.varyMap.Delete(key)
		}
		return true
	})
}

// clientIP returns the source IP of a request (without the port), used to tell
// two observations apart when deciding whether a body is safe to promote.
func clientIP(r *http.Request) string {
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return ip
	}
	return r.RemoteAddr
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
	return !cacheControlForbidsSharing(resp.Header)
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
		pc.remove(entry)
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
	if !pc.cache.enabled.Load() || pc.cache.underPressure.Load() || ttl <= 0 || isCompressedBody(resp.Header) {
		return
	}

	// Only anonymous (cookieless) requests may populate the page cache. A
	// request carrying cookies can receive per-user HTML; caching that body
	// would let it be served to other users (a cross-user data leak). A
	// cookieless response is, by construction, not personalized by session, so
	// it is safe to share with everyone — including cookied users on a hit.
	if r.Header.Get("Cookie") != "" {
		return
	}

	if len(body) > pageMaxBodySize || len(body) == 0 {
		return
	}

	varyHeader := varyFields(resp.Header)
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
	ip := clientIP(r)

	// Stability check: if an entry already exists, compare body hashes.
	// Two identical responses from DISTINCT clients = stable (safe to serve).
	// Different body = dynamic content (e.g. CSRF tokens), don't promote.
	if existing, loaded := pc.entries.Load(key); loaded {
		entry := existing.(*pageEntry)
		if entry.bodyHash == newHash {
			// Promote to stable only once a second, distinct client (different
			// source IP) has produced the identical body. A single client
			// reloading its own page must never stabilize a response — that is
			// exactly how one user's page would otherwise be pinned and served
			// to everyone.
			if !entry.stable && ip != entry.firstIP {
				pc.promote(entry, resp, body, ttl)
			}
			return
		}
		// Content changed: replace entry, reset stability
		pc.remove(entry)
		debugLog("[PageCache] Content changed (dynamic?): %s", key)
	}

	candidate := &pageEntry{
		key:       key,
		urlKey:    urlKey,
		bodyHash:  newHash,
		createdAt: time.Now().UnixNano(),
		size:      pageCandidateOverhead + len(key),
		ttl:       min(ttl, pageCandidateWindow),
		firstIP:   ip,
	}
	if !pc.hasRoom(int64(candidate.size)) {
		return
	}
	if _, loaded := pc.entries.LoadOrStore(key, candidate); !loaded {
		pc.charge(int64(candidate.size))
	}
}

// promote replaces a candidate with a servable copy. The TTL starts here, since this is
// the body's first time being served.
func (pc *PageCache) promote(candidate *pageEntry, resp *http.Response, body []byte, ttl time.Duration) {
	if !pc.hasRoom(int64(len(body))) {
		return
	}

	now := time.Now()
	entry := &pageEntry{
		key:      candidate.key,
		urlKey:   candidate.urlKey,
		body:     body,
		bodyHash: candidate.bodyHash,
		headers: storedHeaders(resp.Header, replayedHeaders, []string{
			"Cache-Control", "Content-Language", "Content-Type",
			"ETag", "Last-Modified", "Link", "Vary", "X-Robots-Tag",
		}),
		etag:       resp.Header.Get("ETag"),
		lastMod:    resp.Header.Get("Last-Modified"),
		size:       len(body),
		statusCode: resp.StatusCode,
		stable:     true,
		ttl:        ttl,
		createdAt:  now.UnixNano(),
	}
	entry.lastAccess.Store(now.UnixNano())
	entry.hitCount.Store(1)

	if pc.entries.CompareAndSwap(entry.key, candidate, entry) {
		pc.charge(int64(entry.size - candidate.size))
		debugLog("[PageCache] Stability confirmed (2 clients): %s", entry.key)
	}
}

// ServePageFromCache writes a cached page response to the client.
func ServePageFromCache(w http.ResponseWriter, r *http.Request, entry *pageEntry) {
	restoreHeaders(w.Header(), entry.headers)

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

// Purge removes all page cache entries for a domain and its subdomains.
func (pc *PageCache) Purge(host string) int {
	pc.cache.invalidateFetches()
	count := 0
	pc.entries.Range(func(key, value interface{}) bool {
		k := key.(string)
		if keyBelongsToHost(strings.TrimPrefix(k, "page:"), host) {
			pc.remove(value.(*pageEntry))
			count++
		}
		return true
	})

	// Clear vary mappings for this domain
	pc.varyMap.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if keyBelongsToHost(k, host) {
			pc.varyMap.Delete(key)
		}
		return true
	})

	return count
}

// PurgeAll clears all page cache entries.
func (pc *PageCache) PurgeAll() int {
	pc.cache.invalidateFetches()
	count := 0
	pc.entries.Range(func(_, value interface{}) bool {
		pc.remove(value.(*pageEntry))
		count++
		return true
	})

	pc.varyMap.Range(func(key, _ interface{}) bool {
		pc.varyMap.Delete(key)
		return true
	})

	return count
}
