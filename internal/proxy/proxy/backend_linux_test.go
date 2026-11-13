//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"odac/internal/proxy/config"
)

// socketRoot is short on purpose: a unix socket path is limited to 108 bytes.
func socketRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "uds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func serveUnix(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func TestDialPinnedRefusesAnythingButASocket(t *testing.T) {
	root := socketRoot(t)
	serveUnix(t, filepath.Join(root, "app", "http.sock"), "app")
	secret := filepath.Join(root, "secret", "s.sock")
	serveUnix(t, secret, "secret")

	dir := filepath.Join(root, "evil")
	os.MkdirAll(dir, 0o755)
	os.Symlink(secret, filepath.Join(dir, "link.sock"))
	syscall.Mkfifo(filepath.Join(dir, "fifo.sock"), 0o644)
	os.WriteFile(filepath.Join(dir, "file.sock"), nil, 0o644)
	os.Symlink(filepath.Join(root, "secret"), filepath.Join(root, "linkdir"))

	conn, err := dialPinned(context.Background(), root, "app", "http.sock")
	if err != nil {
		t.Fatalf("real socket: %v", err)
	}
	conn.Close()

	for _, c := range []struct{ dir, name string }{
		{"evil", "link.sock"},
		{"evil", "fifo.sock"},
		{"evil", "file.sock"},
		{"linkdir", "s.sock"},
		{"evil", "missing.sock"},
	} {
		if conn, err := dialPinned(context.Background(), root, c.dir, c.name); err == nil {
			conn.Close()
			t.Errorf("%s/%s: dial succeeded, want refusal", c.dir, c.name)
		}
	}
	if _, err := dialPinned(context.Background(), root, "evil", "file.sock"); !errors.Is(err, errNotSocket) {
		t.Errorf("regular file: err = %v, want errNotSocket", err)
	}
}

func newSocketProxy(t *testing.T, root string, sites map[string]config.Website) *Proxy {
	t.Helper()
	p := NewProxy()
	t.Cleanup(p.Cache().Stop)
	p.SetSocketRoot(root)
	p.UpdateConfig(sites, nil, nil, nil)
	return p
}

func tcpBackend(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "tcp")
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func fetch(t *testing.T, p *Proxy, host string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/dynamic", nil))
	return rec.Body.String()
}

func TestProxyRoutesOverSocketAndFallsBackToTCP(t *testing.T) {
	root := socketRoot(t)
	port := tcpBackend(t)
	sock := filepath.Join(root, "app", "http.sock")
	serveUnix(t, sock, "uds")
	secret := filepath.Join(root, "secret", "s.sock")
	serveUnix(t, secret, "secret")
	os.MkdirAll(filepath.Join(root, "evil"), 0o755)
	os.Symlink(secret, filepath.Join(root, "evil", "http.sock"))

	site := func(socket string) config.Website {
		return config.Website{ContainerIP: "127.0.0.1", Port: port, Socket: socket}
	}
	cases := []struct {
		name   string
		socket string
		want   string
	}{
		{"socket", sock, "uds"},
		{"missing socket", filepath.Join(root, "gone", "http.sock"), "tcp"},
		{"symlink", filepath.Join(root, "evil", "http.sock"), "tcp"},
		{"outside root", "/var/run/docker.sock", "tcp"},
		{"no socket", "", "tcp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newSocketProxy(t, root, map[string]config.Website{"a.test": site(c.socket)})
			if got := fetch(t, p, "a.test"); got != c.want {
				t.Fatalf("body = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("no socket root", func(t *testing.T) {
		p := newSocketProxy(t, "", map[string]config.Website{"a.test": site(sock)})
		if got := fetch(t, p, "a.test"); got != "tcp" {
			t.Fatalf("body = %q, want tcp", got)
		}
	})

	t.Run("revalidation uses the same route", func(t *testing.T) {
		p := newSocketProxy(t, root, map[string]config.Website{"a.test": site(sock)})
		resp, err := p.revalidator.Get("http://" + backendAddr(site(sock)) + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if b, _ := io.ReadAll(resp.Body); string(b) != "uds" {
			t.Fatalf("body = %q, want uds", b)
		}
	})
}

func TestSocketBackendPoolsSurviveUnrelatedPushes(t *testing.T) {
	root := socketRoot(t)
	sock := filepath.Join(root, "app", "http.sock")
	other := filepath.Join(root, "app2", "http.sock")
	site := config.Website{ContainerIP: "10.0.0.2", Port: 3000, Socket: sock}
	addr := backendAddr(site)

	var s socketBackends
	s.setRoot(root)
	s.update(map[string]config.Website{"a.test": site, "www.a.test": site})
	first := s.lookup(addr)
	if first == nil {
		t.Fatal("no backend registered")
	}
	s.update(map[string]config.Website{"a.test": site})
	if s.lookup(addr) != first {
		t.Fatal("an unchanged socket got a new pool")
	}

	moved := site
	moved.Socket = other
	s.update(map[string]config.Website{"a.test": moved})
	if b := s.lookup(addr); b == first || b == nil || b.path != other {
		t.Fatal("a changed socket kept the old pool")
	}

	s.update(map[string]config.Website{"a.test": site, "b.test": moved})
	if s.lookup(addr) != nil {
		t.Fatal("one address naming two sockets must fall back to TCP")
	}
}

func TestWebSocketOverSocketBackend(t *testing.T) {
	root := socketRoot(t)
	sock := filepath.Join(root, "app", "http.sock")
	os.MkdirAll(filepath.Dir(sock), 0o755)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, msg, err := c.ReadMessage()
		if err == nil {
			c.WriteMessage(websocket.TextMessage, append([]byte("uds:"), msg...))
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// Port 1 has no listener: a TCP fallback would fail the dial.
	p := newSocketProxy(t, root, map[string]config.Website{"a.test": {ContainerIP: "127.0.0.1", Port: 1, Socket: sock}})
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second, NetDial: func(string, string) (net.Conn, error) {
		return net.Dial("tcp", front.Listener.Addr().String())
	}}
	c, _, err := d.Dial("ws://a.test/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := c.ReadMessage(); err != nil || string(msg) != "uds:ping" {
		t.Fatalf("message = %q, err %v", msg, err)
	}
}
