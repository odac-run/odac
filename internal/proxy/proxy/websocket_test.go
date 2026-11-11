package proxy

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketUpgradeAcceptsConnectionTokenList(t *testing.T) {
	upgrader := websocket.Upgrader{}
	flagged := make(chan string, 4)
	p, _ := newAltSvcProxy(t, func(w http.ResponseWriter, r *http.Request) {
		flagged <- r.Header.Get("X-Odac-Websocket")
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.Close()
	})
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	for name, connection := range map[string]string{
		"chrome":  "Upgrade",
		"firefox": "keep-alive, Upgrade",
	} {
		for _, path := range []string{"/ws", "/ws?x=1"} {
			conn, err := net.Dial("tcp", front.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			req := "GET " + path + " HTTP/1.1\r\nHost: " + altSvcTestHost + "\r\n" +
				"Connection: " + connection + "\r\nUpgrade: websocket\r\n" +
				"Accept-Encoding: gzip, br\r\nSec-WebSocket-Version: 13\r\n" +
				"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			status, err := bufio.NewReader(conn).ReadString('\n')
			conn.Close()
			if err != nil || !strings.Contains(status, "101") {
				t.Errorf("%s %s: status %q err %v", name, path, status, err)
				continue
			}
			if got := <-flagged; got != "true" {
				t.Errorf("%s %s: X-Odac-Websocket = %q", name, path, got)
			}
		}
	}
}
