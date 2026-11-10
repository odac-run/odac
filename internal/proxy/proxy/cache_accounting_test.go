package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func cssResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/css"}}}
}

func TestConcurrentPutsKeepSizeAccountingExact(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()
	key := cacheKey("a.com", "/a.css")
	for range 10 {
		cm.recordFrequency(key)
	}
	body := make([]byte, 1024)

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				cm.Put("a.com", "/a.css", cssResponse(), body)
			}
		}()
	}
	wg.Wait()
	if got := cm.totalSize.Load(); got != int64(len(body)) {
		t.Fatalf("totalSize = %d with one %d byte entry", got, len(body))
	}
}

func TestRemovalNeverSubtractsAReplacedEntry(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()
	key := cacheKey("a.com", "/a.css")
	for range 10 {
		cm.recordFrequency(key)
	}
	cm.Put("a.com", "/a.css", cssResponse(), make([]byte, 100))
	old, _ := cm.entries.Load(key)
	cm.Put("a.com", "/a.css", cssResponse(), make([]byte, 300))

	if cm.removeEntry(key, old.(*cacheEntry)) {
		t.Fatal("removed the entry that replaced the one the caller saw")
	}
	if got := cm.totalSize.Load(); got != 300 {
		t.Fatalf("totalSize = %d, want 300", got)
	}
}

func TestRefusedAssetEvictsNothing(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()
	cm.maxSize.Store(1000)
	for i := range 5 {
		path := fmt.Sprintf("/%d.css", i)
		for range 10 {
			cm.recordFrequency(cacheKey("a.com", path))
		}
		cm.Put("a.com", path, cssResponse(), make([]byte, 200))
	}
	// Never requested, so admission refuses it; it must not cost the cache an entry.
	cm.Put("a.com", "/cold.css", cssResponse(), make([]byte, 200))
	if got := cm.totalSize.Load(); got != 1000 {
		t.Fatalf("totalSize = %d after a refused store, want 1000", got)
	}
}

func TestFrequencyTableIsBounded(t *testing.T) {
	cm := NewCacheManager()
	defer cm.Stop()

	cm.recordFrequency("a.com/" + strings.Repeat("x", cacheMaxKeyLen) + ".js")
	if cm.freqCount.Load() != 0 {
		t.Fatal("an oversized key was tracked")
	}

	cm.freqCount.Store(cacheMaxFrequencyRecords)
	cm.recordFrequency("a.com/new.js")
	if _, ok := cm.frequency.Load("a.com/new.js"); ok {
		t.Fatal("a full table took another record")
	}
	cm.freqCount.Store(0)

	cm.recordFrequency("a.com/one.js")
	cm.recordFrequency("a.com/one.js")
	cm.Purge("a.com")
	if got := cm.freqCount.Load(); got != 0 {
		t.Fatalf("freqCount = %d after purging every record", got)
	}
}
