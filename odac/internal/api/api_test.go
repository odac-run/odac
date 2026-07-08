package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// Node-generated fixtures (crypto.createHmac with the same key) — the Go
// token helpers must reproduce them byte-for-byte.
const (
	fixtureKey         = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fixtureDomainToken = "6c4653c8d7e85f889af168d2e86f3dd865a264e276d2a8bb47d8ba6c91454c3f" // HMAC(key, "example.com")
	fixtureAppToken    = "eyJuIjoibXlhcHAiLCJwIjpbIm1haWwuc2VuZCJdLCJ0IjoxNzAwMDAwMDAwMDAwfQ==.79ecc5e2b552a8b08b45dc702782d80e3553b0d0381e7edc8b7f2855bdfec1d0"
	fixtureAppTokenAll = "eyJuIjoiYW55YXBwIiwicCI6dHJ1ZSwidCI6MTcwMDAwMDAwMDAwMH0=.d670dabe912cdd5340a7206a2023e58aa8a0509a98f9e3f70be01986e7d2eee2"
	fixtureAppTokenAny = "eyJuIjoic3RhciIsInAiOlsiKiJdLCJ0IjoxNzAwMDAwMDAwMDAwfQ==.c3085bbb44ff720dea2237c1cb4ea9feab0e724c374538f54b5ee01efb51f438"
)

func TestMain(m *testing.M) {
	logx.Stdout = io.Discard
	logx.Stderr = io.Discard
	os.Exit(m.Run())
}

// shortTempDir avoids t.TempDir for unix sockets (the ~104-byte sun_path
// limit; t.TempDir embeds the full test name on macOS).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "odacapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg, err := config.Open(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("api", map[string]any{"auth": fixtureKey})
	s := NewServer(cfg)
	s.Addr = "127.0.0.1:0"
	return s
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.Init()
	s.Start()
	t.Cleanup(s.Stop)
	waitListening(t, s)
	return s
}

func waitListening(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		l := s.tcp
		s.mu.Unlock()
		if l != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never bound its TCP listener")
}

func tcpAddr(s *Server) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcp.Addr().String()
}

// call sends one raw request and returns every \r\n-separated line plus the
// unterminated final chunk, reading until the server closes the connection.
func call(t *testing.T, network, addr, raw string) []string {
	t.Helper()
	conn, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	// Error finals leave the connection open (Node parity), so reads on
	// those paths run out this deadline instead of seeing a close.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	data, _ := io.ReadAll(conn)
	var lines []string
	for _, part := range strings.Split(string(data), "\r\n") {
		if part != "" {
			lines = append(lines, part)
		}
	}
	return lines
}

func request(auth, action string, args ...any) string {
	req := map[string]any{"auth": auth, "action": action}
	if args != nil {
		req["data"] = args
	}
	raw, _ := json.Marshal(req)
	return string(raw)
}

func TestGenerateTokenMatchesNode(t *testing.T) {
	s := newTestServer(t)
	if got := s.GenerateToken("example.com"); got != fixtureDomainToken {
		t.Errorf("GenerateToken = %s, want Node fixture %s", got, fixtureDomainToken)
	}
}

func TestGenerateAppTokenRoundTrip(t *testing.T) {
	s := newTestServer(t)
	token := s.GenerateAppToken("myapp", []any{"mail.send"})
	decoded := s.verifyAppToken(token)
	if decoded == nil {
		t.Fatal("self-issued app token failed verification")
	}
	if decoded["n"] != "myapp" {
		t.Errorf("n = %v", decoded["n"])
	}
	// The Node fixture (fixed t) must verify too.
	if s.verifyAppToken(fixtureAppToken) == nil {
		t.Error("Node-generated app token failed verification")
	}
}

func TestVerifyAppTokenRejects(t *testing.T) {
	s := newTestServer(t)
	tampered := strings.Replace(fixtureAppToken, "79ec", "79ed", 1)
	for name, token := range map[string]string{
		"no dot":        "abcdef",
		"empty sig":     "eyJuIjoieCJ9.",
		"empty payload": ".abc",
		"bad signature": tampered,
	} {
		if s.verifyAppToken(token) != nil {
			t.Errorf("%s: token verified, want nil", name)
		}
	}
}

func TestRootAuthAndUnknownAction(t *testing.T) {
	s := startTestServer(t)
	s.Register("echo", func(a Args, _ Progress) (*Result, error) {
		r := Res(true, fmt.Sprintf("got %v", a.At(0)))
		return &r, nil
	})

	lines := call(t, "tcp", tcpAddr(s), request(fixtureKey, "echo", "x"))
	if len(lines) != 1 || !strings.Contains(lines[0], `"result":true`) || !strings.Contains(lines[0], `"got x"`) {
		t.Errorf("echo reply = %v", lines)
	}

	lines = call(t, "tcp", tcpAddr(s), request(fixtureKey, "nope"))
	if len(lines) != 1 || !strings.Contains(lines[0], `"message":"unknown_action"`) {
		t.Errorf("unknown action reply = %v", lines)
	}
}

