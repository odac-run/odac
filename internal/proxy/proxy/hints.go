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
// - Send answers every client before the app has checked who is asking, so only an
//   anonymous GET of the bare URL with a shareable response may teach a hint
// - A request with credentials or a query says nothing about that view and is ignored
// - An anonymous response the URL can no longer share withdraws its hints
// - Only stable hints are served (same hints seen consecutively)
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
	lastSeen    atomic.Int64 // Unix nano, the only field that changes after publication
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

// Learn records the Link:rel=preload headers of a backend response for the given URL.
// Called from ModifyResponse on every proxied response, not just cacheable ones.
func (hs *HintsStore) Learn(host, urlPath string, resp *http.Response) {
	if !isAnonymousPageRequest(resp.Request) {
		return
	}
	// A 304 carries no verdict on the page, and a 101 is not the page at all.
	if resp.StatusCode < http.StatusOK || resp.StatusCode == http.StatusNotModified {
		return
	}

	key := host + urlPath
	if resp.StatusCode >= http.StatusMultipleChoices || isPersonalResponse(resp.Header) {
		hs.entries.Delete(key)
		return
	}

	// Only learn from HTML responses (pages, not assets)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		return
	}

	// Extract Link: rel=preload headers
	links := extractPreloadLinks(resp.Header)
	if len(links) == 0 {
		// Backend stopped sending preload hints, so remove the stale entry
		hs.entries.Delete(key)
		return
	}

	// Cap to prevent abuse
	if len(links) > hintsMaxPerURL {
		links = links[:hintsMaxPerURL]
	}

	// Entries are replaced rather than edited because Send reads them without a lock.
	entry := &hintEntry{links: links, fingerprint: buildFingerprint(links)}
	now := time.Now().UnixNano()
	if val, loaded := hs.entries.Load(key); loaded {
		prev := val.(*hintEntry)
		if prev.fingerprint == entry.fingerprint {
			if prev.stable {
				prev.lastSeen.Store(now)
				return
			}
			entry.stable = true
		}
	}
	entry.lastSeen.Store(now)
	hs.entries.Store(key, entry)
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

	// Set Link headers and flush as 103. They must not stay on the writer: the final
	// response would carry them next to the app's own, whatever its status.
	for _, link := range entry.links {
		w.Header().Add("Link", link)
	}
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Link")
	return true
}

// Purge removes all hint entries for a specific domain.
func (hs *HintsStore) Purge(host string) int {
	count := 0
	hs.entries.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if keyBelongsToHost(k, host) {
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

// isAnonymousPageRequest reports a request whose response is what any visitor of the bare
// URL would get. A query can be a capability (/share?token=...), and the key has no room
// for it: adding it would give scanners an endless supply of entries.
func isAnonymousPageRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodGet && r.URL.RawQuery == "" &&
		r.Header.Get("Cookie") == "" && r.Header.Get("Authorization") == ""
}

// isPersonalResponse reports a response whose headers say it is not meant for everyone.
func isPersonalResponse(h http.Header) bool {
	return cacheControlForbidsSharing(h) || h.Get("Set-Cookie") != "" || varyIsPersonal(h)
}

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
