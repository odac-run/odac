// Package api is the local command API on TCP 127.0.0.1:1453 and the unix
// socket <base>/run/api.sock (one JSON request per connection,
// \r\n-separated progress lines, one final JSON response, connection
// destroyed after).
//
// Command handlers are registered by cmd/odac-server; an unregistered action
// answers unknown_action, exactly like an unknown one. Two tokens
// authenticate: the root token (raw config.api.auth) and signed app tokens
// (per-action permission list). There are no derived per-domain tokens.
//
//   - Requests are read with a streaming JSON decoder, so a request
//     fragmented across TCP segments still parses. After a malformed
//     document the connection closes, since the byte stream is undecodable
//     at that point.
//   - A unix-socket listen failure is logged and TCP continues, rather than
//     exiting into a watchdog crash loop on a persistent failure.
//   - Signature and token comparisons are constant-time.
//   - Until a handler runs, a connection is bounded: MaxRequestBytes in all,
//     ReadTimeout from accept, and at most MaxPending such connections per
//     listener. The unix socket is mounted into every API-enabled container,
//     so this is what an app, with or without a valid token, can cost the
//     orchestrator.
package api

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// DefaultAddr is the fixed TCP address from the contract.
const DefaultAddr = "127.0.0.1:1453"

// Request limits applied before a handler runs. The largest real request is
// a mail.send body, and 4 MiB of HTML is far past what any mail client shows.
// Real clients write the whole request right after connecting, so the read
// timeout only matters to a peer that trickles bytes or sends none.
const (
	defaultMaxRequestBytes int64 = 4 << 20
	minMaxRequestBytes     int64 = 64 << 10
	defaultReadTimeout           = 10 * time.Second
	defaultMaxPending            = 16
)

// errRequestTooLarge ends a connection that sent more than MaxRequestBytes.
var errRequestTooLarge = errors.New("request too large")

// Progress streams one progress line to the requesting client; values are
// JSON-encoded as-is (handlers pass strings), nil keys are omitted like JS
// undefined.
type Progress func(process, status, message any)

// Args carries the request's positional arguments. At(i) is nil beyond
// bounds, mirroring JS undefined for missing spread arguments; Raw(i)
// exposes the untouched JSON for handlers that need source-order fidelity
// (mail.send iterates header keys in document order).
type Args struct {
	values []any
	raw    []json.RawMessage
	caller Caller
}

// Caller is who sent a request. Most handlers never look: RBAC has already run.
// It exists for actions an app may be granted that still have to be narrowed to
// that app's own records, which only the handler can judge.
type Caller struct {
	// Root is set for the root token (config.api.auth), which the CLI uses.
	Root bool
	// App is the calling app's identity, from its token, for an app token.
	App string
}

// Operator is the caller with the root token's reach, for the server's owner
// reaching a handler without the local API: the hub acts for the Cloud account.
var Operator = Caller{Root: true}

// Caller returns who sent the request. The zero value is neither root nor an
// app, so a handler that checks gives a call site that forgot to fill it nothing.
func (a Args) Caller() Caller { return a.caller }

func (a Args) Len() int { return len(a.values) }

func (a Args) At(i int) any {
	if i < 0 || i >= len(a.values) {
		return nil
	}
	return a.values[i]
}

func (a Args) Raw(i int) json.RawMessage {
	if i < 0 || i >= len(a.raw) {
		return nil
	}
	return a.raw[i]
}

// Handler executes one action. A nil *Result reproduces Node's undefined
// spread ({"id":"..."} only — server.stop does this); a non-nil error maps
// to result(false, err.message || 'error').
type Handler func(args Args, progress Progress) (*Result, error)

// Server is the Api singleton's port.
type Server struct {
	cfg *config.Store
	log *logx.Logger

	// Addr overrides the TCP address (tests only, like ODAC_API_ADDR in the
	// CLI); DefaultAddr when empty. The retry-on-EADDRINUSE loop is part of
	// the zero-downtime handover and applies either way.
	Addr string

	// MaxRequestBytes caps what one connection may send before a handler
	// runs, ReadTimeout how long it may take from accept, and MaxPending how
	// many connections per listener may be in that phase at once (accepting
	// waits for a slot). NewServer fills them; the environment can raise the
	// size with ODAC_API_MAX_REQUEST_BYTES.
	MaxRequestBytes int64
	ReadTimeout     time.Duration
	MaxPending      int

	mu       sync.Mutex
	commands map[string]Handler
	allowed  map[string]bool // runtime IP allowlist (Allow/Disallow)
	tcp      net.Listener
	unix     net.Listener
	started  bool
	retry    *time.Timer
}

