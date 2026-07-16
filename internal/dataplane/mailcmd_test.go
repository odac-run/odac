package dataplane

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMailModule fakes the Go mail binary's HTTP API (unix socket): the
// account endpoints, /send and /config, capturing every request.
type fakeMailModule struct {
	sock string

	mu       sync.Mutex
	requests []capturedReq
	respond  map[string]map[string]any // "<METHOD> <path>" → envelope override
	configs  chan []byte
}

type capturedReq struct {
	method, path string
	body         map[string]any
}

func newFakeMailModule(t *testing.T) *fakeMailModule {
	t.Helper()
	dir, err := os.MkdirTemp("", "odacmm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	f := &fakeMailModule{
		sock:    filepath.Join(dir, "mail.sock"),
		respond: map[string]map[string]any{},
		configs: make(chan []byte, 16),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/config" {
			f.configs <- raw
			w.Write([]byte("OK"))
			return
		}
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, capturedReq{method: r.Method, path: r.URL.String(), body: body})
		envelope, ok := f.respond[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if !ok {
			envelope = map[string]any{"success": true}
		}
		json.NewEncoder(w).Encode(envelope)
	})

	l, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return f
}

func (f *fakeMailModule) last(t *testing.T) capturedReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request captured")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeMailModule) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestMailCmd(t *testing.T, dns DNSService) (*Mail, *fakeMailModule) {
	t.Helper()
	f := newFakeMailModule(t)
	cfg := newStore(t)
	m := NewMail(cfg, t.TempDir(), dns)
	m.proc = &fakeProc{running: true, socket: f.sock}
	m.retryDelay = 20 * time.Millisecond
	return m, f
}

func waitConfigPush(t *testing.T, f *fakeMailModule) {
	t.Helper()
	select {
	case <-f.configs:
	case <-time.After(5 * time.Second):
		t.Fatal("no config push received")
	}
}

func TestMailCreateValidation(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{"subdomain": []any{"mail"}}})

	for name, tc := range map[string]struct {
		args []any
		want string
	}{
		"missing fields": {[]any{"a@example.com", "", ""}, "All fields are required."},
		"mismatch":       {[]any{"a@example.com", "x", "y"}, "Passwords do not match."},
		"unknown domain": {[]any{"a@nope.test", "x", "x"}, "Domain nope.test not found."},
		"bad subdomain":  {[]any{"a@smtp.example.com", "x", "x"}, "Domain smtp.example.com not found."},
	} {
		res := m.Create(tc.args[0], tc.args[1], tc.args[2])
		if res.Status || res.Message != tc.want {
			t.Errorf("%s: %+v, want message %q", name, res, tc.want)
		}
	}
	if f.count() != 0 {
		t.Errorf("validation failures still hit the module API (%d requests)", f.count())
	}
}

func TestMailCreateSuccess(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{"subdomain": []any{"mail"}}})

	res := m.Create("a@example.com", "pw", "pw")
	if !res.Status || res.Message != "Mail account a@example.com created successfully." {
		t.Fatalf("Create = %+v", res)
	}
	req := f.last(t)
	want := map[string]any{"domain": "example.com", "email": "a@example.com", "password": "pw", "retype": "pw"}
	if req.method != "POST" || req.path != "/account" || len(req.body) != len(want) {
		t.Errorf("request = %+v", req)
	}
	for k, v := range want {
		if req.body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, req.body[k], v)
		}
	}
	waitConfigPush(t, f) // create success → un-awaited syncConfig

	// Subdomain resolution: mail.example.com resolves to example.com.
	res = m.Create("a@mail.example.com", "pw", "pw")
	if !res.Status {
		t.Fatalf("subdomain Create = %+v", res)
	}
	if f.last(t).body["domain"] != "example.com" {
		t.Errorf("subdomain resolved to %v", f.last(t).body["domain"])
	}
}

