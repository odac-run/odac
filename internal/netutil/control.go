package netutil

import (
	"net"
	"net/http"
	"time"
)

const (
	controlReadHeaderTimeout = 10 * time.Second
	controlIdleTimeout       = 60 * time.Second
	// controlWriteTimeout is a backstop well past odac-server's 30 second
	// call timeout: an answer that takes longer has no reader left.
	controlWriteTimeout = 2 * time.Minute
)

// ServeControl serves a data-plane binary's control API on l. Bare
// http.Serve has no idle timeout, so every connection a client parked stayed
// open for the life of the daemon.
func ServeControl(l net.Listener, h http.Handler) error {
	return controlServer(h).Serve(l)
}

func controlServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: controlReadHeaderTimeout,
		IdleTimeout:       controlIdleTimeout,
		WriteTimeout:      controlWriteTimeout,
	}
}
