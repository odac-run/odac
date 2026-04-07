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

	// tunnelPingInterval is the WebSocket-level keepalive interval.
	tunnelPingInterval = 30 * time.Second
)

// tunnelConn represents a single active tunnel connection to ODAC Cloud
// for one domain. Each tunnel has its own WebSocket + yamux session.
type tunnelConn struct {
	domain  string
	token   string
	session *yamux.Session
	ws      *websocket.Conn
	stopCh  chan struct{}
}

// TunnelManager manages outbound tunnel connections to ODAC Cloud.
// This ODAC instance acts as the tunnel AGENT (client) — it connects
// to the cloud tunnel proxy and forwards incoming streams to local apps.
type TunnelManager struct {
	conns   map[string]*tunnelConn // domain -> active connection
	domains map[string]config.Website
	mu      sync.RWMutex
}

// NewTunnelManager creates a tunnel manager for outbound cloud connections.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		conns:   make(map[string]*tunnelConn),
		domains: make(map[string]config.Website),
	}
}

// UpdateConfig replaces the entire tunnel configuration (full-replace reconciliation).
// New tunnels are connected, removed tunnels are disconnected, unchanged tunnels are kept.
func (tm *TunnelManager) UpdateConfig(tunnels map[string]string, domains map[string]config.Website) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.domains = domains

	// Disconnect tunnels that were removed from config
	for domain, conn := range tm.conns {
		if _, exists := tunnels[domain]; !exists {
			log.Printf("[Tunnel] Domain removed, disconnecting: %s", domain)
			close(conn.stopCh)
			delete(tm.conns, domain)
		}
	}

	// Start new tunnels or update token for existing ones
	for domain, token := range tunnels {
		if existing, ok := tm.conns[domain]; ok {
			// Token changed — reconnect
			if existing.token != token {
				log.Printf("[Tunnel] Token changed, reconnecting: %s", domain)
				close(existing.stopCh)
				delete(tm.conns, domain)
			} else {
				continue // Already connected with same token
			}
		}

		conn := &tunnelConn{
			domain: domain,
			token:  token,
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
		HandshakeTimeout: 15 * time.Second,
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

		go tm.handleStream(tc.domain, stream)
	}
}

// handleStream pipes a single yamux stream to the local app backend.
// The stream carries raw HTTP/WebSocket bytes — zero parsing, pure relay.
func (tm *TunnelManager) handleStream(domain string, stream net.Conn) {
	defer stream.Close()

	// Resolve backend target for this domain
	tm.mu.RLock()
	website, exists := tm.domains[domain]
	tm.mu.RUnlock()

	if !exists || website.Port == 0 {
		debugLog("[Tunnel] No backend configured for domain: %s", domain)
		return
	}

	targetHost := "127.0.0.1"
	if website.ContainerIP != "" {
		targetHost = website.ContainerIP
	} else if website.Container != "" {
		targetHost = website.Container
	}

	target := net.JoinHostPort(targetHost, strconv.Itoa(website.Port))

	// Connect to local app
	backend, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		debugLog("[Tunnel] Failed to connect to backend %s for %s: %v", target, domain, err)
		return
	}
	defer backend.Close()

	// Bidirectional raw byte pipe: yamux stream <-> local app
	done := make(chan struct{})
	go func() {
		io.Copy(backend, stream)
		close(done)
	}()
	io.Copy(stream, backend)
	<-done
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

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
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
