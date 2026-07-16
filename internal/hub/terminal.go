// terminal.go ports server/src/Hub/Terminal.js (TerminalManager +
// TerminalSession): per-session dedicated WebSockets to
// <HUB_WS_URL>/terminal/<id> carrying raw pty bytes as binary frames and
// JSON control frames as text — deliberately OFF the main signed Hub socket
// (bulky binary output would starve its ping/pong and get the agent kicked,
// and every keystroke would pay an HMAC over canonical JSON). Full pinning
// in contracts/hub-protocol.md "Terminal sessions".
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"

	"odac/internal/config"
	"odac/internal/docker"
	"odac/internal/logx"
)

// idPattern guards both Hub-minted opaque tokens: they land in a URL path
// and a header, never in a shell command, but anything else is a red flag.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

const terminalConnectTimeout = 10 * time.Second

// maxBufferedBytes: terminal output is unbounded — once this much is
// unflushed the session is beyond saving: dropping chunks would corrupt the
// stream, so it closes honestly instead of growing the heap. Variable only
// for tests.
var maxBufferedBytes = 8 * 1024 * 1024

// terminalDefaults mirrors DEFAULTS in Hub/Terminal.js. allowPrivileged
// stays a deliberate operator opt-in even with terminals enabled: a shell
// in a privileged container is effectively a host shell.
var terminalDefaults = map[string]any{
	"enabled":         true,
	"maxSessions":     float64(3),
	"idleTimeout":     float64(15 * 60 * 1000),
	"maxLifetime":     float64(4 * 60 * 60 * 1000),
	"allowPrivileged": false,
}

type terminalManager struct {
	log      *logx.Logger
	wsURL    string
	getToken func() string
	cfg      *config.Store
	exec     ContainerService // nil on docker-less hosts

	// dial is the test seam for the per-session socket.
	dial func(url, token, ticket string) (*websocket.Conn, error)

	mu       sync.Mutex
	sessions map[string]*terminalSession
}

func newTerminalManager(wsURL string, getToken func() string, cfg *config.Store, exec ContainerService) *terminalManager {
	return &terminalManager{
		log:      logx.New("Hub", "Terminal"),
		wsURL:    wsURL,
		getToken: getToken,
		cfg:      cfg,
		exec:     exec,
		sessions: map[string]*terminalSession{},
		dial: func(url, token, ticket string) (*websocket.Conn, error) {
			ctx, cancel := context.WithTimeout(context.Background(), terminalConnectTimeout)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
				HTTPHeader: http.Header{
					"Authorization": []string{"Bearer " + token},
					"X-Odac-Ticket": []string{ticket},
				},
			})
			return conn, err
		},
	}
}

func (t *terminalManager) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// settings merges config.hub.terminal over the defaults.
func (t *terminalManager) settings() map[string]any {
	merged := map[string]any{}
	for k, v := range terminalDefaults {
		merged[k] = v
	}
	t.cfg.View(func() {
		configured, _ := t.cfg.Map("hub")["terminal"].(map[string]any)
		for k, v := range configured {
			merged[k] = v
		}
	})
	return merged
}

