package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/jscanon"
)

// fakeHubHTTP records requests and plays canned responses for the Hub HTTP
// API (auth/app).
type fakeHubHTTP struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	status int
	body   string
}

type recordedRequest struct {
	path   string
	header http.Header
	body   []byte
}

func newFakeHubHTTP(t *testing.T, status int, body string) *fakeHubHTTP {
	f := &fakeHubHTTP{status: status, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{path: r.URL.Path, header: r.Header.Clone(), body: raw})
		status, body := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHubHTTP) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no requests received")
	}
	return f.requests[len(f.requests)-1]
}

func newHTTPFixture(t *testing.T, status int, body string) (*hubFixture, *fakeHubHTTP) {
	backend := newFakeHubHTTP(t, status, body)
	fx := newHubFixture(t)
	fx.h.baseURL = backend.srv.URL
	return fx, backend
}

func TestCallSuccess(t *testing.T) {
	fx, backend := newHTTPFixture(t, 200,
		`{"result":{"success":true},"data":{"response":"data"}}`)

	got, terr := fx.h.call("test-action", map[string]any{"param": "value"})
	if terr != nil {
		t.Fatalf("call failed: %v", terr)
	}
	if !reflect.DeepEqual(got, map[string]any{"response": "data"}) {
		t.Fatalf("call data = %v", got)
	}
	req := backend.last(t)
	if req.path != "/test-action" {
		t.Errorf("path = %s", req.path)
	}
	if auth := req.header.Get("Authorization"); auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", auth)
	}
	if string(req.body) != `{"param":"value"}` {
		t.Errorf("body = %s", req.body)
	}
}

func TestCallEnvelopeErrors(t *testing.T) {
	// Missing result → Invalid response format.
	fx, _ := newHTTPFixture(t, 200, `{"data":{}}`)
	_, terr := fx.h.call("x", map[string]any{})
	if terr == nil || !terr.isErrorObject || terr.message != "Invalid response format" {
		t.Fatalf("missing-result thrown = %+v", terr)
	}

	// success:false → throw result.message.
	fx2, _ := newHTTPFixture(t, 200, `{"result":{"success":false,"message":"API error"}}`)
	_, terr = fx2.h.call("x", map[string]any{})
	if terr == nil || terr.Error() != "API error" {
		t.Fatalf("api-error thrown = %+v", terr)
	}

	// authenticated:false quirk → the result object comes back as data.
	fx3, _ := newHTTPFixture(t, 200,
		`{"result":{"success":false,"authenticated":false,"message":"nope"}}`)
	got, terr := fx3.h.call("x", map[string]any{})
	if terr != nil {
		t.Fatalf("authenticated:false threw: %v", terr)
	}
	res, _ := got.(map[string]any)
	if res["authenticated"] != false || res["message"] != "nope" {
		t.Fatalf("authenticated:false result = %v", got)
	}
}

func TestCallHTTPErrorThrowsResponseData(t *testing.T) {
	// Non-2xx: the thrown value is the parsed response body.
	fx, _ := newHTTPFixture(t, 500, `{"oops":true}`)
	_, terr := fx.h.call("x", map[string]any{})
	if terr == nil || terr.isErrorObject {
		t.Fatalf("expected value-throw, got %+v", terr)
	}
	if val, _ := terr.value.(map[string]any); val["oops"] != true {
		t.Fatalf("thrown value = %v", terr.value)
	}
}

func TestCallConnectionRefused(t *testing.T) {
	fx := newHubFixture(t)
	fx.h.baseURL = "http://127.0.0.1:9"

	_, terr := fx.h.call("auth", map[string]any{})
	if terr == nil || !terr.isErrorObject {
		t.Fatalf("thrown = %+v", terr)
	}
	if terr.message != "Connection refused at http://127.0.0.1:9/auth" {
		t.Fatalf("message = %q", terr.message)
	}
}

func TestCallRetriesOnDNSFailure(t *testing.T) {
	fx := newHubFixture(t)
	fx.h.baseURL = "http://does-not-exist-odac.invalid"
	var slept []time.Duration
	fx.h.sleep = func(d time.Duration) { slept = append(slept, d) }

	start := time.Now()
	_, terr := fx.h.call("x", map[string]any{})
	if terr == nil {
		t.Fatal("call unexpectedly succeeded")
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("retries slept for real")
	}
	// Linear backoff: 1s, 2s (third attempt fails through).
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Fatalf("backoff = %v", slept)
	}
}

func TestXSignatureHeader(t *testing.T) {
	fx, backend := newHTTPFixture(t, 200, `{"result":{"success":true},"data":{}}`)

	// No timestamp → no signature.
	fx.h.call("app", map[string]any{"name": "web"})
	if sig := backend.last(t).header.Get("X-Signature"); sig != "" {
		t.Errorf("unexpected X-Signature: %s", sig)
	}

	// Timestamped non-auth payload → HMAC over the exact body bytes.
	fx.h.call("report", map[string]any{"timestamp": float64(1752300000), "x": 1})
	req := backend.last(t)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(req.body)
	if sig := req.header.Get("X-Signature"); sig != hex.EncodeToString(mac.Sum(nil)) {
		t.Errorf("X-Signature mismatch: %s (body %s)", sig, req.body)
	}

	// auth action never signs, even with a timestamp.
	fx.h.call("auth", map[string]any{"timestamp": float64(1752300000)})
	if sig := backend.last(t).header.Get("X-Signature"); sig != "" {
		t.Errorf("auth call signed: %s", sig)
	}
}

