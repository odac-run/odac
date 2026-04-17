package proxy

import (
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// ODAC Smart Cache — Adaptive, Zero-Config, Memory-Aware Static Asset Cache
//
// Philosophy: The cache adapts to the machine it runs on. A 64GB server gets
// generous caching; a 512MB VPS gets conservative caching. If memory is tight,
// the cache disables itself entirely. No configuration required.
//
// Strategy: Stale-While-Revalidate with frequency-based admission control.
// Only assets that receive sustained traffic are cached. Cold assets are
// evicted after their TTL expires without a hit.
// ============================================================================

const (
	// cacheMaxFileSize is the maximum size of a single cacheable response (5MB).
	cacheMaxFileSize = 5 * 1024 * 1024

	// cacheDefaultTTL is how long a cache entry lives without being accessed.
	// Each hit resets the TTL timer. If no hit within this window, the entry is evicted.
	cacheDefaultTTL = 2 * time.Minute

	// cacheCleanupInterval is how often the background goroutine runs eviction.
	cacheCleanupInterval = 10 * time.Second

	// cacheMinFrequency is the minimum request frequency (req/sec) for an asset
	// to be admitted into cache. Prevents caching rarely-accessed files.
	cacheMinFrequency = 0.2 // ~1 request per 5 seconds

	// cacheHighFrequency is the threshold above which assets are always cached
	// regardless of memory pressure (as long as cache is not fully disabled).
	cacheHighFrequency = 2.0 // 2+ req/sec

	// cacheRevalidateInterval is the minimum time between backend revalidation
	// requests for the same cache entry. Prevents thundering herd: 5000 req/s
	// on a cached CSS file triggers at most 1 backend request per interval.
	cacheRevalidateInterval = 1 * time.Second

	// memoryFloorPercent is the minimum percentage of total RAM that must remain
	// free for the cache to operate. Below this, the cache disables itself.
	memoryFloorPercent = 20

	// memoryCeilingPercent is the maximum percentage of AVAILABLE (free) RAM
	// the cache may use. This ensures the cache scales with actual headroom.
	memoryCeilingPercent = 15

	// memoryPressurePercent — when free RAM drops below this, only high-frequency
	// assets are cached and low-frequency entries are aggressively evicted.
	memoryPressurePercent = 30
)

// cacheEntry represents a single cached response.
type cacheEntry struct {
	body        []byte
	contentType string
	etag        string
	headers     http.Header // Preserved response headers
	lastAccess  atomic.Int64
	lastMod     string
	size        int
	statusCode  int

	// Frequency tracking: exponentially weighted moving average of request rate
	hitCount  atomic.Int64
	createdAt int64 // unix nano

	// Revalidation throttle: prevents thundering herd of backend requests.
	// Only one revalidation goroutine runs per entry at a time, with a
	// minimum interval between attempts.
	revalidating    atomic.Bool  // true while a revalidation request is in-flight
	lastRevalidated atomic.Int64 // unix nano of last completed revalidation
}

// frequency returns the average requests per second since creation.
func (e *cacheEntry) frequency() float64 {
	elapsed := time.Since(time.Unix(0, e.createdAt)).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	return float64(e.hitCount.Load()) / elapsed
}

// ShouldRevalidate atomically checks whether this entry needs a background
// revalidation request. Returns true at most once per cacheRevalidateInterval,
// and only if no other goroutine is already revalidating this entry.
// The caller MUST call FinishRevalidation when done.
func (e *cacheEntry) ShouldRevalidate() bool {
	now := time.Now().UnixNano()
	last := e.lastRevalidated.Load()

	// Too soon since last revalidation
	if now-last < cacheRevalidateInterval.Nanoseconds() {
		return false
	}

	// Atomically claim the revalidation slot (CAS: false → true)
	return e.revalidating.CompareAndSwap(false, true)
}

// FinishRevalidation releases the revalidation lock and records the timestamp.
func (e *cacheEntry) FinishRevalidation() {
	e.lastRevalidated.Store(time.Now().UnixNano())
	e.revalidating.Store(false)
}

// CacheManager is the zero-config adaptive cache engine.
type CacheManager struct {
	entries   sync.Map // key (domain+path) -> *cacheEntry
	frequency sync.Map // key (domain+path) -> *frequencyRecord (pre-admission tracking)

	totalSize     atomic.Int64 // Total bytes cached
	maxSize       atomic.Int64 // Dynamic max size based on available RAM
	enabled       atomic.Bool  // Master switch — disabled under extreme memory pressure
	underPressure atomic.Bool  // True when memory is tight (only cache hot assets)

	stopCh chan struct{}
}

// frequencyRecord tracks request frequency BEFORE an asset is admitted to cache.
// This prevents one-off requests from polluting the cache.
type frequencyRecord struct {
	firstSeen int64 // unix nano
	count     atomic.Int64
}

// frequency returns requests per second for this pre-admission record.
func (fr *frequencyRecord) frequency() float64 {
	elapsed := time.Since(time.Unix(0, fr.firstSeen)).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	return float64(fr.count.Load()) / elapsed
}

// NewCacheManager creates and starts the adaptive cache engine.
func NewCacheManager() *CacheManager {
	cm := &CacheManager{
		stopCh: make(chan struct{}),
	}
	cm.enabled.Store(true)

	// Conservative default until Node.js sends first memory update via config sync
	cm.maxSize.Store(64 * 1024 * 1024) // 64MB

	go cm.maintenanceLoop()
	return cm
}

// Stop gracefully shuts down the cache maintenance goroutine.
func (cm *CacheManager) Stop() {
	close(cm.stopCh)
}

// cacheKey builds a unique key from domain + request path.
func cacheKey(host, urlPath string) string {
	return host + urlPath
}

// IsCacheable checks if a request is eligible for caching based on method and URL.
func IsCacheable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	// Don't cache requests with authorization or cookies that suggest dynamic content
	if r.Header.Get("Authorization") != "" {
		return false
	}

	// Don't cache requests with query strings — they often carry tokens,
	// session IDs, or cache-busting params that imply per-request variance.
	if r.URL.RawQuery != "" {
		return false
	}

	// Check URL extension
	ext := strings.ToLower(path.Ext(r.URL.Path))
	return cacheableExtension(ext)
}

