package proxy

import (
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"odac/internal/proxy/config"
	"odac/internal/proxysig"
)

const (
	// tunnelWSURL is the ODAC Cloud tunnel proxy WebSocket endpoint.
	tunnelWSURL = "wss://tunnel.odac.run/_odac/ws"

	// tunnelReconnectMin and tunnelReconnectMax bound the reconnect wait, which doubles
	// per short-lived attempt so two agents sharing a token do not evict each other
	// every few seconds.
	tunnelReconnectMin = 5 * time.Second
	tunnelReconnectMax = 5 * time.Minute

	// tunnelStableAfter is how long a session must last before the wait resets: the
	// edge accepts a duplicate token and drops the older agent, so a successful dial
	// alone says nothing.
	tunnelStableAfter = time.Minute
)

// tunnelConn represents a single active tunnel connection to ODAC Cloud
// for one domain. Each tunnel has its own WebSocket + yamux session.
type tunnelConn struct {
	domain string
	host   string // Resolved backend IP/hostname
	port   int    // Resolved backend port
	token  string
	key    string
	stopCh chan struct{}

	// mu guards the live connection against closeTunnel, which runs on the config
	// goroutine while connect runs on the tunnel's own.
	mu      sync.Mutex
	stopped bool
	session *yamux.Session
	ws      *websocket.Conn
}

// attach records the WebSocket and session in use, or reports false once the tunnel was
// closed, so a Dial that finished after closeTunnel cannot keep a removed tunnel serving.
func (tc *tunnelConn) attach(ws *websocket.Conn, session *yamux.Session) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.stopped {
		return false
	}
	tc.ws, tc.session = ws, session
	return true
}

// detach forgets the connection connect is about to close.
func (tc *tunnelConn) detach() {
	tc.mu.Lock()
	tc.ws, tc.session = nil, nil
	tc.mu.Unlock()
}

// TunnelManager manages outbound tunnel connections to ODAC Cloud.
// This ODAC instance acts as the tunnel AGENT (client) — it connects
// to the cloud tunnel proxy and forwards incoming streams to local apps.
type TunnelManager struct {
	conns    map[string]*tunnelConn // domain -> active connection
	endpoint string                 // tunnelWSURL; a field only so tests can dial a local server
	mu       sync.RWMutex
}

// NewTunnelManager creates a tunnel manager for outbound cloud connections.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		conns:    make(map[string]*tunnelConn),
		endpoint: tunnelWSURL,
	}
}

// UpdateConfig replaces the entire tunnel configuration (full-replace reconciliation).
// New tunnels are connected, removed tunnels are disconnected, unchanged tunnels are kept.
func (tm *TunnelManager) UpdateConfig(tunnels []config.Tunnel) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Build incoming set for quick lookup
	incoming := make(map[string]config.Tunnel, len(tunnels))
	for _, t := range tunnels {
		incoming[t.Domain] = t
	}

	// Disconnect tunnels that were removed from config
	for domain, conn := range tm.conns {
		if _, exists := incoming[domain]; !exists {
			log.Printf("[Tunnel] Domain removed, disconnecting: %s", domain)
			tm.closeTunnel(conn)
			delete(tm.conns, domain)
		}
	}

	// Start new tunnels or update existing ones
	for domain, t := range incoming {
		if existing, ok := tm.conns[domain]; ok {
			// Token or backend changed — reconnect
			if existing.token != t.Token || existing.host != t.Host || existing.port != t.Port || existing.key != t.Key {
				log.Printf("[Tunnel] Config changed, reconnecting: %s", domain)
				tm.closeTunnel(existing)
				delete(tm.conns, domain)
			} else {
				continue // Already connected with same config
			}
		}

		conn := &tunnelConn{
			domain: domain,
			host:   t.Host,
			port:   t.Port,
			token:  t.Token,
			key:    t.Key,
			stopCh: make(chan struct{}),
		}
		tm.conns[domain] = conn
		go tm.runTunnel(conn)
	}

	log.Printf("[Tunnel] Config updated: %d active tunnel(s)", len(tm.conns))
}

// runTunnel maintains a persistent tunnel connection for a single domain.
// It reconnects automatically on failure until stopCh is closed.
func (tm *TunnelManager) runTunnel(tc *tunnelConn) {
	wait := tunnelReconnectMin
	for {
		select {
		case <-tc.stopCh:
			return
		default:
		}

		started := time.Now()
		err := tm.connect(tc)
		if err != nil {
			log.Printf("[Tunnel] Connection failed for %s: %v", tc.domain, err)
		}
		if time.Since(started) >= tunnelStableAfter {
			wait = tunnelReconnectMin
		}

		timer := time.NewTimer(jitter(wait))
		select {
		case <-tc.stopCh:
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = min(wait*2, tunnelReconnectMax)
	}
}

// jitter spreads a wait over [d/2, d) so agents dropped together do not return together.
func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2)
}