// open handles the terminal.open command. Guard order and every message are
// pinned in hub-protocol.md.
func (t *terminalManager) open(payload map[string]any) (any, error) {
	settings := t.settings()

	if !jsTruthy(settings["enabled"]) {
		return map[string]any{"success": false, "message": "Terminal access is disabled"}, nil
	}

	app, _ := payload["app"].(string)
	sessionID, _ := payload["sessionId"].(string)
	ticket, _ := payload["ticket"].(string)

	if !idPattern.MatchString(sessionID) || !idPattern.MatchString(ticket) {
		return map[string]any{"success": false, "message": "Invalid session id or ticket"}, nil
	}
	if app == "" {
		return map[string]any{"success": false, "message": "Missing app"}, nil
	}

	maxSessions := int(num(settings["maxSessions"]))

	t.mu.Lock()
	if _, exists := t.sessions[sessionID]; exists {
		t.mu.Unlock()
		return map[string]any{"success": false, "message": "Session already open"}, nil
	}
	if len(t.sessions) >= maxSessions {
		t.mu.Unlock()
		return map[string]any{"success": false, "message": fmt.Sprintf("Too many terminal sessions (max %d)", maxSessions)}, nil
	}

	token := t.getToken()
	if token == "" {
		t.mu.Unlock()
		return map[string]any{"success": false, "message": "Not authenticated"}, nil
	}

	session := &terminalSession{
		id:  sessionID,
		app: app,
		log: t.log,
		onClosed: func(id string) {
			t.mu.Lock()
			delete(t.sessions, id)
			t.mu.Unlock()
		},
	}
	// Reserve the slot before the blocking work below, or concurrent opens
	// both see room.
	t.sessions[sessionID] = session
	t.mu.Unlock()

	err := session.open(t, sessionOpts{
		url:             t.wsURL + "/terminal/" + sessionID,
		token:           token,
		ticket:          ticket,
		cols:            int(num(payload["cols"])),
		rows:            int(num(payload["rows"])),
		idleTimeout:     time.Duration(num(settings["idleTimeout"])) * time.Millisecond,
		maxLifetime:     time.Duration(num(settings["maxLifetime"])) * time.Millisecond,
		allowPrivileged: jsTruthy(settings["allowPrivileged"]),
	})
	if err != nil {
		t.mu.Lock()
		delete(t.sessions, sessionID)
		t.mu.Unlock()
		t.log.Error("[Terminal] Session "+sessionID+" failed to open on "+app+":", err.Error())
		return map[string]any{"success": false, "message": err.Error()}, nil
	}

	return map[string]any{
		"success": true,
		"message": "Terminal session opened",
		"data":    map[string]any{"sessionId": sessionID, "app": app},
	}, nil
}

// close handles the terminal.close command.
func (t *terminalManager) close(payload map[string]any) (any, error) {
	sessionID, _ := payload["sessionId"].(string)
	t.mu.Lock()
	session := t.sessions[sessionID]
	t.mu.Unlock()
	if session == nil {
		return map[string]any{"success": false, "message": "Unknown session"}, nil
	}
	session.close("command")
	return map[string]any{"success": true, "message": "Terminal session closed"}, nil
}

// closeAll tears every session down: the Hub connection dropping means
// nobody is watching, and an unattended shell must not outlive it.
func (t *terminalManager) closeAll() {
	t.mu.Lock()
	if len(t.sessions) == 0 {
		t.mu.Unlock()
		return
	}
	count := len(t.sessions)
	sessions := make([]*terminalSession, 0, count)
	for _, s := range t.sessions {
		sessions = append(sessions, s)
	}
	t.mu.Unlock()

	t.log.Log("[Terminal] Closing %s active session(s)", count)
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *terminalSession) {
			defer wg.Done()
			s.close("hub_disconnected")
		}(s)
	}
	wg.Wait()
}

// num coerces a decoded-JSON numeric setting.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

type sessionOpts struct {
	url, token, ticket string
	cols, rows         int
	idleTimeout        time.Duration
	maxLifetime        time.Duration
	allowPrivileged    bool
}

// terminalSession pairs one dedicated Hub socket with one container exec.
type terminalSession struct {
	id, app  string
	log      *logx.Logger
	onClosed func(id string)

	mu      sync.Mutex
	conn    *websocket.Conn
	term    TerminalExec
	closing bool

	// Outbound pty bytes flow through a writer goroutine so a slow client
	// applies bounded backpressure instead of blocking the exec reader;
	// pending mirrors ws bufferedAmount for the 8 MiB overflow cut-off.
	sendCh  chan []byte
	pending int
}

