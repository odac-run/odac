package proxy

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

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
