package api

import (
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startLimitedServer starts a server with the given limits, which must be set
// before Start because each accept loop reads them once.
func startLimitedServer(t *testing.T, maxBytes int64, timeout time.Duration, pending int) *Server {
	t.Helper()
	s := newTestServer(t)
	s.MaxRequestBytes = maxBytes
	s.ReadTimeout = timeout
	s.MaxPending = pending
	s.Init()
	s.Start()
	t.Cleanup(s.Stop)
	waitListening(t, s)
	waitUnix(t, s)
	return s
}

func waitUnix(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		l := s.unix
		s.mu.Unlock()
		if l != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never bound its unix socket")
}

func dialUnix(t *testing.T, s *Server) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", s.SocketPath(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readUntilClosed reads until the server closes the connection, failing when
// it is still open after wait.
func readUntilClosed(t *testing.T, conn net.Conn, wait time.Duration) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(wait))
	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("connection still open after %s (read %q): %v", wait, data, err)
	}
	return string(data)
}

// An oversized document is cut off at the cap instead of being buffered whole,
// with or without a valid token in it.
func TestOversizedRequestIsRefusedAtTheCap(t *testing.T) {
	s := startLimitedServer(t, 64<<10, 5*time.Second, 4)
	var ran atomic.Bool
	s.Register("app.list", func(Args, Progress) (*Result, error) { ran.Store(true); r := Res(true, "ok"); return &r, nil })

	conn := dialUnix(t, s)
	go conn.Write([]byte(`{"auth":"` + fixtureKey + `","action":"app.list","data":["` + strings.Repeat("a", 256<<10) + `"]}`))
	got := readUntilClosed(t, conn, 2*time.Second)
	if !strings.Contains(got, `"message":"request_too_large"`) {
		t.Fatalf("oversized request answered %q", got)
	}
	if ran.Load() {
		t.Fatal("the handler ran for an oversized request")
	}

	// A request under the cap is untouched.
	lines := call(t, "unix", s.SocketPath(), request(fixtureKey, "app.list", strings.Repeat("a", 32<<10)))
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"result":true`) {
		t.Fatalf("request under the cap = %v", lines)
	}
}

// A peer that connects and sends nothing, or half a document, is closed when
// the read timeout runs out.
func TestIdleAndTricklingConnectionsAreClosed(t *testing.T) {
	s := startLimitedServer(t, 64<<10, 200*time.Millisecond, 4)

	idle := dialUnix(t, s)
	readUntilClosed(t, idle, 2*time.Second)

	partial := dialUnix(t, s)
	partial.Write([]byte(`{"auth":"`))
	readUntilClosed(t, partial, 2*time.Second)
}

// The timeout covers everything before a handler, not each document: a peer
// retrying a bad token on one connection cannot hold it open by retrying.
func TestFailedAttemptsShareOneDeadline(t *testing.T) {
	s := startLimitedServer(t, 64<<10, 300*time.Millisecond, 4)
	s.Register("app.list", func(Args, Progress) (*Result, error) { r := Res(true, "ok"); return &r, nil })

	conn := dialUnix(t, s)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := conn.Write([]byte(request("wrong", "app.list"))); err != nil {
					return
				}
			}
		}
	}()
	start := time.Now()
	readUntilClosed(t, conn, 3*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("closed only after %s", elapsed)
	}
}

// Once a handler runs the deadline is gone: a handler may stream progress for
// minutes, and the slot is no longer taken.
func TestHandlerOutlivesTheReadTimeout(t *testing.T) {
	s := startLimitedServer(t, 64<<10, 150*time.Millisecond, 1)
	s.Register("app.list", func(_ Args, p Progress) (*Result, error) {
		time.Sleep(400 * time.Millisecond)
		p("build", "running", "still here")
		r := Res(true, "done")
		return &r, nil
	})

	long := dialUnix(t, s)
	long.Write([]byte(request(fixtureKey, "app.list")))

	// The single pending slot is free again while that handler runs.
	time.Sleep(50 * time.Millisecond)
	s.Register("dns.list", func(Args, Progress) (*Result, error) { r := Res(true, "quick"); return &r, nil })
	lines := call(t, "unix", s.SocketPath(), request(fixtureKey, "dns.list"))
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"message":"quick"`) {
		t.Fatalf("second request while a handler ran = %v", lines)
	}

	got := readUntilClosed(t, long, 3*time.Second)
	if !strings.Contains(got, "still here") || !strings.Contains(got, `"message":"done"`) {
		t.Fatalf("long handler answered %q", got)
	}
}

// A listener holds at most MaxPending connections before their handler; the
// next waits for a slot. The TCP listener has its own slots, so containers
// filling the unix socket do not lock the CLI out.
func TestPendingConnectionsAreCappedPerListener(t *testing.T) {
	s := startLimitedServer(t, 64<<10, 5*time.Second, 2)
	s.Register("app.list", func(Args, Progress) (*Result, error) { r := Res(true, "ok"); return &r, nil })

	first := dialUnix(t, s)
	dialUnix(t, s)
	// Let the accept loop take both slots before the third connection.
	time.Sleep(100 * time.Millisecond)

	waiting := dialUnix(t, s)
	waiting.Write([]byte(request(fixtureKey, "app.list")))
	waiting.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _ := waiting.Read(make([]byte, 1)); n != 0 {
		t.Fatal("a connection past the cap was served while both slots were taken")
	}

	tcp := call(t, "tcp", tcpAddr(s), request(fixtureKey, "app.list"))
	if len(tcp) == 0 || !strings.Contains(tcp[len(tcp)-1], `"result":true`) {
		t.Fatalf("TCP while the unix socket was full = %v", tcp)
	}

	first.Close()
	got := readUntilClosed(t, waiting, 3*time.Second)
	if !strings.Contains(got, `"result":true`) {
		t.Fatalf("queued connection answered %q", got)
	}
}
