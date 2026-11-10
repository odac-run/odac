package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestAssetTypeMustMatchExtension(t *testing.T) {
	for _, tc := range []struct {
		path, ct string
		want     bool
	}{
		{"/app.js", "application/javascript; charset=utf-8", true},
		{"/app.mjs", "text/javascript", true},
		{"/site.css", "text/css", true},
		{"/logo.png", "image/png", true},
		{"/logo.svg", "image/svg+xml", true},
		{"/font.woff2", "font/woff2", true},
		{"/font.ttf", "application/octet-stream", true},
		{"/app.js.map", "application/json", true},
		{"/api/me/x.js", "application/json", false},
		{"/api/me/x.css", "text/html", false},
		{"/avatar.png", "image/svg+xml", false},
		{"/x.js", "image/png", false},
	} {
		if got := contentTypeMatchesExtension(tc.path, tc.ct); got != tc.want {
			t.Errorf("%s as %s: got %v, want %v", tc.path, tc.ct, got, tc.want)
		}
	}
}

func TestCookiedResponseIsSharedOnlyWhenPublic(t *testing.T) {
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		if strings.HasPrefix(r.URL.Path, "/public") {
			w.Header().Set("Cache-Control", "max-age=60, Public")
		}
		w.Write([]byte("personal for " + r.Header.Get("Cookie")))
	})

	for path, want := range map[string]bool{"/me.js": false, "/public.js": true} {
		for range 2 {
			r := altSvcRequest(path, false)
			r.Header.Set("Cookie", "session=victim")
			serveAltSvc(p, r)
		}
		rec := serveAltSvc(p, altSvcRequest(path, false))
		if got := rec.Header().Get("X-Odac-Cache") == "HIT"; got != want {
			t.Errorf("%s: anonymous request hit=%v, want %v (body %q)", path, got, want, rec.Body.String())
		}
	}
}
