package proxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

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
