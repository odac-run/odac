package proxy

import (
	"net/http"
	"testing"
)

func TestClientCannotStripForwardingHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	})

	r := altSvcRequest("/dynamic", false)
	r.RemoteAddr = "203.0.113.9:4321"
	r.Header.Set("Connection", "X-Real-IP, X-Odac-Connection-RemoteAddress, X-Forwarded-Proto, X-Forwarded-For, X-Forwarded-Host")
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	serveAltSvc(p, r)

	h := <-seen
	for name, want := range map[string]string{
		"X-Real-IP":                       "203.0.113.9",
		"X-Odac-Connection-RemoteAddress": "203.0.113.9",
		"X-Forwarded-Proto":               "http",
		"X-Forwarded-Host":                altSvcTestHost,
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := h.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want exactly the client address once", got)
	}
}