// NewServer wires the API server against the shared config store.
func NewServer(cfg *config.Store) *Server {
	s := &Server{
		cfg:         cfg,
		log:         logx.New("Api"),
		commands:    map[string]Handler{},
		allowed:     map[string]bool{},
		ReadTimeout: defaultReadTimeout,
		MaxPending:  defaultMaxPending,
	}
	s.MaxRequestBytes = s.maxRequestBytesFromEnv()
	return s
}

func (s *Server) maxRequestBytesFromEnv() int64 {
	raw := os.Getenv("ODAC_API_MAX_REQUEST_BYTES")
	if raw == "" {
		return defaultMaxRequestBytes
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < minMaxRequestBytes {
		s.log.Warn(fmt.Sprintf("Ignoring invalid ODAC_API_MAX_REQUEST_BYTES=%q (minimum %d)", raw, minMaxRequestBytes))
		return defaultMaxRequestBytes
	}
	return v
}

// Register adds (or replaces) an action handler. cmd/odac-server registers
// the actions whose services exist; the set grows as tasks 3.4–3.7 land.
func (s *Server) Register(action string, h Handler) {
	s.mu.Lock()
	s.commands[action] = h
	s.mu.Unlock()
}

// SocketPath returns the unix socket path (<base>/run/api.sock).
func (s *Server) SocketPath() string {
	return filepath.Join(s.cfg.BaseDir(), "run", "api.sock")
}

// HostSocketDir ports Api's hostSocketDir getter: the run directory holding
// api.sock, mounted read-only into api-enabled app containers (Container's
// ResolveHostPath translates it for the Docker daemon under DooD).
func (s *Server) HostSocketDir() string {
	return filepath.Dir(s.SocketPath())
}

// appDeniedActions are actions an app token may never call, whatever its
// grant says. Two kinds live here: the server's own lifecycle and identity
// (update restarts it, server.stop takes the platform down, auth re-points
// its Cloud pairing), and the ones that hand out privilege (app.privileged
// elevates a container to root or full Docker privileged; app.api rewrites
// the grants this table protects, so an app holding it could simply widen
// itself; domain.mtls.set switches a client certificate policy, and
// domain.mtls.cert.scope widens a certificate, on any domain;
// domain.mtls.ca.import plants a CA key, which signs for every record that
// trusts that CA, and domain.mtls.ca.remove destroys one, cutting off every
// certificate it signed on every record). app.device.add and app.network are
// here for the same reason as app.privileged: a host block device mapped into
// a container is raw access to the host's disk, and a container moved into
// the host's network namespace reaches every loopback service and, as root,
// can join the proxy's SO_REUSEPORT groups on :80 and :443.
//
// app.create is grantable because Manager.Create narrows it by Caller: a
// container an app creates may bind only named volumes inside its own app
// directory (appBindRefusal).
//
// domain.mtls.cert.issue, revoke, list and import are deliberately absent.
// Their danger was reach, not the action: on any domain they mint a way past
// a policy. domains.MTLS narrows them to the records routed to the calling
// app (Args.Caller), on the request path, so a grant lets an edge app such as
// odac-tunnel certify devices for its own hosts and nothing else. A new action
// that is safe only when narrowed that way belongs here until its handler
// does the narrowing.
var appDeniedActions = map[string]bool{
	"auth":                   true,
	"update":                 true,
	"server.stop":            true,
	"app.privileged":         true,
	"app.api":                true,
	"app.device.add":         true,
	"app.network":            true,
	"domain.mtls.set":        true,
	"domain.mtls.cert.scope": true,
	"domain.mtls.ca.import":  true,
	"domain.mtls.ca.remove":  true,
}

// AppMayCall reports whether an app token is ever allowed to call an action.
// Grant-time validation uses it too, but the authoritative check is the one
// on the request path: a hand-edited config never passes through validation.
func AppMayCall(action string) bool {
	return !appDeniedActions[action]
}

// AppDeniedActions lists, in a stable order, the actions no app grant can
// ever include. Callers use it to explain a refusal.
func AppDeniedActions() []string {
	out := make([]string, 0, len(appDeniedActions))
	for action := range appDeniedActions {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}

// HasAction reports whether an action is registered. Grant-time callers use
// it to reject unknown action names up front, instead of persisting a
// permission that can only ever answer permission_denied.
func (s *Server) HasAction(action string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.commands[action]
	return ok
}

// Init ports Api.init(): ensure config.api exists, generate the root auth
// token on first start (32 random bytes, hex).
func (s *Server) Init() {
	apiCfg := s.cfg.Map("api")
	if apiCfg == nil {
		apiCfg = map[string]any{}
		s.cfg.Set("api", apiCfg)
	}
	if v, _ := apiCfg["auth"].(string); v == "" {
		s.cfg.Mutate(func() {
			apiCfg["auth"] = randomHex(32)
			s.cfg.Touch("api")
		})
	}
}

// Allow adds an IP to the runtime TCP allowlist.
func (s *Server) Allow(ip string) {
	s.mu.Lock()
	s.allowed[ip] = true
	s.mu.Unlock()
}

// Disallow removes an IP from the runtime TCP allowlist.
func (s *Server) Disallow(ip string) {
	s.mu.Lock()
	delete(s.allowed, ip)
	s.mu.Unlock()
}

func (s *Server) addr() string {
	if s.Addr != "" {
		return s.Addr
	}
	return DefaultAddr
}

// Start ports Api.start(): TCP with indefinite 1s retry while the old
// instance still holds the port (zero-downtime handover), then the unix
// socket (unlink stale file, listen, chmod 0666).
func (s *Server) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	s.startTCP()
	s.startUnix()
}

func (s *Server) startTCP() {
	l, err := net.Listen("tcp", s.addr())
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			s.log.Log("Port 1453 in use. Waiting for release...")
			s.mu.Lock()
			if s.started {
				s.retry = time.AfterFunc(time.Second, s.startTCP)
			}
			s.mu.Unlock()
		} else {
			s.log.Error(fmt.Sprintf("TCP Server error: %s", err))
		}
		return
	}

	s.mu.Lock()
	if !s.started { // stopped while we were binding
		s.mu.Unlock()
		l.Close()
		return
	}
	s.tcp = l
	s.mu.Unlock()
	go s.acceptLoop(l, false)
}

