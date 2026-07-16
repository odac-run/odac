package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHintsStore_LearnAndSend(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	host := "example.com"
	urlPath := "/dashboard"

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html; charset=utf-8"},
			"Link":         []string{`</assets/style.css>; rel=preload; as=style`},
		},
	}

	// First observation — not stable yet
	hs.Learn(host, urlPath, resp)

	w := httptest.NewRecorder()
	if hs.Send(w, host, urlPath) {
		t.Error("Should not send hints after only one observation (not stable)")
	}

	// Second observation with same hints — now stable
	hs.Learn(host, urlPath, resp)

	w = httptest.NewRecorder()
	if !hs.Send(w, host, urlPath) {
		t.Fatal("Should send hints after two consistent observations")
	}

	if w.Code != http.StatusEarlyHints {
		t.Errorf("Expected 103, got %d", w.Code)
	}

	links := w.Header().Values("Link")
	if len(links) != 1 || links[0] != `</assets/style.css>; rel=preload; as=style` {
		t.Errorf("Unexpected Link headers: %v", links)
	}
}

func TestHintsStore_MultipleLinks(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link": []string{
				`</style.css>; rel=preload; as=style, </app.js>; rel=preload; as=script`,
				`</font.woff2>; rel=preload; as=font; crossorigin`,
			},
		},
	}

	hs.Learn("example.com", "/", resp)
	hs.Learn("example.com", "/", resp)

	w := httptest.NewRecorder()
	hs.Send(w, "example.com", "/")

	links := w.Header().Values("Link")
	if len(links) != 3 {
		t.Errorf("Expected 3 Link headers, got %d: %v", len(links), links)
	}
}

func TestHintsStore_UnstableHintsNotSent(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp1 := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</v1.css>; rel=preload; as=style`},
		},
	}
	resp2 := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</v2.css>; rel=preload; as=style`},
		},
	}

	hs.Learn("example.com", "/dynamic", resp1)
	hs.Learn("example.com", "/dynamic", resp2) // Different hints — unstable

	w := httptest.NewRecorder()
	if hs.Send(w, "example.com", "/dynamic") {
		t.Error("Should not send unstable hints")
	}
}

func TestHintsStore_RedirectClearsHints(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</style.css>; rel=preload; as=style`},
		},
	}

	// Build up stable hints
	hs.Learn("example.com", "/login", resp)
	hs.Learn("example.com", "/login", resp)

	// Now a redirect happens (user logged in, app redirects)
	redirect := &http.Response{
		StatusCode: 302,
		Header:     http.Header{"Location": []string{"/dashboard"}},
	}
	hs.Learn("example.com", "/login", redirect)

	w := httptest.NewRecorder()
	if hs.Send(w, "example.com", "/login") {
		t.Error("Redirect should clear hints for that URL")
	}
}

func TestHintsStore_IgnoresNonHTML(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Link":         []string{`</style.css>; rel=preload; as=style`},
		},
	}

	hs.Learn("example.com", "/api/data", resp)
	hs.Learn("example.com", "/api/data", resp)

	w := httptest.NewRecorder()
	if hs.Send(w, "example.com", "/api/data") {
		t.Error("Should not learn hints from non-HTML responses")
	}
}

func TestHintsStore_IgnoresNonPreloadLinks(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</next>; rel=next`, `</style.css>; rel=prefetch`},
		},
	}

	hs.Learn("example.com", "/page", resp)
	hs.Learn("example.com", "/page", resp)

	w := httptest.NewRecorder()
	if hs.Send(w, "example.com", "/page") {
		t.Error("Should not store non-preload Link headers")
	}
}

func TestHintsStore_Purge(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</style.css>; rel=preload; as=style`},
		},
	}

	hs.Learn("a.com", "/", resp)
	hs.Learn("a.com", "/", resp)
	hs.Learn("b.com", "/", resp)
	hs.Learn("b.com", "/", resp)

	count := hs.Purge("a.com")
	if count != 1 {
		t.Errorf("Expected 1 purged, got %d", count)
	}

	w := httptest.NewRecorder()
	if hs.Send(w, "a.com", "/") {
		t.Error("a.com hints should be purged")
	}

	w = httptest.NewRecorder()
	if !hs.Send(w, "b.com", "/") {
		t.Error("b.com hints should still exist")
	}
}

func TestHintsStore_NoHintsRemovesEntry(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</style.css>; rel=preload; as=style`},
		},
	}

	hs.Learn("example.com", "/page", resp)
	hs.Learn("example.com", "/page", resp)

	// Backend stops sending Link headers
	noLinks := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
	}
	hs.Learn("example.com", "/page", noLinks)

	w := httptest.NewRecorder()
	if hs.Send(w, "example.com", "/page") {
		t.Error("Should remove hints when backend stops sending them")
	}
}

func TestExtractPreloadLinks(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   int
	}{
		{
			"single preload",
			http.Header{"Link": []string{`</style.css>; rel=preload; as=style`}},
			1,
		},
		{
			"multiple comma-separated",
			http.Header{"Link": []string{`</a.css>; rel=preload; as=style, </b.js>; rel=preload; as=script`}},
			2,
		},
		{
			"quoted rel",
			http.Header{"Link": []string{`</font.woff2>; rel="preload"; as=font`}},
			1,
		},
		{
			"mixed preload and other",
			http.Header{"Link": []string{`</style.css>; rel=preload; as=style, </next>; rel=next`}},
			1,
		},
		{
			"no preload",
			http.Header{"Link": []string{`</next>; rel=next`}},
			0,
		},
		{
			"empty",
			http.Header{},
			0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPreloadLinks(tt.header)
			if len(got) != tt.want {
				t.Errorf("extractPreloadLinks() = %d links, want %d: %v", len(got), tt.want, got)
			}
		})
	}
}
