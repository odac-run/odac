package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const udpBindRetryInterval = 100 * time.Millisecond

// HTTP3Service owns the QUIC socket for the lifetime of the server. The port is not
// shared during an update, unlike the TCP listeners: SO_REUSEPORT hashes each client
// to a socket, and QUIC connection state lives in a single process, so a shared port
// sends about half of the old process's connections to a process that cannot read them.
// A replacement waits for the port instead and clients use TCP in the meantime.
type HTTP3Service struct {
	server   *http3.Server
	resetKey *quic.StatelessResetKey
	cancel   context.CancelFunc
	done     chan struct{}
	conn     net.PacketConn // written before done closes
}

// NewHTTP3Service wraps srv, which binds srv.Addr once started.
func NewHTTP3Service(srv *http3.Server, resetKey *quic.StatelessResetKey) *HTTP3Service {
	return &HTTP3Service{server: srv, resetKey: resetKey}
}

// Start returns at once because the port may stay busy for as long as the previous
// process drains, and HTTP/3 must not hold up the TCP listeners. A bind failure is
// logged and not fatal, since clients can still reach the proxy over TCP.
func (s *HTTP3Service) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		conn, err := bindUDP(ctx, s.server.Addr)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("HTTP/3 UDP listener failed: %v", err)
				log.Println("Hint: If you see 'message too long' or performance issues, run: sysctl -w net.core.rmem_max=2500000")
			}
			return
		}
		s.conn = conn
		log.Printf("Starting HTTP/3 server on %s (UDP)", s.server.Addr)
		if err := ServeHTTP3(s.server, conn, s.resetKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP/3 server failed: %v", err)
		}
	}()
}

// Stop drains until ctx ends and only then releases the port, because the connections
// being drained still receive their packets on it.
func (s *HTTP3Service) Stop(ctx context.Context) error {
	s.cancel()
	err := s.server.Shutdown(ctx)
	<-s.done
	if s.conn != nil {
		if closeErr := s.conn.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// bindUDP retries only while the port is taken: giving up there would leave HTTP/3
// down for the lifetime of the process. Any other error will not fix itself.
func bindUDP(ctx context.Context, addr string) (net.PacketConn, error) {
	waiting := false
	for {
		conn, err := net.ListenPacket("udp", addr)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
		if !waiting {
			waiting = true
			log.Printf("HTTP/3 waiting for UDP %s to be released: %v", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(udpBindRetryInterval):
		}
	}
}

// ServeHTTP3 serves srv on conn with a stateless reset key, which http3.Server.Serve
// has no way to carry. ServeListener leaves the listener to its owner, so it is closed
// here when Shutdown ends the accept loop. The transport stays open: closing it would
// drop the connections Shutdown is still draining.
func ServeHTTP3(srv *http3.Server, conn net.PacketConn, resetKey *quic.StatelessResetKey) error {
	tr := &quic.Transport{Conn: conn, StatelessResetKey: resetKey}
	ln, err := tr.ListenEarly(http3.ConfigureTLSConfig(srv.TLSConfig), srv.QUICConfig)
	if err != nil {
		return err
	}
	defer ln.Close()
	return srv.ServeListener(ln)
}

// LoadStatelessResetKey returns the key stored at path, creating it when missing or
// corrupt. It must survive restarts: a client only accepts a reset derived from the
// key that issued its connection, otherwise it waits out the 30s idle timeout.
func LoadStatelessResetKey(path string) (*quic.StatelessResetKey, error) {
	var key quic.StatelessResetKey
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil && len(data) == len(key) {
		copy(key[:], data)
		return &key, nil
	}

	rand.Read(key[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key[:], 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return &key, nil
}

// NewHTTP3Server builds the QUIC listener's server. It lives here rather than in
// the binary because the proxy's cache correctness depends on how it is configured.
func NewHTTP3Server(addr string, handler http.Handler, tlsConfig *tls.Config) *http3.Server {
	return &http3.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: tlsConfig,
		QUICConfig: &quic.Config{
			Allow0RTT: true,
		},
		ConnContext: markNetHTTPServer(&http.Server{Addr: addr}),
	}
}

// markNetHTTPServer exists for httputil.ReverseProxy, which only aborts a failed
// body copy when http.ServerContextKey is set, and quic-go sets its own key. Without
// it a cut-off response ends as a clean 200 and ServeHTTP stores it in the cache.
func markNetHTTPServer(srv *http.Server) func(context.Context, *quic.Conn) context.Context {
	return func(ctx context.Context, _ *quic.Conn) context.Context {
		return context.WithValue(ctx, http.ServerContextKey, srv)
	}
}