// cacheableExtension returns true for static asset file extensions.
func cacheableExtension(ext string) bool {
	switch ext {
	case ".avif", ".bmp", ".css", ".eot", ".gif", ".ico",
		".jpeg", ".jpg", ".js", ".map", ".mjs",
		".otf", ".png", ".svg", ".ttf",
		".webp", ".woff", ".woff2":
		return true
	}
	return false
}

// assetPriority returns a weight multiplier based on asset type.
// Render-blocking resources (CSS, JS, fonts) get higher priority than images,
// which get higher priority than non-essential files (source maps).
// Used in admission control and eviction scoring: score = frequency × priority.
func assetPriority(urlPath string) float64 {
	ext := strings.ToLower(path.Ext(urlPath))
	switch ext {
	// Critical: render-blocking — browser can't paint without these
	case ".css", ".js", ".mjs":
		return 3.0
	// High: fonts cause layout shift (FOUT/FOIT) if not loaded fast
	case ".woff", ".woff2", ".ttf", ".eot", ".otf":
		return 2.5
	// Medium: visible content but non-blocking
	case ".svg", ".ico":
		return 2.0
	// Normal: images — important but browser can progressively render
	case ".webp", ".avif", ".png", ".jpg", ".jpeg", ".gif", ".bmp":
		return 1.0
	// Low: developer tools, not user-facing
	case ".map":
		return 0.3
	}
	return 1.0
}

// cacheableContentType validates that the response Content-Type matches
// what we expect for the file extension. Prevents caching error pages
// served with wrong extensions.
func cacheableContentType(ct string) bool {
	if idx := strings.Index(ct, ";"); idx != -1 {
		ct = ct[:idx]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))

	switch {
	case strings.HasPrefix(ct, "text/css"),
		strings.HasPrefix(ct, "text/javascript"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "image/"),
		strings.HasPrefix(ct, "font/"),
		strings.HasPrefix(ct, "application/font"),
		strings.HasPrefix(ct, "application/x-font"),
		ct == "application/octet-stream",
		ct == "image/svg+xml",
		ct == "application/wasm":
		return true
	}
	return false
}

