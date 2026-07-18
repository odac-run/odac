// ws.go ports WebSocketClient (server/src/Hub/WebSocket.js) over
// github.com/coder/websocket (dependency decision in contract 0.8):
// connect/disconnect, the post-close reconnect window (now + 5s +
// rand(0..15s)), and the 30s ping heartbeat that terminates the socket when
// the previous ping never got its pong.
package hub

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"odac/internal/logx"
)

const (
	heartbeatEvery = 30 * time.Second
	dialTimeout    = 30 * time.Second
	writeTimeout   = 30 * time.Second
)

type wsClient struct {
	log *logx.Logger
	now func() time.Time

	onConnect    func()
	onMessage    func(raw []byte)
	onDisconnect func()

	// dial is the test seam; the default dials the real endpoint with the
	// Authorization header and TLS verification (Node: rejectUnauthorized).
	dial      func(url, token string) (*websocket.Conn, error)
	pingEvery time.Duration

	mu            sync.Mutex
	conn          *websocket.Conn
	dialing       bool
	isAlive       bool
	nextReconnect time.Time
	closeOnce     *sync.Once
}

func newWSClient() *wsClient {
	c := &wsClient{
		log:       logx.New("Hub", "WebSocket"),
		now:       time.Now,
		pingEvery: heartbeatEvery,
	}
	c.dial = func(url, token string) (*websocket.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
		})
		return conn, err
	}
	return c
}

// connected mirrors the readyState===OPEN getter.
func (c *wsClient) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// shouldReconnect mirrors Date.now() >= nextReconnectTime.
func (c *wsClient) shouldReconnect() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.now().Before(c.nextReconnect)
}

// connect dials asynchronously (Node's `new WebSocket` returns a CONNECTING
// socket immediately; the 1s check tick must never block on a dial). A
// second connect while one is in flight logs like Node's existing-socket
// guard. A failed dial follows Node's error→close path: reconnect window
// scheduled, onDisconnect fired.
func (c *wsClient) connect(url, token string) {
	c.mu.Lock()
	if c.conn != nil || c.dialing {
		c.mu.Unlock()
		c.log.Log("WebSocket already connected")
		return
	}
	c.dialing = true
	c.mu.Unlock()

	c.log.Log("Connecting to WebSocket: %s", url)
	go func() {
		conn, err := c.dial(url, token)
		c.mu.Lock()
		c.dialing = false
		if err != nil {
			c.scheduleReconnectLocked()
			c.mu.Unlock()
			c.log.Log("WebSocket error: %s", err.Error())
			if c.onDisconnect != nil {
				c.onDisconnect()
			}
			return
		}
		// Terminal output can exceed the 32KiB default; the main socket
		// only carries JSON but command payloads (env blobs) can be large.
		conn.SetReadLimit(16 * 1024 * 1024)
		c.conn = conn
		c.isAlive = true
		once := &sync.Once{}
		c.closeOnce = once
		c.mu.Unlock()

		c.log.Log("WebSocket connected")
		go c.readLoop(conn, once)
		go c.heartbeat(conn, once)
		if c.onConnect != nil {
			c.onConnect()
		}
	}()
}

// disconnect ports disconnect(): a manual close still runs the close path
// (reconnect window + onDisconnect), exactly like Node where the 'close'
// event fires after socket.close(). Hub.stop() clears the active flag so
// check() never uses the window.
func (c *wsClient) disconnect() {
	c.mu.Lock()
	conn, once := c.conn, c.closeOnce
	c.mu.Unlock()
	if conn == nil {
		return
	}
	c.log.Log("Disconnecting WebSocket")
	conn.Close(websocket.StatusNormalClosure, "")
	// The read loop notices and runs handleClose; nothing else to do here.
	_ = once
}

// send writes one text frame; false when not connected (Node returns false
// and the message is dropped).
func (c *wsClient) send(payload []byte) bool {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		// Node dropped silently; keep the drop but not the silence — a
		// systematic write failure is otherwise invisible in the logs.
		c.log.Log("WebSocket write failed (%s bytes): %s", fmt.Sprint(len(payload)), err.Error())
		return false
	}
	return true
}

func (c *wsClient) readLoop(conn *websocket.Conn, once *sync.Once) {
	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			c.handleClose(conn, once, err)
			return
		}
		if c.onMessage != nil {
			c.onMessage(data)
		}
	}
}

// heartbeat ports #startHeartbeat: every 30s, a still-pending ping means the
// peer is gone — terminate (CloseNow) so the read loop runs the close path.
func (c *wsClient) heartbeat(conn *websocket.Conn, once *sync.Once) {
	t := time.NewTicker(c.pingEvery)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		gone := c.conn != conn
		alive := c.isAlive
		c.mu.Unlock()
		if gone {
			return
		}
		if !alive {
			c.log.Log("WebSocket connection dead (no pong), terminating...")
			conn.CloseNow()
			return
		}
		c.mu.Lock()
		c.isAlive = false
		c.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), c.pingEvery)
			defer cancel()
			if conn.Ping(ctx) == nil {
				c.mu.Lock()
				if c.conn == conn {
					c.isAlive = true
				}
				c.mu.Unlock()
			}
		}()
	}
}

func (c *wsClient) handleClose(conn *websocket.Conn, once *sync.Once, cause error) {
	once.Do(func() {
		conn.CloseNow()
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.scheduleReconnectLocked()
		c.mu.Unlock()
		// The cause distinguishes "peer sent close 1008: bad signature" from
		// a local read failure — without it a server-side rejection and a
		// dropped TCP conn log identically and are undebuggable.
		if cause != nil {
			if status := websocket.CloseStatus(cause); status != -1 {
				c.log.Log("WebSocket disconnected (close %s: %s)", fmt.Sprint(int(status)), cause.Error())
			} else {
				c.log.Log("WebSocket disconnected (%s)", cause.Error())
			}
		} else {
			c.log.Log("WebSocket disconnected")
		}
		if c.onDisconnect != nil {
			c.onDisconnect()
		}
	})
}

func (c *wsClient) scheduleReconnectLocked() {
	delay := 5*time.Second + time.Duration(rand.Int63n(15000))*time.Millisecond
	c.nextReconnect = c.now().Add(delay)
}
