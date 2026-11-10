package netutil

import (
	"net/http"
	"testing"
)

// TestControlServerTimeouts pins the server half of H2: bare http.Serve kept
// idle and stalled connections open for the life of the daemon.
func TestControlServerTimeouts(t *testing.T) {
	srv := controlServer(http.NotFoundHandler())
	if srv.IdleTimeout <= 0 || srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("control server has no idle (%v) or header (%v) timeout", srv.IdleTimeout, srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Fatalf("a whole-request timeout would cut a long /send short (read %v, write %v)", srv.ReadTimeout, srv.WriteTimeout)
	}
}