func (s *Server) startUnix() {
	sockPath := s.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		s.log.Error(fmt.Sprintf("Failed to create socket dir: %s", err))
		return
	}
	if _, err := os.Stat(sockPath); err == nil {
		if err := os.Remove(sockPath); err != nil {
			s.log.Error(fmt.Sprintf("Failed to remove old socket: %s", err))
		}
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		// Node crashes here (unhandled 'error' event); we keep TCP serving —
		// deviation recorded in the package comment.
		s.log.Error(fmt.Sprintf("Unix socket listen failed: %s", err))
		return
	}
	os.Chmod(sockPath, 0o666)
	s.log.Log("Unix socket listening at %s", sockPath)

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		l.Close()
		os.Remove(sockPath)
		return
	}
	s.unix = l
	s.mu.Unlock()
	go s.acceptLoop(l, true)
}

// acceptLoop takes a pending slot before each accept, so a listener never
// holds more than MaxPending connections that have not reached a handler.
// Each listener has its own slots: containers flooding the unix socket do not
// lock the CLI out of TCP.
func (s *Server) acceptLoop(l net.Listener, skipIPCheck bool) {
	pending := make(chan struct{}, max(s.MaxPending, 1))
	for {
		pending <- struct{}{}
		conn, err := l.Accept()
		if err != nil {
			return // listener closed by Stop
		}
		var once sync.Once
		release := func() { once.Do(func() { <-pending }) }
		go func() {
			defer release()
			s.handleConn(conn, skipIPCheck, release)
		}()
	}
}

// Stop ports Api.stop(): close both listeners, unlink the socket file.
func (s *Server) Stop() {
	s.mu.Lock()
	s.started = false
	if s.retry != nil {
		s.retry.Stop()
		s.retry = nil
	}
	tcp, unix := s.tcp, s.unix
	s.tcp, s.unix = nil, nil
	s.mu.Unlock()

	if tcp != nil {
		tcp.Close()
	}
	if unix != nil {
		// Go unlinks the socket file automatically on close; mirror Node's
		// explicit cleanup for stale files regardless.
		unix.Close()
	}
	if _, err := os.Stat(s.SocketPath()); err == nil {
		os.Remove(s.SocketPath())
	}
}

func (s *Server) isAllowed(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed[ip]
}

