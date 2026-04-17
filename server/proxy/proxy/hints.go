package proxy

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// ODAC Early Hints — Learns Link:rel=preload from backend, serves 103 instantly
//
// How it works:
// 1. Backend sends "Link: </style.css>; rel=preload; as=style" in response headers
// 2. HintsStore records these per URL (domain + path)
// 3. On next request to the same URL, proxy sends 103 Early Hints BEFORE proxying
// 4. Browser starts fetching CSS/JS/fonts while waiting for the full response
//
// Safety:
// - Only stable hints are served (same hints seen consecutively)
// - Redirect responses (3xx) clear hints for that URL
// - Hints expire after hintsDefaultTTL without being refreshed
// - Purge per domain (on deploy) or globally
// ============================================================================

const (
	// hintsDefaultTTL is how long a hint entry lives without being refreshed
	// by a backend response. After this, the hint is evicted.
	hintsDefaultTTL = 10 * time.Minute

	// hintsCleanupInterval is how often stale hints are evicted.
	hintsCleanupInterval = 30 * time.Second

	// hintsMaxPerURL is the maximum number of Link headers stored per URL.
	hintsMaxPerURL = 20
)

// hintEntry stores learned Link preload headers for a single URL.
type hintEntry struct {
	links       []string     // Link header values (e.g. "</style.css>; rel=preload; as=style")
	fingerprint string       // Sorted concatenation of links for stability comparison
	stable      bool         // True if last two observations had the same fingerprint
	lastSeen    atomic.Int64 // Unix nano — refreshed on every backend response
}

// HintsStore learns and serves 103 Early Hints based on backend Link headers.
type HintsStore struct {
	entries sync.Map // key (domain+path) -> *hintEntry
	stopCh  chan struct{}
}

// NewHintsStore creates and starts the early hints engine.
func NewHintsStore() *HintsStore {
	hs := &HintsStore{
		stopCh: make(chan struct{}),
	}
	go hs.cleanupLoop()
	return hs
}

// Stop shuts down the cleanup goroutine.
func (hs *HintsStore) Stop() {
	close(hs.stopCh)
}

// Learn extracts Link:rel=preload headers from a backend response and
// records them for the given URL. Called from ModifyResponse on every
// proxied response — not just cacheable ones.
func (hs *HintsStore) Learn(host, urlPath string, resp *http.Response) {
	// Don't learn from redirects — the target page will have different assets
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		hs.entries.Delete(host + urlPath)
		return
	}

	// Only learn from successful HTML responses (pages, not assets)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		return
	}

	// Extract Link: rel=preload headers
	links := extractPreloadLinks(resp.Header)
	if len(links) == 0 {
		// Backend stopped sending preload hints — remove stale entry
		hs.entries.Delete(host + urlPath)
		return
	}

	// Cap to prevent abuse
	if len(links) > hintsMaxPerURL {
		links = links[:hintsMaxPerURL]
	}

	fingerprint := buildFingerprint(links)
	key := host + urlPath

	now := time.Now().UnixNano()

	val, loaded := hs.entries.Load(key)
	if loaded {
		entry := val.(*hintEntry)
		// Check stability: same hints as last time?
		if entry.fingerprint == fingerprint {
			entry.stable = true
		} else {
			// Hints changed — update but mark unstable until confirmed again
			entry.links = links
			entry.fingerprint = fingerprint
			entry.stable = false
		}
		entry.lastSeen.Store(now)
	} else {
		entry := &hintEntry{
			links:       links,
			fingerprint: fingerprint,
			stable:      false, // Need at least 2 consistent observations
		}
		entry.lastSeen.Store(now)
		hs.entries.Store(key, entry)
	}
}

// Send writes a 103 Early Hints response if stable hints exist for this URL.
// Must be called BEFORE the main response is written.
// Returns true if hints were sent.
func (hs *HintsStore) Send(w http.ResponseWriter, host, urlPath string) bool {
	val, ok := hs.entries.Load(host + urlPath)
	if !ok {
		return false
	}

	entry := val.(*hintEntry)
	if !entry.stable || len(entry.links) == 0 {
		return false
	}

	// Set Link headers and flush as 103
	for _, link := range entry.links {
		w.Header().Add("Link", link)
	}
	w.WriteHeader(http.StatusEarlyHints)
	return true
}

// Purge removes all hint entries for a specific domain.
func (hs *HintsStore) Purge(host string) int {
	prefix := host + "/"
	count := 0
	hs.entries.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if k == host || strings.HasPrefix(k, prefix) {
			hs.entries.Delete(key)
			count++
		}
		return true
	})
	return count
}

// PurgeAll clears all hint entries.
func (hs *HintsStore) PurgeAll() {
	hs.entries.Range(func(key, _ interface{}) bool {
		hs.entries.Delete(key)
		return true
	})
}

// ============================================================================
// Internal
// ============================================================================

// extractPreloadLinks returns Link header values that contain rel=preload.
func extractPreloadLinks(h http.Header) []string {
	var links []string
	for _, v := range h.Values("Link") {
		// A single Link header can contain multiple comma-separated entries
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			lower := strings.ToLower(part)
			if strings.Contains(lower, "rel=preload") || strings.Contains(lower, "rel=\"preload\"") {
				links = append(links, part)
			}
		}
	}
	return links
}

// buildFingerprint creates a stable string from sorted link values
// for comparing whether hints changed between responses.
func buildFingerprint(links []string) string {
	// Simple sort via insertion sort (small slices)
	sorted := make([]string, len(links))
	copy(sorted, links)
	for i := 1; i < len(sorted); i++ {
		j := i
		for j > 0 && sorted[j] < sorted[j-1] {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			j--
		}
	}
	return strings.Join(sorted, "|")
}

func (hs *HintsStore) cleanupLoop() {
	ticker := time.NewTicker(hintsCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-hs.stopCh:
			return
		case <-ticker.C:
			hs.evictExpired()
		}
	}
}

func (hs *HintsStore) evictExpired() {
	now := time.Now().UnixNano()
	ttl := hintsDefaultTTL.Nanoseconds()

	hs.entries.Range(func(key, value interface{}) bool {
		entry := value.(*hintEntry)
		if now-entry.lastSeen.Load() > ttl {
			hs.entries.Delete(key)
		}
		return true
	})
}