// open creates the container exec FIRST, then dials the Hub: a bad app name
// or a stopped container must fail in the command response, not as a socket
// that opens and immediately dies.
func (s *terminalSession) open(mgr *terminalManager, opts sessionOpts) error {
	if mgr.exec == nil {
		return fmt.Errorf("Docker is not available")
	}
	term, err := mgr.exec.CreateTerminalSession(s.app, docker.TerminalOptions{
		Cols:            opts.cols,
		Rows:            opts.rows,
		IdleTimeout:     &opts.idleTimeout,
		MaxLifetime:     &opts.maxLifetime,
		AllowPrivileged: opts.allowPrivileged,
		OnData:          s.send,
		OnExit:          func(info docker.ExitInfo) { s.handleExit(info) },
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.term = term
	s.sendCh = make(chan []byte, 256)
	s.mu.Unlock()

	conn, err := mgr.dial(opts.url, opts.token, opts.ticket)
	if err != nil {
		term.Close("error")
		return err
	}
	conn.SetReadLimit(int64(maxBufferedBytes))

	s.mu.Lock()
	s.conn = conn
	sendCh := s.sendCh
	s.mu.Unlock()

	go s.writeLoop(conn, sendCh)
	go s.readLoop(conn)

	s.log.Log("[Terminal] Session %s attached to %s", s.id, s.app)
	return nil
}

// send queues pty output as a binary frame; > 8 MiB unflushed closes the
// session (see maxBufferedBytes).
func (s *terminalSession) send(chunk []byte) {
	s.mu.Lock()
	if s.closing || s.conn == nil || s.sendCh == nil {
		s.mu.Unlock()
		return
	}
	if s.pending+len(chunk) > maxBufferedBytes {
		s.mu.Unlock()
		s.log.Error("[Terminal] Session " + s.id + " outran its socket, closing")
		s.close("overflow")
		return
	}
	buf := append([]byte(nil), chunk...)
	// Queued under the lock: close() nils sendCh before closing it, so this
	// can never hit a closed channel.
	select {
	case s.sendCh <- buf:
		s.pending += len(buf)
		s.mu.Unlock()
	default:
		// Writer starved with a full queue: same beyond-saving condition.
		s.mu.Unlock()
		s.log.Error("[Terminal] Session " + s.id + " outran its socket, closing")
		s.close("overflow")
	}
}

func (s *terminalSession) writeLoop(conn *websocket.Conn, sendCh <-chan []byte) {
	for buf := range sendCh {
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		err := conn.Write(ctx, websocket.MessageBinary, buf)
		cancel()
		s.mu.Lock()
		s.pending -= len(buf)
		s.mu.Unlock()
		if err != nil {
			return // read loop notices the dead socket and closes
		}
	}
}

func (s *terminalSession) readLoop(conn *websocket.Conn) {
	for {
		typ, data, err := conn.Read(context.Background())
		if err != nil {
			s.close("socket")
			return
		}
		// Keystrokes are the overwhelming majority; cheapest path first.
		if typ == websocket.MessageBinary {
			s.mu.Lock()
			term := s.term
			s.mu.Unlock()
			if term != nil {
				term.Write(data)
			}
			continue
		}
		s.handleControl(data)
	}
}

func (s *terminalSession) handleControl(data []byte) {
	var msg struct {
		Type string  `json:"type"`
		Cols float64 `json:"cols"`
		Rows float64 `json:"rows"`
	}
	if json.Unmarshal(data, &msg) != nil {
		s.log.Log("[Terminal] Session %s sent malformed control frame", s.id)
		return
	}
	switch msg.Type {
	case "resize":
		s.mu.Lock()
		term := s.term
		s.mu.Unlock()
		if term != nil {
			term.Resize(int(msg.Cols), int(msg.Rows))
		}
	case "close":
		s.close("remote")
	default:
		s.log.Log("[Terminal] Session %s sent unknown control frame: %s", s.id, msg.Type)
	}
}

// handleExit: the shell ended on its own (or a timer reaped it) — tell the
// Hub, then hang up.
func (s *terminalSession) handleExit(info docker.ExitInfo) {
	s.mu.Lock()
	conn := s.conn
	closing := s.closing
	s.mu.Unlock()
	if conn != nil && !closing {
		payload, _ := json.Marshal(struct {
			Type     string `json:"type"`
			Reason   string `json:"reason"`
			ExitCode any    `json:"exitCode"`
		}{"exit", info.Reason, info.ExitCode})
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		conn.Write(ctx, websocket.MessageText, payload)
		cancel()
	}
	s.close(info.Reason)
}

// close is idempotent and reachable from both ends: the socket closing must
// reap the exec, and the exec exiting must close the socket.
func (s *terminalSession) close(reason string) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	conn := s.conn
	term := s.term
	ch := s.sendCh
	s.conn = nil
	s.term = nil
	s.sendCh = nil
	s.mu.Unlock()

	if ch != nil {
		close(ch)
	}
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
	if term != nil && !term.Closed() {
		term.Close(reason)
	}

	s.log.Log("[Terminal] Session %s detached from %s (%s)", s.id, s.app, reason)
	if s.onClosed != nil {
		s.onClosed(s.id)
	}
}