func TestMailCreateModuleFailure(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{}})

	f.respond["POST /account"] = map[string]any{"success": false, "message": "Account already exists"}
	res := m.Create("a@example.com", "pw", "pw")
	if res.Status || res.Message != "Account already exists" {
		t.Errorf("module message not passed through: %+v", res)
	}

	f.respond["POST /account"] = map[string]any{"success": false}
	res = m.Create("a@example.com", "pw", "pw")
	if res.Status || res.Message != "Account creation failed." {
		t.Errorf("fallback message = %+v", res)
	}

	// Binary not running → catch-path message.
	m.proc.(*fakeProc).running = false
	res = m.Create("a@example.com", "pw", "pw")
	if res.Status || res.Message != "Account creation failed." {
		t.Errorf("dead module = %+v", res)
	}
}

func TestMailDeletePassword(t *testing.T) {
	m, f := newTestMailCmd(t, nil)

	if res := m.Delete(nil); res.Status || res.Message != "Email address is required." {
		t.Errorf("Delete validation = %+v", res)
	}
	res := m.Delete("a@example.com")
	if !res.Status || res.Message != "Mail account a@example.com deleted successfully." {
		t.Errorf("Delete = %+v", res)
	}
	if req := f.last(t); req.method != "DELETE" || req.path != "/account" || req.body["email"] != "a@example.com" {
		t.Errorf("Delete request = %+v", req)
	}

	if res := m.Password("a@example.com", "x", "y"); res.Status || res.Message != "Passwords do not match." {
		t.Errorf("Password validation = %+v", res)
	}
	res = m.Password("a@example.com", "new", "new")
	if !res.Status || res.Message != "Mail account a@example.com password updated successfully." {
		t.Errorf("Password = %+v", res)
	}
	if req := f.last(t); req.method != "PUT" || req.path != "/account/password" {
		t.Errorf("Password request = %+v", req)
	}
}

func TestMailList(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{}})

	if res := m.List(nil); res.Status || res.Message != "Domain is required." {
		t.Errorf("List validation = %+v", res)
	}
	if res := m.List("nope.test"); res.Status || res.Message != "Domain nope.test not found." {
		t.Errorf("List unknown domain = %+v", res)
	}

	f.respond["GET /accounts"] = map[string]any{"success": true, "accounts": []any{"a@example.com", "b@example.com"}}
	res := m.List("example.com")
	want := "Mail accounts for domain example.com.\na@example.com\nb@example.com"
	if !res.Status || res.Message != want {
		t.Errorf("List = %+v, want %q", res, want)
	}
	if req := f.last(t); req.path != "/accounts?domain=example.com" {
		t.Errorf("List request path = %s", req.path)
	}
}