// Get retrieves a cached response. Returns nil if not cached.
// Also records a hit for frequency tracking (pre-admission).
func (cm *CacheManager) Get(host, urlPath string) *cacheEntry {
	key := cacheKey(host, urlPath)

	// Always record frequency, even on miss (for admission control)
	cm.recordFrequency(key)

	if !cm.enabled.Load() {
		return nil
	}

	val, ok := cm.entries.Load(key)
	if !ok {
		return nil
	}

	entry := val.(*cacheEntry)
	entry.lastAccess.Store(time.Now().UnixNano())
	entry.hitCount.Add(1)
	return entry
}

// Put stores a response in cache if admission criteria are met.
func (cm *CacheManager) Put(host, urlPath string, resp *http.Response, body []byte) {
	if !cm.enabled.Load() {
		return
	}

	// Size guard
	if len(body) > cacheMaxFileSize || len(body) == 0 {
		return
	}

	// Content-Type validation
	ct := resp.Header.Get("Content-Type")
	if !cacheableContentType(ct) {
		return
	}

	// Only cache successful responses
	if resp.StatusCode != http.StatusOK {
		return
	}

	// Respect Cache-Control: no-store, private
	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") {
		return
	}

	// Never cache responses that set cookies — they are per-user/session
	if resp.Header.Get("Set-Cookie") != "" {
		return
	}

	// Never cache responses with Vary: Cookie or Vary: * (uncacheable by definition)
	if vary := strings.ToLower(resp.Header.Get("Vary")); vary != "" {
		if strings.Contains(vary, "*") || strings.Contains(vary, "cookie") || strings.Contains(vary, "authorization") {
			return
		}
	}

	key := cacheKey(host, urlPath)

	// Admission control: check if this asset has enough request frequency
	if !cm.shouldAdmit(key, urlPath, len(body)) {
		return
	}

	// Preserve relevant headers
	headers := make(http.Header)
	for _, h := range []string{
		"Cache-Control", "Content-Type", "ETag",
		"Last-Modified", "Vary",
	} {
		if v := resp.Header.Get(h); v != "" {
			headers.Set(h, v)
		}
	}

	now := time.Now()
	entry := &cacheEntry{
		body:        body,
		contentType: ct,
		createdAt:   now.UnixNano(),
		etag:        resp.Header.Get("ETag"),
		headers:     headers,
		lastMod:     resp.Header.Get("Last-Modified"),
		size:        len(body),
		statusCode:  resp.StatusCode,
	}
	entry.lastAccess.Store(now.UnixNano())
	entry.hitCount.Store(1)

	// Check if replacing existing entry
	if old, loaded := cm.entries.LoadAndDelete(key); loaded {
		cm.totalSize.Add(-int64(old.(*cacheEntry).size))
	}

	cm.entries.Store(key, entry)
	cm.totalSize.Add(int64(entry.size))
}

// Purge removes all cache entries for a specific domain (app).
// Called after deployments to ensure fresh assets are served.
func (cm *CacheManager) Purge(host string) int {
	prefix := host + "/"
	count := 0

	cm.entries.Range(func(key, value interface{}) bool {
		k := key.(string)
		if k == host || strings.HasPrefix(k, prefix) {
			entry := value.(*cacheEntry)
			cm.entries.Delete(key)
			cm.totalSize.Add(-int64(entry.size))
			count++
		}
		return true
	})

	// Also clear frequency records for this domain
	cm.frequency.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if k == host || strings.HasPrefix(k, prefix) {
			cm.frequency.Delete(key)
		}
		return true
	})

	return count
}

// PurgeAll clears the entire cache.
func (cm *CacheManager) PurgeAll() int {
	count := 0
	cm.entries.Range(func(key, value interface{}) bool {
		entry := value.(*cacheEntry)
		cm.entries.Delete(key)
		cm.totalSize.Add(-int64(entry.size))
		count++
		return true
	})

	// Clear all frequency records
	cm.frequency.Range(func(key, _ interface{}) bool {
		cm.frequency.Delete(key)
		return true
	})

	return count
}

// Stats returns current cache statistics.
func (cm *CacheManager) Stats() map[string]interface{} {
	entryCount := 0
	cm.entries.Range(func(_, _ interface{}) bool {
		entryCount++
		return true
	})

	return map[string]interface{}{
		"enabled":       cm.enabled.Load(),
		"entries":       entryCount,
		"maxSizeMB":     cm.maxSize.Load() / (1024 * 1024),
		"totalSizeMB":   cm.totalSize.Load() / (1024 * 1024),
		"underPressure": cm.underPressure.Load(),
	}
}

