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
	go srv.Serve(udp)

	transport := &http3.Transport{
		DisableCompression: true,
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true, ServerName: h3TestHost},
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			return quic.DialAddrEarly(ctx, udp.LocalAddr().String(), tlsCfg, cfg)
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
