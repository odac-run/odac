package netutil

import (
	"net"
	"net/http"
	"time"
)

const (
	controlReadHeaderTimeout = 10 * time.Second
	controlIdleTimeout       = 60 * time.Second
)

// ServeControl serves a data-plane binary's control API on l. Bare
// http.Serve has no idle timeout, so every connection a client parked stayed
// open for the life of the daemon.
func ServeControl(l net.Listener, h http.Handler) error {
	return controlServer(h).Serve(l)
}

// controlServer has no write timeout on purpose: odac-mail's /send answers
// only after SMTP delivery finishes.
func controlServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: controlReadHeaderTimeout,
		IdleTimeout:       controlIdleTimeout,
	}
}
