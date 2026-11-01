package proxy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"odac/internal/proxy/config"
)

const h3TestHost = "h3.test"

var h3TestAsset = bytes.Repeat([]byte("var asset=1;\n"), 20000)

type h3Rig struct {
	proxy   *Proxy
	client  *http.Client
	baseURL string
	served  chan struct{} // receives once per request the proxy finished handling
}

func writeTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: h3TestHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{h3TestHost},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// dialH3 binds the client to loopback as well: a wildcard socket can land on the
// server's port number, and the server's replies then come back to the server itself.
func dialH3(ctx context.Context, t *testing.T, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { udp.Close() })
	remote, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	return quic.Dial(ctx, udp, remote, tlsCfg, cfg)
}

func newH3Rig(t *testing.T, upstream http.HandlerFunc) *h3Rig {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)

	certPath, keyPath := writeTestCert(t)
	p := NewProxy()
	p.UpdateConfig(map[string]config.Website{h3TestHost: {
		Domain: h3TestHost,
		Port:   backend.Listener.Addr().(*net.TCPAddr).Port,
		Cert:   config.Cert{SSL: config.SSL{Key: keyPath, Cert: certPath}},
	}}, nil, nil, nil)

	served := make(chan struct{}, 16)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- struct{}{} }()
		p.ServeHTTP(w, r)
	})

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTP3Server(":443", handler, &tls.Config{GetCertificate: p.GetCertificate, MinVersion: tls.VersionTLS13})
	go ServeHTTP3(srv, udp, nil)

	transport := &http3.Transport{
		DisableCompression: true,
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true, ServerName: h3TestHost},
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			return dialH3(ctx, t, udp.LocalAddr().String(), tlsCfg, cfg)
		},
	}
	t.Cleanup(func() {
		transport.Close()
		srv.Close()
		udp.Close()
	})

	return &h3Rig{
		proxy:   p,
		client:  &http.Client{Transport: transport, Timeout: 10 * time.Second},
		baseURL: "https://" + h3TestHost,
		served:  served,
	}
}

func (r *h3Rig) waitServed(t *testing.T) {
	t.Helper()
	select {
	case <-r.served:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy handler did not finish")
	}
}

func (r *h3Rig) assertNotCached(t *testing.T, path string) {
	t.Helper()
	if entry := r.proxy.Cache().Get(h3TestHost, path); entry != nil {
		t.Fatalf("incomplete response was cached: %d of %d bytes", entry.size, len(h3TestAsset))
	}
}

func TestHTTP3UpstreamFailureAbortsResponseAndSkipsCache(t *testing.T) {
	upstream := func(withLength bool) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			if withLength {
				w.Header().Set("Content-Length", strconv.Itoa(len(h3TestAsset)))
			}
			w.Write(h3TestAsset[:len(h3TestAsset)/4])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	}

	cases := []struct {
		name       string
		withLength bool
		encoding   string
	}{
		{name: "declared length", withLength: true},
		{name: "chunked"},
		// Compression drops Content-Length and writes a valid footer, so the
		// stream reset is the only thing that tells the client the body is short.
		{name: "chunked gzip", encoding: "gzip"},
		{name: "declared length gzip", withLength: true, encoding: "gzip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newH3Rig(t, upstream(tc.withLength))
			req, _ := http.NewRequest(http.MethodGet, rig.baseURL+"/app.js", nil)
			if tc.encoding != "" {
				req.Header.Set("Accept-Encoding", tc.encoding)
			}
			resp, err := rig.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Error("truncated response reached the client as a clean end of stream")
			}
			rig.waitServed(t)
			rig.assertNotCached(t, "/app.js")
		})
	}
}

func TestHTTP3ClientAbortSkipsCache(t *testing.T) {
	chunk := h3TestAsset[:16<<10]
	rig := newH3Rig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("ETag", `"v1"`)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rig.baseURL+"/app.js", nil)
	resp, err := rig.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	cancel()

	rig.waitServed(t)
	rig.assertNotCached(t, "/app.js")
}

func TestHTTP3CompleteResponseIsCached(t *testing.T) {
	rig := newH3Rig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(h3TestAsset)
	})

	resp, err := rig.client.Get(rig.baseURL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, h3TestAsset) {
		t.Fatalf("client received %d of %d bytes", len(body), len(h3TestAsset))
	}

	rig.waitServed(t)
	entry := rig.proxy.Cache().Get(h3TestHost, "/app.js")
	if entry == nil {
		t.Fatal("complete response was not cached")
	}
	if !bytes.Equal(entry.body, h3TestAsset) {
		t.Fatalf("cached %d of %d bytes", entry.size, len(h3TestAsset))
	}
}