// ServeFromCache writes a cached response to the client.
func ServeFromCache(w http.ResponseWriter, r *http.Request, entry *cacheEntry) {
	// Copy preserved headers (only safe-to-cache headers were stored in Put)
	for key, vals := range entry.headers {
		for _, v := range vals {
			w.Header().Set(key, v)
		}
	}

	// Age header (RFC 7234): how many seconds the response has been in cache
	age := int(time.Since(time.Unix(0, entry.createdAt)).Seconds())
	w.Header().Set("Age", strconv.Itoa(age))
	w.Header().Set("X-Odac-Cache", "HIT")
	w.Header().Set("Server", "ODAC")

	// Handle conditional requests (If-None-Match)
	if entry.etag != "" && r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Handle conditional requests (If-Modified-Since)
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

// ============================================================================
// Internal: Admission Control
// ============================================================================

// recordFrequency tracks request frequency for a key before it enters cache.
func (cm *CacheManager) recordFrequency(key string) {
	val, loaded := cm.frequency.LoadOrStore(key, &frequencyRecord{
		firstSeen: time.Now().UnixNano(),
	})
	fr := val.(*frequencyRecord)
	if loaded {
		fr.count.Add(1)
	} else {
		fr.count.Store(1)
	}
}

// shouldAdmit decides if an asset should be admitted to cache based on
// its request frequency, asset priority, and current memory conditions.
// Score = frequency × priority weight. A 1 req/s CSS (×3.0 = 3.0) beats
// a 2 req/s source map (×0.3 = 0.6) when memory is tight.
func (cm *CacheManager) shouldAdmit(key, urlPath string, size int) bool {
	// Would this exceed our memory budget?
	if cm.totalSize.Load()+int64(size) > cm.maxSize.Load() {
		// Try eviction first
		cm.evictLRU(int64(size))
		// Re-check after eviction
		if cm.totalSize.Load()+int64(size) > cm.maxSize.Load() {
			return false
		}
	}

	// Check pre-admission frequency
	val, ok := cm.frequency.Load(key)
	if !ok {
		return false
	}

	fr := val.(*frequencyRecord)
	score := fr.frequency() * assetPriority(urlPath)

	// Under memory pressure: only admit high-score assets
	if cm.underPressure.Load() {
		return score >= cacheHighFrequency
	}

	return score >= cacheMinFrequency
}

// ============================================================================
// Internal: Memory Management
// ============================================================================

// UpdateMemory recalculates cache limits based on host memory info from Node.js.
// Called on every config sync, so the cache adapts in near-real-time to the
// actual system state — even if the proxy runs in an isolated container.
func (cm *CacheManager) UpdateMemory(total, used uint64) {
	if total == 0 {
		return
	}

	available := total - used
	if used > total {
		available = 0
	}

	freePercent := float64(available) / float64(total) * 100

	// Critical: disable cache entirely when free RAM is dangerously low
	if freePercent < float64(memoryFloorPercent) {
		if cm.enabled.Load() {
			log.Printf("[Cache] Memory critical (%.0f%% free of %dMB). Disabling cache.",
				freePercent, total/(1024*1024))
			cm.enabled.Store(false)
			cm.PurgeAll()
		}
		return
	}

	// Re-enable if previously disabled and memory recovered
	if !cm.enabled.Load() {
		log.Printf("[Cache] Memory recovered (%.0f%% free). Re-enabling cache.", freePercent)
		cm.enabled.Store(true)
	}

	// Set pressure flag
	wasPressure := cm.underPressure.Load()
	isPressure := freePercent < float64(memoryPressurePercent)
	cm.underPressure.Store(isPressure)

	if isPressure && !wasPressure {
		log.Printf("[Cache] Memory pressure detected (%.0f%% free). Restricting to hot assets only.", freePercent)
	} else if !isPressure && wasPressure {
		log.Printf("[Cache] Memory pressure relieved (%.0f%% free). Normal caching resumed.", freePercent)
	}

	// Max cache size = percentage of AVAILABLE (free) RAM, not total.
	// On a 64GB machine with 50GB free → up to 7.5GB cache.
	// On a 512MB VPS with 200MB free → up to 30MB cache.
	newMax := int64(float64(available) * float64(memoryCeilingPercent) / 100)

	// Floor: at least 16MB if cache is enabled
	if newMax < 16*1024*1024 {
		newMax = 16 * 1024 * 1024
	}

	cm.maxSize.Store(newMax)
}

// ============================================================================
// Internal: Eviction
// ============================================================================

// evictExpired removes entries that haven't been accessed within their TTL.
func (cm *CacheManager) evictExpired() {
	now := time.Now().UnixNano()
	ttlNanos := cacheDefaultTTL.Nanoseconds()

	cm.entries.Range(func(key, value interface{}) bool {
		entry := value.(*cacheEntry)
		lastAccess := entry.lastAccess.Load()

		if now-lastAccess > ttlNanos {
			cm.entries.Delete(key)
			cm.totalSize.Add(-int64(entry.size))
			debugLog("[Cache] TTL evicted: %s (freq: %.2f req/s)", key, entry.frequency())
		}
		return true
	})

	// Also clean stale frequency records (older than 2x TTL with no cache entry)
	staleThreshold := 2 * ttlNanos
	cm.frequency.Range(func(key, value interface{}) bool {
		fr := value.(*frequencyRecord)
		if now-fr.firstSeen > staleThreshold {
			// Only delete if there's no corresponding cache entry
			if _, exists := cm.entries.Load(key); !exists {
				cm.frequency.Delete(key)
			}
		}
		return true
	})
}

// evictLRU evicts least-valuable entries until `needed` bytes are freed.
// Value = frequency × asset priority. Low-value entries are evicted first.
func (cm *CacheManager) evictLRU(needed int64) {
	var candidates []evictCandidate
	cm.entries.Range(func(key, value interface{}) bool {
		entry := value.(*cacheEntry)
		k := key.(string)
		candidates = append(candidates, evictCandidate{
			key:        k,
			lastAccess: entry.lastAccess.Load(),
			score:      entry.frequency() * assetPriority(k),
			size:       entry.size,
		})
		return true
	})

	if len(candidates) == 0 {
		return
	}

	// Sort by eviction priority: lowest score first, then oldest access
	sortCandidates(candidates)

	freed := int64(0)
	for _, c := range candidates {
		if freed >= needed {
			break
		}
		if val, loaded := cm.entries.LoadAndDelete(c.key); loaded {
			entry := val.(*cacheEntry)
			cm.totalSize.Add(-int64(entry.size))
			freed += int64(entry.size)
			debugLog("[Cache] LRU evicted: %s (score: %.2f, size: %d)", c.key, c.score, c.size)
		}
	}
}

// evictUnderPressure aggressively removes low-value entries when memory is tight.
func (cm *CacheManager) evictUnderPressure() {
	if !cm.underPressure.Load() {
		return
	}

	cm.entries.Range(func(key, value interface{}) bool {
		entry := value.(*cacheEntry)
		score := entry.frequency() * assetPriority(key.(string))
		if score < cacheHighFrequency {
			cm.entries.Delete(key)
			cm.totalSize.Add(-int64(entry.size))
			debugLog("[Cache] Pressure evicted: %s (score: %.2f)", key, score)
		}
		return true
	})
}

// evictCandidate represents a cache entry being considered for eviction.
type evictCandidate struct {
	key        string
	lastAccess int64
	score      float64 // frequency × asset priority
	size       int
}

// sortCandidates sorts eviction candidates: lowest score first,
// then oldest lastAccess as tiebreaker. Simple insertion sort since
// eviction runs infrequently and candidate lists are typically small.
func sortCandidates(c []evictCandidate) {
	for i := 1; i < len(c); i++ {
		j := i
		for j > 0 && (c[j].score < c[j-1].score ||
			(c[j].score == c[j-1].score && c[j].lastAccess < c[j-1].lastAccess)) {
			c[j], c[j-1] = c[j-1], c[j]
			j--
		}
	}
}

// ============================================================================
// Internal: Maintenance Loop
// ============================================================================

func (cm *CacheManager) maintenanceLoop() {
	cleanupTicker := time.NewTicker(cacheCleanupInterval)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-cm.stopCh:
			return
		case <-cleanupTicker.C:
			cm.evictExpired()
			cm.evictUnderPressure()
		}
	}
}