func TestAuthSuccess(t *testing.T) {
	fx, backend := newHTTPFixture(t, 200,
		`{"result":{"success":true},"data":{"token":"new-token","secret":"new-secret"}}`)

	res := fx.h.Auth("valid-code-123")
	if !res.Status || res.Message != "Authentication successful" {
		t.Fatalf("auth result = %+v", res)
	}
	hubCfg := fx.cfg.Map("hub")
	if hubCfg["token"] != "new-token" || hubCfg["secret"] != "new-secret" {
		t.Fatalf("config.hub = %v", hubCfg)
	}

	// The body is {code, ...System.info()} in that insertion order.
	var body map[string]any
	req := backend.last(t)
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "valid-code-123" || body["arch"] != "arm64" || body["version"] != "1.10.1" {
		t.Fatalf("auth body = %s", req.body)
	}
	if string(req.body[:8]) != `{"code":` {
		t.Errorf("code is not the first body field: %s", req.body[:20])
	}
}

func TestAuthFailureShapes(t *testing.T) {
	nodeErrorShape := func(t *testing.T, res api.Result) {
		t.Helper()
		// Node feeds the thrown Error INSTANCE into Api.result: it
		// stringifies to {} → {result:false, message:null, data:{}}.
		if res.Status || !res.NullMsg || !res.HasData {
			t.Fatalf("result shape = %+v", res)
		}
		if m, _ := res.Data.(map[string]any); m == nil || len(m) != 0 {
			t.Fatalf("data = %#v", res.Data)
		}
	}

	// Invalid schema (missing secret).
	fx, _ := newHTTPFixture(t, 200, `{"result":{"success":true},"data":{"token":"only"}}`)
	nodeErrorShape(t, fx.h.Auth("code"))
	if fx.cfg.Map("hub")["token"] != testToken {
		t.Fatal("config.hub overwritten on failed auth")
	}

	// authenticated:false envelope → same schema-failure path.
	fx2, _ := newHTTPFixture(t, 200, `{"result":{"success":false,"authenticated":false}}`)
	nodeErrorShape(t, fx2.h.Auth("code"))

	// Network failure (Error instance).
	fx3 := newHubFixture(t)
	fx3.h.baseURL = "http://127.0.0.1:9"
	nodeErrorShape(t, fx3.h.Auth("code"))

	// Non-2xx with a STRING body: the thrown value is that string and it
	// lands in message.
	fx4, _ := newHTTPFixture(t, 503, `busy`)
	fx4.h.postJSON = func(url string, body []byte, headers map[string]string) (*httpResponse, error) {
		return &httpResponse{status: 503, data: "busy"}, nil
	}
	res := fx4.h.Auth("code")
	if res.Status || res.Message != "busy" {
		t.Fatalf("string-throw result = %+v", res)
	}

	// Non-2xx with a null body: `error || fallback` picks the fallback.
	fx5, _ := newHTTPFixture(t, 500, ``)
	res = fx5.h.Auth("code")
	if res.Status || res.Message != "Authentication failed" {
		t.Fatalf("null-throw result = %+v", res)
	}
}

func TestGetApp(t *testing.T) {
	fx, backend := newHTTPFixture(t, 200,
		`{"result":{"success":true},"data":{"name":"wordpress","apps":{}}}`)

	recipe, err := fx.h.GetApp("wordpress")
	if err != nil {
		t.Fatal(err)
	}
	if recipe["name"] != "wordpress" {
		t.Fatalf("recipe = %v", recipe)
	}
	if string(backend.last(t).body) != `{"name":"wordpress"}` {
		t.Errorf("body = %s", backend.last(t).body)
	}

	fx2, _ := newHTTPFixture(t, 200, `{"result":{"success":false,"message":"not found"}}`)
	if _, err := fx2.h.GetApp("ghost"); err == nil || err.Error() != "not found" {
		t.Fatalf("GetApp error = %v", err)
	}
}

// TestAuthBodyIsCanonical: the auth body must be jscanon output (the same
// bytes an X-Signature would sign).
func TestAuthBodyIsCanonical(t *testing.T) {
	fx, backend := newHTTPFixture(t, 200,
		`{"result":{"success":true},"data":{"token":"t","secret":"s"}}`)
	fx.h.Auth("c")
	body := backend.last(t).body
	canon, err := jscanon.Canon(body)
	if err != nil || string(canon) != string(body) {
		t.Fatalf("auth body not canonical: %s (%v)", body, err)
	}
}