// handleConn ports the handleConnection closure in Api.init(). release
// frees the connection's pending slot; it runs once a handler starts.
func (s *Server) handleConn(conn net.Conn, skipIPCheck bool, release func()) {
	if !skipIPCheck {
		host := conn.RemoteAddr().String()
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Node: socket.remoteAddress.replace(/^.*:/, '') — strips the
		// ::ffff: prefix of mapped IPv4 (and mangles a literal ::1, which
		// cannot occur on a 127.0.0.1 listener).
		ip := host[strings.LastIndex(host, ":")+1:]
		s.log.Log(fmt.Sprintf("Incoming TCP connection from: %s", ip))
		isLocal := ip == "127.0.0.1" || ip == "::1"
		if !isLocal && !s.isAllowed(ip) {
			s.log.Log(fmt.Sprintf("Blocking connection from unauthorized IP: %s", ip))
			conn.Close()
			return
		}
	}

	c := &apiConn{conn: conn, admit: release}
	defer c.close()

	// One deadline and one byte budget for everything before a handler, not
	// per document: a failed request leaves the connection open for another,
	// and a fresh allowance per attempt would let a peer hold its slot forever.
	if s.ReadTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
	}
	id := connID()
	budget := s.MaxRequestBytes
	if budget <= 0 {
		budget = defaultMaxRequestBytes
	}
	br := bufio.NewReader(&cappedReader{r: conn, left: budget})
	first, err := peekNonSpace(br)
	if err != nil {
		s.logReadLimit(err)
		return // closed before sending anything
	}

	// A top-level scalar can't be end-delimited without reading past it, so
	// the streaming decoder below would block on it forever. Node parses
	// the first chunk as one document either way; do the same here and
	// close after (degenerate input — real requests are objects).
	if first != '{' && first != '[' {
		chunk := make([]byte, 64*1024)
		n, _ := br.Read(chunk)
		raw := bytes.TrimSpace(chunk[:n])
		if !json.Valid(raw) {
			r := Res(false, "invalid_json") // the one final sent WITHOUT an id
			c.write(encodeFinal(id, false, &r))
			return
		}
		s.handleRequest(c, id, json.RawMessage(raw))
		return
	}

	dec := json.NewDecoder(br)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, errRequestTooLarge) {
				s.logReadLimit(err)
				r := Res(false, "request_too_large") // like invalid_json, WITHOUT an id
				c.write(encodeFinal(id, false, &r))
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				s.logReadLimit(err)
				return
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
				errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
				return
			}
			if _, ok := err.(*json.SyntaxError); ok || errors.Is(err, io.ErrUnexpectedEOF) {
				// invalid_json is the one final sent WITHOUT an id.
				r := Res(false, "invalid_json")
				c.write(encodeFinal(id, false, &r))
				return
			}
			s.log.Log(fmt.Sprintf("Socket error: %s", err))
			return
		}
		if done := s.handleRequest(c, id, raw); done {
			return
		}
	}
}

// logReadLimit records a connection cut off by a request limit; any other
// read error is a peer that went away and is not worth a line.
func (s *Server) logReadLimit(err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		s.log.Warn(fmt.Sprintf("Closed a connection that sent more than %d bytes before a handler ran", s.MaxRequestBytes))
	case errors.Is(err, os.ErrDeadlineExceeded):
		s.log.Warn(fmt.Sprintf("Closed a connection that sent no complete request within %s", s.ReadTimeout))
	}
}

// cappedReader fails with errRequestTooLarge once left bytes have been read.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errRequestTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// peekNonSpace returns the first byte of the request past JSON whitespace,
// without consuming it.
func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return 0, err
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			br.Discard(1)
		default:
			return b[0], nil
		}
	}
}

