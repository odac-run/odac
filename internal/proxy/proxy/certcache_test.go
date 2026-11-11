package proxy

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"testing"

	"odac/internal/proxy/config"
)

func newCertProxy(t *testing.T) (*Proxy, string) {
	t.Helper()
	certPath, keyPath := writeTestCert(t)
	p := NewProxy()
	t.Cleanup(p.Cache().Stop)
	p.UpdateConfig(map[string]config.Website{h3TestHost: {
		Domain: h3TestHost,
		Cert:   config.Cert{SSL: config.SSL{Key: keyPath, Cert: certPath}},
	}}, nil, nil, nil)
	return p, certPath
}

func TestCertificateCacheIgnoresTheClientsName(t *testing.T) {
	p, _ := newCertProxy(t)

	var wg sync.WaitGroup
	certs := make([]*tls.Certificate, 500)
	for i := range certs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cert, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("r%d.a.b.%s", i, h3TestHost)})
			if err != nil || cert == nil {
				t.Errorf("handshake %d: cert=%v err=%v", i, cert, err)
			}
			certs[i] = cert
		}()
	}
	wg.Wait()

	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.sslCache) != 1 {
		t.Fatalf("sslCache holds %d entries for one key pair", len(p.sslCache))
	}
	for i, c := range certs {
		if c == nil || len(c.Certificate) == 0 {
			t.Fatalf("handshake %d got an empty certificate", i)
		}
	}
}

func TestCertificateLoadOvertakenByPushIsNotCached(t *testing.T) {
	p, _ := newCertProxy(t)

	p.mu.RLock()
	stale := p.certGen
	p.mu.RUnlock()
	p.UpdateConfig(p.domains, nil, nil, nil)

	id := certID{}
	p.mu.RLock()
	site := p.domains[h3TestHost]
	p.mu.RUnlock()
	id.cert, id.key = site.Cert.SSL.Cert, site.Cert.SSL.Key

	if cert, err := p.loadCertificate(id, stale); err != nil || cert == nil {
		t.Fatalf("load: cert=%v err=%v", cert, err)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, ok := p.sslCache[id]; ok {
		t.Fatal("a load that started before a config push was cached over the push")
	}
}

func TestCertificateLoadFailureIsNotCached(t *testing.T) {
	p, certPath := newCertProxy(t)
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: h3TestHost}); err == nil {
		t.Fatal("a missing certificate file loaded")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.sslCache) != 0 || len(p.certCalls) != 0 {
		t.Fatalf("failed load left state behind: cache=%d calls=%d", len(p.sslCache), len(p.certCalls))
	}
}
