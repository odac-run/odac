package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"testing"
)

func anonymousPageRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
}

func TestHintsStore_LearnAndSend(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	host := "example.com"
	urlPath := "/dashboard"

	resp := &http.Response{
		StatusCode: 200,
		Request:    anonymousPageRequest(),
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

	links := w.Result().Header.Values("Link")
	if len(links) != 1 || links[0] != `</assets/style.css>; rel=preload; as=style` {
		t.Errorf("Unexpected Link headers: %v", links)
	}
}

func TestHintsStore_MultipleLinks(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp := &http.Response{
		StatusCode: 200,
		Request:    anonymousPageRequest(),
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

	links := w.Result().Header.Values("Link")
	if len(links) != 3 {
		t.Errorf("Expected 3 Link headers, got %d: %v", len(links), links)
	}
}

func TestHintsStore_UnstableHintsNotSent(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()

	resp1 := &http.Response{
		StatusCode: 200,
		Request:    anonymousPageRequest(),
		Header: http.Header{
			"Content-Type": []string{"text/html"},
			"Link":         []string{`</v1.css>; rel=preload; as=style`},
		},
	}
	resp2 := &http.Response{
		StatusCode: 200,
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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
		Request:    anonymousPageRequest(),
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

const privateHint = "</private/acme/bundle.js?token=secret>; rel=preload; as=script"

func hintResponse(status int, req *http.Request, headers http.Header) *http.Response {
	h := http.Header{"Content-Type": {"text/html"}, "Link": {privateHint}}
	for k, v := range headers {
		h[k] = v
	}
	return &http.Response{StatusCode: status, Request: req, Header: h}
}

func requestWith(method, target string, headers http.Header) *http.Request {
	r := httptest.NewRequest(method, "http://example.com"+target, nil)
	for k, v := range headers {
		r.Header[k] = v
	}
	return r
}

func TestHintsNeverLearnedFromPersonalTraffic(t *testing.T) {
	anon := anonymousPageRequest()
	tests := []struct {
		name string
		resp *http.Response
	}{
		{"no request", hintResponse(200, nil, nil)},
		{"cookie", hintResponse(200, requestWith(http.MethodGet, "/", http.Header{"Cookie": {"sid=1"}}), nil)},
		{"authorization", hintResponse(200, requestWith(http.MethodGet, "/", http.Header{"Authorization": {"Bearer x"}}), nil)},
		{"post", hintResponse(200, requestWith(http.MethodPost, "/", nil), nil)},
		{"head", hintResponse(200, requestWith(http.MethodHead, "/", nil), nil)},
		{"capability query", hintResponse(200, requestWith(http.MethodGet, "/?token=abc", nil), nil)},
		{"private", hintResponse(200, anon, http.Header{"Cache-Control": {"private"}})},
		{"no-store", hintResponse(200, anon, http.Header{"Cache-Control": {"no-store"}})},
		{"mixed case private", hintResponse(200, anon, http.Header{"Cache-Control": {"Private, max-age=0"}})},
		{"second cache-control line", hintResponse(200, anon, http.Header{"Cache-Control": {"max-age=0", "private"}})},
		{"set-cookie", hintResponse(200, anon, http.Header{"Set-Cookie": {"sid=1"}})},
		{"vary cookie", hintResponse(200, anon, http.Header{"Vary": {"Accept-Encoding, Cookie"}})},
		{"second vary line", hintResponse(200, anon, http.Header{"Vary": {"Accept-Encoding", "Authorization"}})},
		{"vary star", hintResponse(200, anon, http.Header{"Vary": {"*"}})},
		{"unauthorized", hintResponse(401, anon, nil)},
		{"server error", hintResponse(500, anon, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := NewHintsStore()
			defer hs.Stop()
			hs.Learn("example.com", "/", tt.resp)
			hs.Learn("example.com", "/", tt.resp)
			if _, stored := hs.entries.Load("example.com/"); stored {
				t.Fatal("learned a hint that must stay out of the store")
			}
		})
	}
}

func TestHintsWithdrawal(t *testing.T) {
	anon := anonymousPageRequest()
	json := http.Header{"Content-Type": {"application/json"}}
	cookied := requestWith(http.MethodGet, "/", http.Header{"Cookie": {"sid=1"}})
	tests := []struct {
		name string
		resp *http.Response
		kept bool
	}{
		{"anonymous json 401", hintResponse(401, anon, json), false},
		{"anonymous html 401", hintResponse(401, anon, nil), false},
		{"anonymous redirect", hintResponse(302, anon, nil), false},
		{"anonymous server error", hintResponse(500, anon, nil), false},
		{"anonymous private page", hintResponse(200, anon, http.Header{"Cache-Control": {"private"}}), false},
		{"anonymous private json", hintResponse(200, anon, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"private"}}), false},
		{"anonymous set-cookie", hintResponse(200, anon, http.Header{"Set-Cookie": {"sid=1"}}), false},
		{"not modified", hintResponse(304, anon, nil), true},
		{"websocket upgrade", hintResponse(101, anon, nil), true},
		{"public json", hintResponse(200, anon, json), true},
		{"signed in user sees a private page", hintResponse(200, cookied, http.Header{"Cache-Control": {"private"}}), true},
		{"signed in user sees an error", hintResponse(500, cookied, nil), true},
		{"post fails", hintResponse(500, requestWith(http.MethodPost, "/", nil), nil), true},
		{"query fails", hintResponse(404, requestWith(http.MethodGet, "/?a=1", nil), nil), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := NewHintsStore()
			defer hs.Stop()
			public := hintResponse(200, anon, nil)
			hs.Learn("example.com", "/", public)
			hs.Learn("example.com", "/", public)
			hs.Learn("example.com", "/", tt.resp)
			if got := hs.Send(httptest.NewRecorder(), "example.com", "/"); got != tt.kept {
				t.Fatalf("hint served = %v, want %v", got, tt.kept)
			}
		})
	}
}

func TestPrivateHintsNeverReachAnotherClient(t *testing.T) {
	anonymousAnswers := map[string]func(http.ResponseWriter){
		"html 401": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusUnauthorized)
		},
		"json 401": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
		},
		"login redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
		},
	}
	for name, answer := range anonymousAnswers {
		t.Run(name, func(t *testing.T) {
			_, front := newEdgeFront(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Cookie") == "" {
					answer(w)
					return
				}
				w.Header().Set("Link", privateHint)
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte("private"))
			})
			front.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}

			for range 3 {
				req, _ := http.NewRequest(http.MethodGet, front.URL+"/dashboard", nil)
				req.Header.Set("Cookie", "sid=alice")
				edgeDo(t, front, req)
			}

			var interim []string
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
					interim = append(interim, fmt.Sprint(code, h["Link"]))
					return nil
				},
			})
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/dashboard", nil)
			resp := edgeDo(t, front, req)
			if len(interim) != 0 {
				t.Errorf("anonymous client received %v", interim)
			}
			if got := resp.Header.Values("Link"); len(got) != 0 {
				t.Errorf("final response carries Link %v", got)
			}
		})
	}
}

func TestLearnedHintsLeaveTheFinalResponseToTheApp(t *testing.T) {
	const hint = "</a.css>; rel=preload"
	_, front := newEdgeFront(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/page" {
			w.Header().Set("Link", hint)
		}
		w.Write([]byte("hello"))
	})

	var last *http.Response
	for range 3 {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/page", nil)
		last = edgeDo(t, front, req)
	}
	if got := last.Header.Values("Link"); len(got) != 1 || got[0] != hint {
		t.Fatalf("final Link = %q, want only the app's own", got)
	}
}

func TestHintsConcurrentLearnAndSend(t *testing.T) {
	hs := NewHintsStore()
	defer hs.Stop()
	responses := []*http.Response{
		hintResponse(200, anonymousPageRequest(), http.Header{"Link": {"</a.css>; rel=preload"}}),
		hintResponse(200, anonymousPageRequest(), http.Header{"Link": {"</b.css>; rel=preload"}}),
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 2000 {
			hs.Learn("example.com", "/", responses[i%2])
			hs.Learn("example.com", "/", responses[i%2])
		}
	})
	wg.Go(func() {
		for range 2000 {
			hs.Send(httptest.NewRecorder(), "example.com", "/")
		}
	})
	wg.Wait()
}