func TestFinalResponseShape(t *testing.T) {
	s := startTestServer(t)
	s.Register("obj", func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, map[string]any{"a": float64(1)})
		return &r, nil
	})
	s.Register("none", func(_ Args, _ Progress) (*Result, error) {
		return nil, nil // server.stop quirk: {"id":"..."} only
	})
	s.Register("boom", func(_ Args, _ Progress) (*Result, error) {
		return nil, errors.New("kapow")
	})

	// Object message moves to data with an explicit null message.
	lines := call(t, "tcp", tcpAddr(s), request(fixtureKey, "obj"))
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp["message"]) != "null" || string(resp["data"]) != `{"a":1}` {
		t.Errorf("obj reply = %s", lines[0])
	}
	// Key order matches Node's JSON.stringify({id, ...result}).
	if !strings.HasPrefix(lines[0], `{"id":"`) || !strings.Contains(lines[0], `"result":true,"message":null,"data":`) {
		t.Errorf("obj reply key order = %s", lines[0])
	}

	lines = call(t, "tcp", tcpAddr(s), request(fixtureKey, "none"))
	var idOnly map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &idOnly); err != nil {
		t.Fatal(err)
	}
	if len(idOnly) != 1 || idOnly["id"] == nil {
		t.Errorf("undefined-result reply = %s, want id-only", lines[0])
	}

	lines = call(t, "tcp", tcpAddr(s), request(fixtureKey, "boom"))
	if !strings.Contains(lines[0], `"result":false`) || !strings.Contains(lines[0], `"message":"kapow"`) {
		t.Errorf("thrown-error reply = %s", lines[0])
	}
}

func TestProgressStreaming(t *testing.T) {
	s := startTestServer(t)
	s.Register("steps", func(_ Args, progress Progress) (*Result, error) {
		progress("build", "running", "step one")
		progress("build", "success", "done")
		r := Res(true, "finished")
		return &r, nil
	})

	lines := call(t, "tcp", tcpAddr(s), request(fixtureKey, "steps"))
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want 2 progress + 1 final", lines)
	}
	if lines[0] != `{"process":"build","status":"running","message":"step one"}` {
		t.Errorf("progress line = %s", lines[0])
	}
	if !strings.Contains(lines[2], `"result":true`) {
		t.Errorf("final = %s", lines[2])
	}
}

func TestInvalidJSONHasNoID(t *testing.T) {
	s := startTestServer(t)
	lines := call(t, "tcp", tcpAddr(s), "{nope")
	if len(lines) != 1 || lines[0] != `{"result":false,"message":"invalid_json"}` {
		t.Errorf("invalid json reply = %v", lines)
	}
}

func TestUnauthorized(t *testing.T) {
	s := startTestServer(t)
	s.Register("echo", func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, "yes")
		return &r, nil
	})
	for name, raw := range map[string]string{
		"wrong token":  request("wrong", "echo"),
		"empty auth":   request("", "echo"),
		"non-object":   `"hello"`,
		"null request": "null",
	} {
		lines := call(t, "tcp", tcpAddr(s), raw)
		if len(lines) != 1 || !strings.Contains(lines[0], `"message":"unauthorized"`) {
			t.Errorf("%s: reply = %v", name, lines)
		}
	}
}

func TestDomainTokenRBAC(t *testing.T) {
	s := startTestServer(t)
	s.cfg.Set("domains", map[string]any{"example.com": map[string]any{}})
	s.ReloadTokens()
	sent := false
	s.Register("mail.send", func(_ Args, _ Progress) (*Result, error) {
		sent = true
		r := Res(true, "ok")
		return &r, nil
	})
	s.Register("app.list", func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, "nope")
		return &r, nil
	})

	lines := call(t, "tcp", tcpAddr(s), request(fixtureDomainToken, "mail.send"))
	if !sent || !strings.Contains(lines[0], `"result":true`) {
		t.Errorf("domain token mail.send = %v (sent=%v)", lines, sent)
	}
	lines = call(t, "tcp", tcpAddr(s), request(fixtureDomainToken, "app.list"))
	if !strings.Contains(lines[0], `"message":"permission_denied"`) {
		t.Errorf("domain token app.list = %v", lines)
	}

	s.RemoveToken("example.com")
	lines = call(t, "tcp", tcpAddr(s), request(fixtureDomainToken, "mail.send"))
	if !strings.Contains(lines[0], `"message":"unauthorized"`) {
		t.Errorf("removed token = %v", lines)
	}
}

