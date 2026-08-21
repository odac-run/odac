package blob

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// RefChecker reports whether a blob is still referenced by stored mail.
// Keeping the sweeper behind this one method leaves the blob package free of
// any dependency on the database layer.
type RefChecker interface {
	RawRefExists(ctx context.Context, ref string) (bool, error)
}

const (
	defaultSweepInterval = 6 * time.Hour
	// defaultGrace must comfortably exceed the window between writing a blob
	// and committing the row that references it. A blob younger than this is
	// never collected, so a delivery in flight cannot lose its message.
	defaultGrace = 24 * time.Hour
)

// Sweeper reclaims blobs that no message row references any more. Content
// addressing means a blob outlives the row that created it whenever a message
// is deleted, so without this the store grows without bound.
type Sweeper struct {
	store    *Store
	refs     RefChecker
	interval time.Duration
	grace    time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSweeper creates a sweeper for the given store.
// ODAC_MAIL_SWEEP_HOURS overrides the interval; 0 disables sweeping entirely.
func NewSweeper(store *Store, refs RefChecker) *Sweeper {
	interval := defaultSweepInterval
	if raw := os.Getenv("ODAC_MAIL_SWEEP_HOURS"); raw != "" {
		if h, err := strconv.Atoi(raw); err == nil && h >= 0 {
			interval = time.Duration(h) * time.Hour
		} else {
			log.Printf("[Mail Sweeper] Ignoring invalid ODAC_MAIL_SWEEP_HOURS=%q", raw)
		}
	}
	return &Sweeper{store: store, refs: refs, interval: interval, grace: defaultGrace}
}

// Start begins periodic sweeping on a tracked goroutine.
func (s *Sweeper) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		return
	}
	if s.interval <= 0 {
		log.Println("[Mail Sweeper] Disabled by configuration")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		log.Printf("[Mail Sweeper] Started (every %s, grace %s)", s.interval, s.grace)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Sweep(ctx)
			}
		}
	}()
}

// Stop halts sweeping and waits for an in-flight sweep to finish.
func (s *Sweeper) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	s.wg.Wait()
	log.Println("[Mail Sweeper] Stopped")
}

// Sweep deletes unreferenced blobs older than the grace period once, returning
// how many were removed and how many bytes that reclaimed.
func (s *Sweeper) Sweep(ctx context.Context) (int, int64) {
	cutoff := time.Now().Add(-s.grace).Unix()
	var deleted int
	var freed int64
	var scanned int

	err := s.store.Walk(func(ref string, size int64, modTime int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		scanned++
		if modTime > cutoff {
			return nil
		}
		referenced, err := s.refs.RawRefExists(ctx, ref)
		if err != nil {
			// A lookup failure must never be read as "unreferenced": that
			// would delete live mail. Skip the candidate and retry next sweep.
			log.Printf("[Mail Sweeper] Reference check failed for %s, keeping it: %v", ref, err)
			return nil
		}
		if referenced {
			return nil
		}
		if err := s.store.Delete(ref); err != nil {
			log.Printf("[Mail Sweeper] Delete failed for %s: %v", ref, err)
			return nil
		}
		deleted++
		freed += size
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("[Mail Sweeper] Walk failed: %v", err)
	}

	if deleted > 0 {
		log.Printf("[Mail Sweeper] Reclaimed %d orphaned messages (%d bytes) from %d scanned", deleted, freed, scanned)
	}
	return deleted, freed
}
