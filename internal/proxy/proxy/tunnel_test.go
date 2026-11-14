package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"odac/internal/proxy/config"
	"odac/internal/proxysig"
)

func TestTunnelClosedDuringDialDoesNotStayConnected(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	closed := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t&k" || r.URL.RawQuery != "" {
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

func TestJitterStaysWithinHalfToFull(t *testing.T) {
	for range 1000 {
		if d := jitter(10 * time.Second); d < 5*time.Second || d >= 10*time.Second {
			t.Fatalf("jitter(10s) = %v, want [5s, 10s)", d)
		}
	}
}

// fakeCloud stands in for ODAC Cloud: it accepts the agent's WebSocket and hands
// back the yamux session it opens streams on.
func fakeCloud(t *testing.T) (endpoint string, sessions <-chan *yamux.Session) {
	t.Helper()
	ch := make(chan *yamux.Session, 1)
	upgrader := websocket.Upgrader{}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = io.Discard
		s, err := yamux.Server(newWSConn(c), cfg)
		if err != nil {
			c.Close()
			return
		}
		ch <- s
		<-s.CloseChan()
	}))
	t.Cleanup(cloud.Close)
	return "ws" + strings.TrimPrefix(cloud.URL, "http") + "/_odac/ws", ch
}

// tunnelTo connects an agent for app and returns a client whose requests reach
// it the way the Cloud sends them.
func tunnelTo(t *testing.T, key string, app http.HandlerFunc) *http.Client {
	t.Helper()
	backend := httptest.NewServer(app)
	t.Cleanup(backend.Close)
	endpoint, sessions := fakeCloud(t)

	tm := NewTunnelManager()
	tm.endpoint = endpoint
	t.Cleanup(tm.Stop)
	tm.UpdateConfig([]config.Tunnel{{
		Domain: "t.test", Host: "127.0.0.1", Port: backend.Listener.Addr().(*net.TCPAddr).Port, Token: "tok", Key: key,
	}})

	var session *yamux.Session
	select {
	case session = <-sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never connected")
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return session.OpenStream() },
	}}
}

// What the Cloud says about the visitor reaches the app signed, so the
// app trusts a tunnel the way it trusts the proxy.
func TestTunnelSignsWhatTheCloudSaysAboutTheVisitor(t *testing.T) {
	type seen struct {
		header    http.Header
		host, uri string
	}
	got := make(chan seen, 1)
	client := tunnelTo(t, "app-key", func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Clone(), r.Host, r.RequestURI}
	})

	req, _ := http.NewRequest(http.MethodPut, "http://t.test/a%2Fb?x=1;y=2", nil)
	req.Header.Set(proxysig.HeaderRemoteAddress, "198.51.100.7")
	req.Header.Set(proxysig.HeaderSSL, "true")
	req.Header.Set(proxysig.HeaderSignature, "t=1,v1=signed-upstream")
	req.Header.Set("X-Real-IP", "192.0.2.1")
	req.Header.Set("X-Request-ID", "cloud-id")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	s := <-got
	if s.uri != "/a%2Fb?x=1;y=2" || s.host != "t.test" {
		t.Fatalf("app saw %s %s", s.host, s.uri)
	}
	for name, want := range map[string]string{
		proxysig.HeaderRemoteAddress: "198.51.100.7",
		proxysig.HeaderSSL:           "true",
		"X-Real-IP":                  "198.51.100.7",
		"X-Forwarded-For":            "198.51.100.7",
		"X-Forwarded-Proto":          "https",
		"X-Request-ID":               "cloud-id",
	} {
		if v := s.header.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	sig := s.header.Get(proxysig.HeaderSignature)
	ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
	unix, _ := strconv.ParseInt(ts, 10, 64)
	want := proxysig.Connection{
		Host: "t.test", Method: http.MethodPut, RemoteAddress: "198.51.100.7", SSL: true, Timestamp: unix, URI: s.uri,
	}.Sign("app-key")
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
}

func TestTunnelDropsAnAddressThatIsNotOne(t *testing.T) {
	got := make(chan http.Header, 1)
	client := tunnelTo(t, "", func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	})
	req, _ := http.NewRequest(http.MethodGet, "http://t.test/", nil)
	req.Header.Set(proxysig.HeaderRemoteAddress, "198.51.100.7, 192.0.2.1")
	req.Header.Set(proxysig.HeaderSignature, "t=1,v1=signed-upstream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := <-got
	for _, name := range proxysig.Headers {
		if v, ok := h[name]; ok {
			t.Errorf("%s reached the app as %q", name, v)
		}
	}
}

func TestTunnelCarriesWebSockets(t *testing.T) {
	upgrader := websocket.Upgrader{}
	client := tunnelTo(t, "app-key", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Odac-Websocket") != "true" {
			http.Error(w, "not marked", http.StatusBadRequest)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		mt, msg, err := c.ReadMessage()
		if err == nil {
			c.WriteMessage(mt, append([]byte("echo "), msg...))
		}
	})
	dialer := websocket.Dialer{NetDialContext: client.Transport.(*http.Transport).DialContext}
	c, _, err := dialer.Dial("ws://t.test/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := c.ReadMessage(); err != nil || string(msg) != "echo hi" {
		t.Fatalf("read %q, %v", msg, err)
	}
}
