package dataplane

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

func TestMain(m *testing.M) {
	logx.Stdout = io.Discard
	logx.Stderr = io.Discard
	os.Exit(m.Run())
}

// fakeProc replaces the supervisor: services only need Running/SocketPath for
// config pushes plus call tracking for Start/Stop/Check semantics.
type fakeProc struct {
	mu      sync.Mutex
	running bool
	socket  string
	ensures int
	stops   int
}

func (f *fakeProc) Ensure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	f.running = true
}

func (f *fakeProc) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.running = false
}

func (f *fakeProc) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeProc) SocketPath() string { return f.socket }

func (f *fakeProc) counts() (ensures, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures, f.stops
}

// controlServer fakes a data-plane control API on a unix socket.
type controlServer struct {
	sock    string
	configs chan []byte
	ready   atomic.Int32
}

func newControlServer(t *testing.T) *controlServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "odacdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	cs := &controlServer{
		sock:    filepath.Join(dir, "mod.sock"),
		configs: make(chan []byte, 16),
	}
	cs.ready.Store(http.StatusOK)

	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.configs <- body
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(cs.ready.Load()))
	})

	l, err := net.Listen("unix", cs.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return cs
}

func (cs *controlServer) nextConfig(t *testing.T) map[string]any {
	t.Helper()
	select {
	case raw := <-cs.configs:
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("invalid config payload: %v\n%s", err, raw)
		}
		return payload
	case <-time.After(5 * time.Second):
		t.Fatal("no config push received")
		return nil
	}
}

func (cs *controlServer) expectNoConfig(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case raw := <-cs.configs:
		t.Fatalf("unexpected config push: %s", raw)
	case <-time.After(wait):
	}
}

func newStore(t *testing.T) *config.Store {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeResolver implements ContainerIPs.
type fakeResolver struct {
	available atomic.Bool
	ips       map[string]string
}

func (f *fakeResolver) Available() bool { return f.available.Load() }

func (f *fakeResolver) GetIP(name string) (string, error) {
	if ip, ok := f.ips[name]; ok {
		return ip, nil
	}
	return "", errors.New("no such container")
}

// fakeIPSource implements DNSService (IPSource + a recording Record).
type fakeIPSource struct {
	v4, v6  []IPEntry
	primary string

	mu      sync.Mutex
	records []map[string]any
}

func (f *fakeIPSource) IPInfo() ([]IPEntry, []IPEntry, string) { return f.v4, f.v6, f.primary }

func (f *fakeIPSource) Record(args ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, args...)
}

func (f *fakeIPSource) recorded() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.records...)
}
