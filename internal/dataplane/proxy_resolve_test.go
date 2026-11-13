package dataplane

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// slowResolver counts lookups and can hold one until released, standing in
// for a daemon that stalls under I/O pressure.
type slowResolver struct {
	mu      sync.Mutex
	calls   map[string]int
	ip      string
	entered chan struct{}
	release chan struct{}
}

func (s *slowResolver) Available() bool { return true }

func (s *slowResolver) GetBackend(name, _ string) (string, string, error) {
	s.mu.Lock()
	s.calls[name]++
	s.mu.Unlock()
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
	}
	return s.ip, "", nil
}

func (s *slowResolver) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

// TestSyncInspectsEachContainerOnce: an app with many domains was inspected
// once per domain record, under the config write lock.
func TestSyncInspectsEachContainerOnce(t *testing.T) {
	cs := newControlServer(t)
	res := &slowResolver{calls: map[string]int{}, ip: "10.5.0.2"}
	p, _ := newTestProxy(t, cs, res)
	p.cfg.Set("apps", []any{
		map[string]any{"name": "web", "id": "a1", "ports": []any{map[string]any{"container": float64(8080)}}},
	})
	domains := map[string]any{}
	for i := 0; i < 50; i++ {
		domains["d"+strconv.Itoa(i)+".test"] = map[string]any{"appId": "web"}
	}
	p.cfg.Set("domains", domains)

	p.SyncConfig()
	payload := cs.nextConfig(t)
	if got := len(payload["domains"].(map[string]any)); got != 50 {
		t.Fatalf("%d domains pushed, want 50", got)
	}
	if got := res.count("web"); got != 1 {
		t.Fatalf("container inspected %d times, want 1", got)
	}
}

// TestSyncDoesNotHoldTheConfigLockOnDocker: while Docker has not answered,
// config readers (API authentication, autosave, the app pulse) must not wait.
func TestSyncDoesNotHoldTheConfigLockOnDocker(t *testing.T) {
	cs := newControlServer(t)
	res := &slowResolver{
		calls: map[string]int{}, ip: "10.5.0.2",
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	p, _ := newTestProxy(t, cs, res)
	p.cfg.Set("apps", []any{
		map[string]any{"name": "web", "id": "a1", "ports": []any{map[string]any{"container": float64(8080)}}},
	})
	p.cfg.Set("domains", map[string]any{"web.test": map[string]any{"appId": "web"}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.SyncConfig()
	}()
	select {
	case <-res.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sync never asked Docker")
	}

	viewed := make(chan struct{})
	go func() {
		p.cfg.Mutate(func() {})
		close(viewed)
	}()
	select {
	case <-viewed:
	case <-time.After(2 * time.Second):
		t.Fatal("the config write lock is held while Docker is asked")
	}

	close(res.release)
	<-done
	entry := cs.nextConfig(t)["domains"].(map[string]any)["web.test"].(map[string]any)
	if entry["containerIP"] != "10.5.0.2" {
		t.Fatalf("containerIP = %v", entry["containerIP"])
	}
}