func TestAppTokenRBAC(t *testing.T) {
	s := startTestServer(t)
	s.cfg.Set("apps", []any{
		map[string]any{"name": "myapp", "active": true},
		map[string]any{"name": "star", "active": true},
		map[string]any{"name": "anyapp", "active": false},
	})
	ok := func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, "ok")
		return &r, nil
	}
	s.Register("mail.send", ok)
	s.Register("app.list", ok)

	// Explicit permission list: allowed action passes, others denied.
	lines := call(t, "tcp", tcpAddr(s), request(fixtureAppToken, "mail.send"))
	if !strings.Contains(lines[0], `"result":true`) {
		t.Errorf("app token allowed action = %v", lines)
	}
	lines = call(t, "tcp", tcpAddr(s), request(fixtureAppToken, "app.list"))
	if !strings.Contains(lines[0], `"message":"permission_denied"`) {
		t.Errorf("app token denied action = %v", lines)
	}

	// "*" wildcard allows everything.
	lines = call(t, "tcp", tcpAddr(s), request(fixtureAppTokenAny, "app.list"))
	if !strings.Contains(lines[0], `"result":true`) {
		t.Errorf("wildcard token = %v", lines)
	}

	// Inactive app: unauthorized even with permissions true.
	lines = call(t, "tcp", tcpAddr(s), request(fixtureAppTokenAll, "app.list"))
	if !strings.Contains(lines[0], `"message":"unauthorized"`) {
		t.Errorf("inactive app token = %v", lines)
	}

	// Unknown app name: unauthorized.
	s.cfg.Set("apps", []any{})
	lines = call(t, "tcp", tcpAddr(s), request(fixtureAppToken, "mail.send"))
	if !strings.Contains(lines[0], `"message":"unauthorized"`) {
		t.Errorf("unknown app token = %v", lines)
	}
}

func TestUnixSocket(t *testing.T) {
	s := startTestServer(t)
	sock := s.SocketPath()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("unix socket missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o666 {
		t.Errorf("socket mode = %o, want 666", perm)
	}

	s.Register("echo", func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, "via unix")
		return &r, nil
	})
	lines := call(t, "unix", sock, request(fixtureKey, "echo"))
	if !strings.Contains(lines[0], `"via unix"`) {
		t.Errorf("unix reply = %v", lines)
	}

	s.Stop()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file not unlinked after Stop: %v", err)
	}
}

func TestUnixSocketReplacesStaleFile(t *testing.T) {
	s := newTestServer(t)
	sock := s.SocketPath()
	os.MkdirAll(filepath.Dir(sock), 0o755)
	if err := os.WriteFile(sock, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Init()
	s.Start()
	t.Cleanup(s.Stop)
	waitListening(t, s)

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Error("stale file was not replaced by a socket")
	}
}

func TestPortInUseRetries(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := blocker.Addr().String()

	s := newTestServer(t)
	s.Addr = addr
	s.Init()
	s.Start() // EADDRINUSE → 1s retry loop (zero-downtime handover)
	t.Cleanup(s.Stop)

	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	bound := s.tcp != nil
	s.mu.Unlock()
	if bound {
		t.Fatal("server bound a busy port")
	}

	blocker.Close() // old instance releases the port
	waitListening(t, s)
	s.Register("echo", func(_ Args, _ Progress) (*Result, error) {
		r := Res(true, "up")
		return &r, nil
	})
	lines := call(t, "tcp", addr, request(fixtureKey, "echo"))
	if !strings.Contains(lines[0], `"up"`) {
		t.Errorf("post-retry reply = %v", lines)
	}
}

func TestInitGeneratesAuth(t *testing.T) {
	cfg, err := config.Open(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg)
	s.Init()
	auth, _ := cfg.Map("api")["auth"].(string)
	if len(auth) != 64 {
		t.Fatalf("generated auth = %q, want 64 hex chars", auth)
	}
	s.Init() // second init keeps the existing token
	if again, _ := cfg.Map("api")["auth"].(string); again != auth {
		t.Error("Init regenerated an existing auth token")
	}
}

func TestArgsAt(t *testing.T) {
	a := Args{values: []any{"x", float64(2)}}
	if a.At(0) != "x" || a.At(1) != float64(2) {
		t.Errorf("At = %v, %v", a.At(0), a.At(1))
	}
	if a.At(2) != nil || a.At(-1) != nil {
		t.Error("out-of-range At should be nil (JS undefined)")
	}
}

func TestResNormalization(t *testing.T) {
	// Scalar message stays put, no data key.
	if got := string(encodeFinal("i", true, ptr(Res(false, "msg")))); got != `{"id":"i","result":false,"message":"msg"}` {
		t.Errorf("scalar = %s", got)
	}
	// Undefined message → both keys omitted.
	if got := string(encodeFinal("i", true, ptr(Res(true, nil)))); got != `{"id":"i","result":true}` {
		t.Errorf("nil message = %s", got)
	}
	// Array message moves to data too (typeof [] === 'object').
	if got := string(encodeFinal("i", true, ptr(Res(true, []any{"a"})))); got != `{"id":"i","result":true,"message":null,"data":["a"]}` {
		t.Errorf("array = %s", got)
	}
	// Explicit data keeps the message in place.
	if got := string(encodeFinal("i", true, ptr(Res(true, "m", map[string]any{})))); got != `{"id":"i","result":true,"message":"m","data":{}}` {
		t.Errorf("explicit data = %s", got)
	}
}

func ptr(r Result) *Result { return &r }