// Send fixtures generated from the REAL Mail.js (Odac registry stubbed,
// Http.post captured) — the Go assembly must be byte-identical.
func TestMailSendNodeFixtures(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{"subdomain": []any{"mail"}}})

	cases := []struct {
		name     string
		data     string // raw request JSON of the send argument
		wantBody string
		wantFrom string
		wantTo   string
	}{
		{
			name: "multipart",
			data: `{"from":{"value":[{"address":"noreply@example.com"}]},"to":{"value":[{"address":"user@dest.test"}]},` +
				`"header":{"X-Zeta":"last","Content-Type":"multipart/alternative; boundary=\"b123\"","X-Alpha":"first"},` +
				`"subject":"Hi there","html":"<b>hello</b>","text":"hello"}`,
			wantBody: "X-Zeta: last\r\nX-Alpha: first\r\nFrom: noreply@example.com\r\nTo: user@dest.test\r\nSubject: Hi there\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"b123\"\r\n\r\n--b123\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nhello\r\n--b123\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<b>hello</b>\r\n--b123--\r\n",
			wantFrom: "noreply@example.com",
			wantTo:   "user@dest.test",
		},
		{
			// Sender domain resolves by label-shift: mail.example.com → example.com.
			name: "htmlOnly",
			data: `{"from":{"value":[{"address":"noreply@mail.example.com"}]},"to":{"value":[{"address":"user@dest.test"}]},` +
				`"header":{"X-Mailer":"odac"},"subject":"Sub","html":"<i>hi</i>"}`,
			wantBody: "X-Mailer: odac\r\nFrom: noreply@mail.example.com\r\nTo: user@dest.test\r\nSubject: Sub\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<i>hi</i>",
			wantFrom: "noreply@mail.example.com",
			wantTo:   "user@dest.test",
		},
		{
			// Existing From is kept; Reply-To's "to:" substring suppresses
			// the To: injection (Node quirk, preserved).
			name: "textWithFrom",
			data: `{"from":{"value":[{"address":"a@example.com"}]},"to":{"value":[{"address":"b@dest.test"}]},` +
				`"header":{"From":"Custom <a@example.com>","Reply-To":"c@example.com"},"text":"plain body"}`,
			wantBody: "From: Custom <a@example.com>\r\nReply-To: c@example.com\r\nSubject: \r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nplain body",
			wantFrom: "a@example.com",
			wantTo:   "b@dest.test",
		},
		{
			name:     "headerOnly",
			data:     `{"from":{"value":[{"address":"a@example.com"}]},"to":{"value":[{"address":"b@dest.test"}]},"header":{}}`,
			wantBody: "From: a@example.com\r\nTo: b@dest.test\r\nSubject: \r\nMIME-Version: 1.0\r\n\r\n",
			wantFrom: "a@example.com",
			wantTo:   "b@dest.test",
		},
	}

	for _, tc := range cases {
		var data any
		if err := json.Unmarshal([]byte(tc.data), &data); err != nil {
			t.Fatal(err)
		}
		res := m.Send(data, json.RawMessage(tc.data))
		if !res.Status || res.Message != "Mail sent successfully." {
			t.Fatalf("%s: Send = %+v", tc.name, res)
		}
		req := f.last(t)
		if req.method != "POST" || req.path != "/send" {
			t.Errorf("%s: request = %s %s", tc.name, req.method, req.path)
		}
		if got := req.body["body"]; got != tc.wantBody {
			t.Errorf("%s: body mismatch\ngot:  %q\nwant: %q", tc.name, got, tc.wantBody)
		}
		if req.body["from"] != tc.wantFrom || req.body["to"] != tc.wantTo {
			t.Errorf("%s: from/to = %v/%v", tc.name, req.body["from"], req.body["to"])
		}
	}
}

func TestMailSendValidation(t *testing.T) {
	m, f := newTestMailCmd(t, nil)
	m.cfg.Set("domains", map[string]any{"example.com": map[string]any{}})

	for name, tc := range map[string]struct {
		data string
		want string
	}{
		"missing header": {`{"from":{"value":[{"address":"a@example.com"}]},"to":{"value":[{"address":"b@x.y"}]}}`, "All fields are required."},
		"empty from":     {`{"from":{"value":[]},"to":{"value":[{"address":"b@x.y"}]},"header":{}}`, "Invalid email address."},
		"empty to":       {`{"from":{"value":[{"address":"a@example.com"}]},"to":{"value":[{}]},"header":{}}`, "Invalid email address."},
		"unknown domain": {`{"from":{"value":[{"address":"a@sub.nope.test"}]},"to":{"value":[{"address":"b@x.y"}]},"header":{}}`, "Domain nope.test not found."},
	} {
		var data any
		json.Unmarshal([]byte(tc.data), &data)
		res := m.Send(data, json.RawMessage(tc.data))
		if res.Status || res.Message != tc.want {
			t.Errorf("%s: %+v, want %q", name, res, tc.want)
		}
	}
	if f.count() != 0 {
		t.Errorf("validation failures hit the module API (%d requests)", f.count())
	}
}

