package proxy

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTunnelClosedDuringDialDoesNotStayConnected(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	closed := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("odac_ws_token") != "t&k" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		reached <- struct{}{}
		<-release
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				closed <- err
				return
			}
		}
	}))
	t.Cleanup(cloud.Close)

	tm := NewTunnelManager()
	tm.endpoint = "ws" + strings.TrimPrefix(cloud.URL, "http") + "/_odac/ws"
	tc := &tunnelConn{domain: "a.test", host: "127.0.0.1", port: 1, token: "t&k", stopCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		tm.runTunnel(tc)
		close(done)
	}()

	<-reached
	tm.closeTunnel(tc)
	close(release)

	select {
	case err := <-closed:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("the removed tunnel stayed connected")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cloud side never saw the connection end")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runTunnel kept running after closeTunnel")
	}
	tm.closeTunnel(tc) // a second close must be harmless
}