// handleRequest processes one decoded request document. It reports true when
// the connection was destroyed (handler ran or threw); error responses leave
// the connection open for another attempt, exactly like Node's early
// `return socket.write(...)` paths.
func (s *Server) handleRequest(c *apiConn, id string, raw json.RawMessage) bool {
	var payload struct {
		Auth   any               `json:"auth"`
		Action any               `json:"action"`
		Data   []json.RawMessage `json:"data"`
	}
	// A valid JSON document that is not an object destructures to all-
	// undefined in Node ({auth, action, data} = payload || {}); a failed
	// unmarshal here leaves the zero struct, which flows the same way.
	json.Unmarshal(raw, &payload)

	auth, _ := payload.Auth.(string)
	action, _ := payload.Action.(string)

	// Auth logic: the root token, or an app token checked against the live
	// app record.
	isRoot := false
	isApp := false
	appName := ""
	var appPermissions any

	rootKey := s.authKey()
	if rootKey != "" && constantTimeEqual(auth, rootKey) {
		isRoot = true
	} else if appAuth := s.verifyAppToken(auth); appAuth != nil {
		name, _ := appAuth["n"].(string)
		var app map[string]any
		var livePerms any
		s.cfg.View(func() {
			apps, _ := s.cfg.Get("apps").([]any)
			for _, a := range apps {
				if am, _ := a.(map[string]any); am != nil && am["name"] == any(name) {
					app = am
					break
				}
			}
			if app != nil && !jsTruthy(app["active"]) {
				app = nil // inactive counts as missing below
			}
			if app != nil {
				livePerms = copyPermissions(app["api"])
			}
		})
		if app == nil {
			s.log.Warn(fmt.Sprintf("Rejected app token: App '%s' not found or inactive", name))
			r := Res(false, "unauthorized")
			c.write(encodeFinal(id, true, &r))
			return false
		}
		isApp = true
		appName = name
		// Deviation from Node, which trusted the token's own "p" claim: the
		// grant lives in config.apps[].api and is read per request, so
		// revoking one lands on the next call instead of surviving in an
		// already-issued token until the container restarts. The token's "p"
		// stays in the signed payload (wire format untouched) as the
		// issuing-time snapshot.
		appPermissions = livePerms
	} else {
		r := Res(false, "unauthorized")
		c.write(encodeFinal(id, true, &r))
		return false
	}

	s.mu.Lock()
	handler := s.commands[action]
	s.mu.Unlock()
	if action == "" || handler == nil {
		r := Res(false, "unknown_action")
		c.write(encodeFinal(id, true, &r))
		return false
	}

	// RBAC: app tokens carry an explicit permission list ('*' wildcard or the
	// literal true for everything), minus the actions no grant can ever cover.
	if !isRoot {
		allowed := false
		if !AppMayCall(action) {
			s.log.Warn(fmt.Sprintf("Blocked restricted action '%s' from app '%s'", action, appName))
			r := Res(false, "permission_denied")
			c.write(encodeFinal(id, true, &r))
			return false
		}
		switch p := appPermissions.(type) {
		case bool:
			allowed = p
		case []any:
			for _, v := range p {
				if v == any("*") || v == any(action) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			s.log.Warn(fmt.Sprintf("Blocked unauthorized action '%s' from app '%s'", action, appName))
			r := Res(false, "permission_denied")
			c.write(encodeFinal(id, true, &r))
			return false
		}
	}

	caller := Caller{Root: isRoot}
	if isApp {
		caller.App = appName
	}
	args := Args{raw: payload.Data, values: make([]any, len(payload.Data)), caller: caller}
	for i, rawArg := range payload.Data {
		json.Unmarshal(rawArg, &args.values[i]) // failure leaves nil (undefined)
	}
	progress := func(process, status, message any) {
		c.write(encodeProgress(process, status, message))
	}

	c.admitted()
	result, err := runHandler(handler, args, progress)
	if err != nil {
		msg := err.Error()
		if msg == "" {
			msg = "error"
		}
		r := Res(false, msg)
		c.write(encodeFinal(id, true, &r))
		return true
	}
	c.write(encodeFinal(id, true, result))
	return true
}

// runHandler converts a handler panic into Node's thrown-error path
// (result(false, err.message)). This is NOT a keep-alive recover — Node
// wrapped every command in try/catch, so a failing handler answers the
// client instead of killing the orchestrator; parity, per the 3.1 trap-note
// discussion.
func runHandler(h Handler, args Args, progress Progress) (result *Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return h(args, progress)
}

// apiConn serializes writes and drops them once the connection is destroyed
// (Node: this.#connections[id] deleted on close, send() checks it).
type apiConn struct {
	conn   net.Conn
	admit  func()
	mu     sync.Mutex
	closed bool
}

// admitted ends the pre-handler phase: the pending slot is freed, and the
// read deadline is lifted, since a handler may stream progress for minutes.
func (c *apiConn) admitted() {
	c.conn.SetReadDeadline(time.Time{})
	if c.admit != nil {
		c.admit()
	}
}

func (c *apiConn) write(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.conn.Write(b)
}

func (c *apiConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
	}
}

// jsTruthy is JS truthiness for the decoded-JSON values seen here.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	}
	return true
}

// copyPermissions detaches a permission list from the config map so the RBAC
// check below runs outside the store lock without aliasing shared state.
func copyPermissions(v any) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	return append([]any(nil), list...)
}

func constantTimeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// connID mirrors Math.random().toString(36).substring(7): an opaque short
// alphanumeric connection id echoed in final responses.
func connID() string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 6)
	for i := range b {
		b[i] = digits[rand.Intn(len(digits))]
	}
	return string(b)
}

// randomHex ports crypto.randomBytes(n).toString('hex'). Like Node, a failed
// entropy read is fatal — an auth token must never be weak.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
