package dataplane

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestControlCallsLeaveNoConnectionOpen pins H2: every control-socket call
// used to park its connection in a per-call Transport nothing reused, so
// both ends held a descriptor until a restart. The server here has no idle
// timeout, like the daemons had, so only the client can close.
func TestControlCallsLeaveNoConnectionOpen(t *testing.T) {
	dir, err := os.MkdirTemp("", "odacdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mod.sock")

	var open atomic.Int64
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"success":true}`))
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				open.Add(-1)
			}
		},
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	const calls = 50
	for i := 0; i < calls; i++ {
		if err := postRaw(sock, "/config", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := postStatus(sock, "/client-certs", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := requestJSON(sock, "POST", "/cache/purge", map[string]any{}, controlTimeout); err != nil {
			t.Fatal(err)
		}
		if _, err := requestStatus(sock, "DELETE", "/acme/challenge", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if _, err := getStatus(sock, "/ready", time.Second); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("%d control connections still open after %d calls", n, calls*5)
	}
}

// TestControlCallTimesOut pins the other half of H2: a daemon that accepts
// and never answers must not block a sync path forever.
func TestControlCallTimesOut(t *testing.T) {
	dir, err := os.MkdirTemp("", "odacdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mod.sock")

	release := make(chan struct{})
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	})}
	go srv.Serve(l)
	t.Cleanup(func() { close(release); srv.Close() })

	start := time.Now()
	if _, err := requestJSON(sock, "GET", "/accounts", nil, 100*time.Millisecond); err == nil {
		t.Fatal("a hung daemon answered")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call took %v despite a 100ms timeout", elapsed)
	}
}
