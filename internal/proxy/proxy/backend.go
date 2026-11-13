package proxy

import (
	"context"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"odac/internal/proxy/config"
)

const socketFallbackLogEvery = time.Minute

var tcpDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// backendAddr is the TCP address a record's traffic goes to. It is also the key
// that routes a request to a socket backend, so every path that forwards to an
// app must derive its URL host here.
func backendAddr(w config.Website) string {
	host := "127.0.0.1"
	if w.ContainerIP != "" {
		host = w.ContainerIP
	} else if w.Container != "" {
		host = w.Container
	}
	return net.JoinHostPort(host, strconv.Itoa(w.Port))
}

func newUpstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           tcpDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// socketBackend is one app socket with its own connection pool, so idle
// connections of two backends never mix.
type socketBackend struct {
	path         string
	transport    *http.Transport
	lastFallback atomic.Int64
}

// socketBackends maps a record's TCP address to its socket. Paths outside root
// are refused, so a config push cannot point a backend at docker.sock.
type socketBackends struct {
	mu     sync.Mutex
	root   string
	byAddr atomic.Pointer[map[string]*socketBackend]
}

func (s *socketBackends) setRoot(root string) {
	if root != "" && !pinnedDialSupported {
		log.Printf("[Proxy] Backend sockets are not supported on this platform, using TCP")
		root = ""
	}
	if root != "" {
		root = filepath.Clean(root)
	}
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
}

func (s *socketBackends) lookup(addr string) *socketBackend {
	m := s.byAddr.Load()
	if m == nil {
		return nil
	}
	return (*m)[addr]
}

func (s *socketBackends) update(domains map[string]config.Website) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var old map[string]*socketBackend
	if m := s.byAddr.Load(); m != nil {
		old = *m
	}
	next := make(map[string]*socketBackend)
	conflicted := make(map[string]bool)
	for name, w := range domains {
		if w.Socket == "" || s.root == "" {
			continue
		}
		if !socketUnder(s.root, w.Socket) {
			log.Printf("[Proxy] Refusing socket %q for %q: not under %s", w.Socket, name, s.root)
			continue
		}
		addr := backendAddr(w)
		if conflicted[addr] {
			continue
		}
		if b, ok := next[addr]; ok {
			if b.path != w.Socket {
				log.Printf("[Proxy] Records for %s name different sockets, using TCP", addr)
				delete(next, addr)
				conflicted[addr] = true
			}
			continue
		}
		if b, ok := old[addr]; ok && b.path == w.Socket {
			next[addr] = b
			continue
		}
		next[addr] = s.newBackend(w.Socket)
	}
	s.byAddr.Store(&next)

	for addr, b := range old {
		if next[addr] != b {
			b.transport.CloseIdleConnections()
		}
	}
}

func (s *socketBackends) newBackend(path string) *socketBackend {
	b := &socketBackend{path: path}
	root := s.root
	rel, _ := filepath.Rel(root, path)
	dir, name := filepath.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	t := newUpstreamTransport()
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialPinned(ctx, root, dir, name)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		b.noteFallback(err)
		return tcpDialer.DialContext(ctx, network, addr)
	}
	b.transport = t
	return b
}

func (b *socketBackend) noteFallback(err error) {
	now := time.Now().UnixNano()
	last := b.lastFallback.Load()
	if now-last < int64(socketFallbackLogEvery) || !b.lastFallback.CompareAndSwap(last, now) {
		return
	}
	log.Printf("[Proxy] Socket %s unavailable (%v), using TCP", b.path, err)
}

// socketUnder accepts exactly <root>/<dir>/<name>: one directory per container
// and the socket inside it, which is the shape dialPinned walks.
func socketUnder(root, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}

// backendRouter sends a request to its socket backend when the record has one,
// and to the shared TCP transport otherwise.
type backendRouter struct {
	sockets *socketBackends
	tcp     http.RoundTripper
}

func (r backendRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	if b := r.sockets.lookup(req.URL.Host); b != nil {
		return b.transport.RoundTrip(req)
	}
	return r.tcp.RoundTrip(req)
}
