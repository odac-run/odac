package proxy

import (
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"odac-proxy/config"
)

const (
	// tunnelWSURL is the ODAC Cloud tunnel proxy WebSocket endpoint.
	tunnelWSURL = "wss://tunnel.odac.run/_odac/ws"

	// tunnelReconnectInterval is the delay between reconnection attempts.
	tunnelReconnectInterval = 5 * time.Second

	// tunnelCopyBufSize is the buffer size for bidirectional stream copying.
	// 64KB reduces syscall frequency vs the default 32KB, improving throughput.
	tunnelCopyBufSize = 64 * 1024
)

// tunnelConn represents a single active tunnel connection to ODAC Cloud
// for one domain. Each tunnel has its own WebSocket + yamux session.
type tunnelConn struct {
	domain  string
	host    string // Resolved backend IP/hostname
	port    int    // Resolved backend port
	token   string
	session *yamux.Session
	ws      *websocket.Conn
	stopCh  chan struct{}
}

// TunnelManager manages outbound tunnel connections to ODAC Cloud.
// This ODAC instance acts as the tunnel AGENT (client) — it connects
// to the cloud tunnel proxy and forwards incoming streams to local apps.
type TunnelManager struct {
	conns map[string]*tunnelConn // domain -> active connection
	mu    sync.RWMutex
}

// NewTunnelManager creates a tunnel manager for outbound cloud connections.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		conns: make(map[string]*tunnelConn),
	}
}

// tunnelBufPool reuses 64KB buffers for stream copying to reduce GC pressure.
var tunnelBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, tunnelCopyBufSize)
		return &buf
	},
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
			close(conn.stopCh)
			delete(tm.conns, domain)
		}
	}

	// Start new tunnels or update existing ones
	for domain, t := range incoming {
		if existing, ok := tm.conns[domain]; ok {
			// Token or backend changed — reconnect
			if existing.token != t.Token || existing.host != t.Host || existing.port != t.Port {
				log.Printf("[Tunnel] Config changed, reconnecting: %s", domain)
				close(existing.stopCh)
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
	for {
		select {
		case <-tc.stopCh:
			return
		default:
		}

		err := tm.connect(tc)
		if err != nil {
			log.Printf("[Tunnel] Connection failed for %s: %v", tc.domain, err)
		}

		// Wait before reconnecting (or exit if stopped)
		select {
		case <-tc.stopCh:
			return
		case <-time.After(tunnelReconnectInterval):
		}
	}
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

	url := tunnelWSURL + "?odac_ws_token=" + tc.token

	log.Printf("[Tunnel] Connecting to cloud for domain: %s", tc.domain)

	ws, _, err := dialer.Dial(url, header)
	if err != nil {
		return err
	}
	tc.ws = ws

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
		ws.Close()
		return err
	}
	tc.session = session

	// Accept streams from ODAC Cloud and forward to local app
	tm.acceptStreams(tc)

	// Cleanup
	session.Close()
	ws.Close()
	tc.session = nil
	tc.ws = nil

	log.Printf("[Tunnel] Disconnected from cloud for domain: %s", tc.domain)
	return nil
}

// acceptStreams continuously accepts yamux streams from ODAC Cloud
// and forwards each one to the local app as a raw TCP pipe.
func (tm *TunnelManager) acceptStreams(tc *tunnelConn) {
	for {
		stream, err := tc.session.AcceptStream()
		if err != nil {
			// Session closed or error — trigger reconnect
			select {
			case <-tc.stopCh:
				return
			default:
			}
			debugLog("[Tunnel] AcceptStream error for %s: %v", tc.domain, err)
			return
		}

		go handleStream(tc, stream)
	}
}

// handleStream pipes a single yamux stream to the local app backend.
// The stream carries raw HTTP/WebSocket bytes — zero parsing, pure relay.
// Uses pooled buffers, TCP_NODELAY, and proper half-close to prevent data loss.
func handleStream(tc *tunnelConn, stream net.Conn) {
	target := net.JoinHostPort(tc.host, strconv.Itoa(tc.port))

	backend, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		stream.Close()
		debugLog("[Tunnel] Failed to connect to backend %s for %s: %v", target, tc.domain, err)
		return
	}

	// Disable Nagle's algorithm for minimum latency on small writes
	if conn, ok := backend.(*net.TCPConn); ok {
		conn.SetNoDelay(true)
	}

	// Bidirectional raw byte pipe with proper half-close.
	// When one direction finishes (EOF), we half-close that side
	// so the other direction can finish flushing its remaining data.
	// This prevents data loss on large binary transfers (png, mp4).
	var wg sync.WaitGroup
	wg.Add(2)

	// Cloud → Backend (request direction)
	go func() {
		defer wg.Done()
		bp := tunnelBufPool.Get().(*[]byte)
		io.CopyBuffer(backend, stream, *bp)
		tunnelBufPool.Put(bp)
		// Half-close: signal backend that request is complete
		if conn, ok := backend.(*net.TCPConn); ok {
			conn.CloseWrite()
		}
	}()

	// Backend → Cloud (response direction)
	go func() {
		defer wg.Done()
		bp := tunnelBufPool.Get().(*[]byte)
		io.CopyBuffer(stream, backend, *bp)
		tunnelBufPool.Put(bp)
		// Half-close: signal cloud that response is complete
		stream.Close()
	}()

	wg.Wait()
	backend.Close()
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
	return len(p), c.ws.WriteMessage(websocket.BinaryMessage, p)
}

func (c *wsConn) Close() error { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr               { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return nil }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// Stop gracefully shuts down all active tunnel connections.
func (tm *TunnelManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for domain, conn := range tm.conns {
		log.Printf("[Tunnel] Stopping tunnel: %s", domain)
		close(conn.stopCh)
	}
	tm.conns = make(map[string]*tunnelConn)
}