// connect establishes a single WebSocket + yamux session to ODAC Cloud.
// It blocks until the session closes or an error occurs.
func (tm *TunnelManager) connect(tc *tunnelConn) error {
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		ReadBufferSize:    128 * 1024,
		WriteBufferSize:   128 * 1024,
		EnableCompression: false,
	}

	header := http.Header{}
	header.Set("X-Agent-Domain", tc.domain)
	// In a header, not the query: a URL lands in access logs in front of the edge.
	header.Set("Authorization", "Bearer "+tc.token)

	log.Printf("[Tunnel] Connecting to cloud for domain: %s", tc.domain)

	ws, _, err := dialer.Dial(tm.endpoint, header)
	if err != nil {
		return err
	}
	if !tc.attach(ws, nil) {
		ws.Close()
		return nil
	}

	log.Printf("[Tunnel] Connected for domain: %s", tc.domain)

	// Wrap WebSocket as net.Conn for yamux
	conn := newWSConn(ws)

	// Start yamux CLIENT session — ODAC Cloud is the server that opens streams
	yamuxCfg := yamux.DefaultConfig()
	yamuxCfg.EnableKeepAlive = true
	yamuxCfg.KeepAliveInterval = 15 * time.Second
	yamuxCfg.ConnectionWriteTimeout = 10 * time.Second
	yamuxCfg.MaxStreamWindowSize = 4 * 1024 * 1024 // 4MB — must match server side
	yamuxCfg.LogOutput = io.Discard

	session, err := yamux.Client(conn, yamuxCfg)
	if err != nil {
		tc.detach()
		ws.Close()
		return err
	}
	if !tc.attach(ws, session) {
		session.Close()
		ws.Close()
		return nil
	}

	serveTunnel(tc, session)

	// Cleanup
	tc.detach()
	session.Close()
	ws.Close()

	log.Printf("[Tunnel] Disconnected from cloud for domain: %s", tc.domain)
	return nil
}

// serveTunnel answers the Cloud's streams as HTTP until the session ends. Each
// stream is one keep-alive connection.
func serveTunnel(tc *tunnelConn, session net.Listener) {
	handler, transport := newTunnelProxy(tc)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	err := srv.Serve(session)
	srv.Close()
	transport.CloseIdleConnections()
	select {
	case <-tc.stopCh:
	default:
		debugLog("[Tunnel] Session ended for %s: %v", tc.domain, err)
	}
}

// newTunnelProxy forwards a tunnel's requests to its app. ODAC Cloud reports the
// visitor's address and TLS state, and the stream comes from the Cloud this
// agent dialed over verified TLS, so the agent carries those two values over
// and signs them with the app's key, as the proxy does for a request it
// receives itself. A raw pipe could not sign, and the app would have
// to choose between trusting nobody's headers and anybody's.
func newTunnelProxy(tc *tunnelConn) (*httputil.ReverseProxy, *http.Transport) {
	target := net.JoinHostPort(tc.host, strconv.Itoa(tc.port))
	transport := newUpstreamTransport()
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			remoteIP, ssl := cloudConnection(pr.In.Header)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			stripForwardingHeaders(pr.Out.Header)
			setConnectionHeaders(pr.Out, remoteIP, ssl, tc.key)
		},
		Transport:     transport,
		BufferPool:    bufferPool{},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			debugLog("[Tunnel] Backend %s for %s: %v", target, tc.domain, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}, transport
}

// cloudConnection reads what ODAC Cloud said about the visitor. An address
// that does not parse is dropped rather than signed.
func cloudConnection(h http.Header) (remoteIP string, ssl bool) {
	remoteIP = h.Get(proxysig.HeaderRemoteAddress)
	if net.ParseIP(remoteIP) == nil {
		remoteIP = ""
	}
	return remoteIP, h.Get(proxysig.HeaderSSL) == "true"
}

// wsConn wraps gorilla/websocket.Conn to implement net.Conn for yamux.
// Yamux requires a stream-oriented net.Conn but WebSocket is message-based,
// so we bridge by reading/writing binary messages as a continuous byte stream.
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
	mu     sync.Mutex // Serialize writes (WebSocket is not concurrent-write safe)
}

func newWSConn(ws *websocket.Conn) *wsConn {
	return &wsConn{ws: ws}
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(p)
			if err == io.EOF {
				c.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}

		_, reader, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = reader
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.ws.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error         { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// closeTunnel signals a tunnel goroutine to stop and forcefully closes
// the underlying yamux session and WebSocket connection. This ensures
// AcceptStream unblocks immediately instead of hanging until the remote
// side closes — preventing goroutine and connection leaks.
func (tm *TunnelManager) closeTunnel(tc *tunnelConn) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.stopped {
		return
	}
	tc.stopped = true
	close(tc.stopCh)
	if tc.session != nil {
		tc.session.Close()
	}
	if tc.ws != nil {
		tc.ws.Close()
	}
}

// Stop gracefully shuts down all active tunnel connections.
func (tm *TunnelManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for domain, conn := range tm.conns {
		log.Printf("[Tunnel] Stopping tunnel: %s", domain)
		tm.closeTunnel(conn)
	}
	tm.conns = make(map[string]*tunnelConn)
}