func h3TestServer(t *testing.T, handler http.Handler) *http3.Server {
	t.Helper()
	certPath, keyPath := writeTestCert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return NewHTTP3Server(":443", handler, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
}

func h3ClientTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, ServerName: h3TestHost, NextProtos: []string{http3.NextProtoH3}}
}

// rebindUDP retries because the old socket's read loop can hold the port briefly after Close.
func rebindUDP(t *testing.T, addr string) net.PacketConn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		udp, err := net.ListenPacket("udp", addr)
		if err == nil {
			return udp
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTP3RestartResetsStaleConnections(t *testing.T) {
	newKey := func() *quic.StatelessResetKey {
		key := &quic.StatelessResetKey{}
		rand.Read(key[:])
		return key
	}
	key := newKey()

	cases := []struct {
		name      string
		restartAs *quic.StatelessResetKey
		wantReset bool
	}{
		{name: "same key", restartAs: key, wantReset: true},
		// The client must ignore a reset it cannot verify, so this is what a
		// key regenerated on every start would look like.
		{name: "regenerated key", restartAs: newKey()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.NewServeMux()
			udp, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := udp.LocalAddr().String()
			go ServeHTTP3(h3TestServer(t, handler), udp, key)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := dialH3(ctx, t, addr, h3ClientTLS(), &quic.Config{MaxIdleTimeout: 30 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseWithError(0, "")

			// A crash sends no CONNECTION_CLOSE, closing the socket first keeps it that way.
			udp.Close()
			restarted := rebindUDP(t, addr)
			defer restarted.Close()
			go ServeHTTP3(h3TestServer(t, handler), restarted, tc.restartAs)

			str, err := conn.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := str.Write(make([]byte, 512)); err != nil {
				t.Fatal(err)
			}

			wait := time.Second
			if tc.wantReset {
				wait = 5 * time.Second
			}
			select {
			case <-conn.Context().Done():
				var reset *quic.StatelessResetError
				if !errors.As(context.Cause(conn.Context()), &reset) {
					t.Fatalf("connection ended with %v, not a stateless reset", context.Cause(conn.Context()))
				}
				if !tc.wantReset {
					t.Fatal("client accepted a reset signed with a different key")
				}
			case <-time.After(wait):
				if tc.wantReset {
					t.Fatalf("client still waiting %v after the restart", wait)
				}
			}
		})
	}
}

func TestHTTP3ShutdownFinishesInFlightAndRefusesNewConnections(t *testing.T) {
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(h3TestAsset[:1024])
		w.(http.Flusher).Flush()
		<-release
		w.Write(h3TestAsset[1024:])
	})
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addr := udp.LocalAddr().String()

	srv := h3TestServer(t, handler)
	served := make(chan error, 1)
	go func() { served <- ServeHTTP3(srv, udp, nil) }()

	transport := &http3.Transport{
		TLSClientConfig: h3ClientTLS(),
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			return dialH3(ctx, t, addr, tlsCfg, cfg)
		},
	}
	defer transport.Close()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	resp, err := client.Get("https://" + h3TestHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Shutdown(ctx) }()

	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ServeHTTP3 returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP3 did not return after Shutdown")
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer dialCancel()
	if conn, err := dialH3(dialCtx, t, addr, h3ClientTLS(), nil); err == nil {
		conn.CloseWithError(0, "")
		t.Error("a server shutting down accepted a new connection")
	}

	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("in-flight response failed: %v", err)
	}
	if len(body) != len(h3TestAsset) {
		t.Fatalf("in-flight response got %d of %d bytes", len(body), len(h3TestAsset))
	}

	transport.Close()
	if err := <-stopped; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestLoadStatelessResetKeyPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")
	path := filepath.Join(dir, "reset.key")

	first, err := LoadStatelessResetKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadStatelessResetKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if *first != *second {
		t.Fatal("key changed between loads")
	}
	if *first == (quic.StatelessResetKey{}) {
		t.Fatal("key is all zeroes")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode is %o", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the key file, found %d entries", len(entries))
	}
}

func TestLoadStatelessResetKeyReplacesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reset.key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := LoadStatelessResetKey(path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, key[:]) {
		t.Fatal("replacement key was not persisted")
	}
}

func TestLoadStatelessResetKeyReportsReadFailure(t *testing.T) {
	// A directory fails to read with something other than "not found".
	path := t.TempDir()
	if _, err := LoadStatelessResetKey(path); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