func TestDKIMGeneration(t *testing.T) {
	dns := &fakeIPSource{}
	m, f := newTestMailCmd(t, dns)
	m.cfg.Set("domains", map[string]any{
		"example.com": map[string]any{},                                                               // MX zone below → gets DKIM
		"nocert.test": map[string]any{"cert": false},                                                  // cert disabled → skipped
		"nomx.test":   map[string]any{},                                                               // no MX record → skipped
		"has.test":    map[string]any{"cert": map[string]any{"dkim": map[string]any{"private": "x"}}}, // already done
	})
	mx := func(name string) map[string]any {
		return map[string]any{"records": []any{map[string]any{"type": "MX", "name": name, "value": "mx." + name}}}
	}
	m.cfg.Set("dns", map[string]any{
		"example.com": mx("example.com"),
		"nocert.test": mx("nocert.test"),
		"has.test":    mx("has.test"),
	})

	m.Start()
	m.Check()

	// The DKIM pass runs on a goroutine; wait for cert.dkim to appear.
	deadline := time.Now().Add(10 * time.Second)
	var dkim map[string]any
	for time.Now().Before(deadline) {
		m.cfg.View(func() {
			rec, _ := m.cfg.Map("domains")["example.com"].(map[string]any)
			cert, _ := rec["cert"].(map[string]any)
			dkim, _ = cert["dkim"].(map[string]any)
		})
		if dkim != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dkim == nil {
		t.Fatal("cert.dkim never set")
	}
	if dkim["selector"] != "default" {
		t.Errorf("selector = %v", dkim["selector"])
	}

	keyPath := str(dkim["private"])
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("private key missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %o, want 600", info.Mode().Perm())
	}
	priv, _ := os.ReadFile(keyPath)
	if !strings.HasPrefix(string(priv), "-----BEGIN RSA PRIVATE KEY-----") {
		t.Errorf("private key not PKCS#1 PEM: %.40s", priv)
	}
	pub, err := os.ReadFile(str(dkim["public"]))
	if err != nil {
		t.Fatalf("public key missing: %v", err)
	}
	if !strings.HasPrefix(string(pub), "-----BEGIN PUBLIC KEY-----") {
		t.Errorf("public key not SPKI PEM: %.40s", pub)
	}

	// TXT record published via the DNS service, p= is the armor-stripped pub.
	records := dns.recorded()
	if len(records) != 1 {
		t.Fatalf("DNS records published = %v", records)
	}
	txt := records[0]
	if txt["type"] != "TXT" || txt["name"] != "default._domainkey.example.com" {
		t.Errorf("TXT = %v", txt)
	}
	stripped := strings.NewReplacer("-----BEGIN PUBLIC KEY-----", "", "-----END PUBLIC KEY-----", "", "\n", "", "\r", "").Replace(string(pub))
	if txt["value"] != "v=DKIM1; k=rsa; p="+stripped {
		t.Errorf("TXT value = %v", txt["value"])
	}

	// Persisted + synced.
	raw, err := os.ReadFile(filepath.Join(m.cfg.BaseDir(), "config", "domain.json"))
	if err != nil || !strings.Contains(string(raw), "_domainkey") && !strings.Contains(string(raw), "dkim") {
		t.Errorf("domain.json not persisted: %v", err)
	}
	waitConfigPush(t, f)

	// Skipped domains stay untouched.
	m.cfg.View(func() {
		domains := m.cfg.Map("domains")
		for _, name := range []string{"nomx.test"} {
			rec, _ := domains[name].(map[string]any)
			if _, ok := rec["cert"]; ok {
				t.Errorf("%s gained a cert: %v", name, rec)
			}
		}
		rec, _ := domains["nocert.test"].(map[string]any)
		if rec["cert"] != false {
			t.Errorf("nocert.test cert = %v, want untouched false", rec["cert"])
		}
	})
}
