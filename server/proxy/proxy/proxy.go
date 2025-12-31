package proxy

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac-proxy/config"
)

type Proxy struct {
	websites     map[string]config.Website
	sslCache     map[string]*tls.Certificate
	globalSSL    *config.SSL
	mu           sync.RWMutex
	reverseProxy *httputil.ReverseProxy
}

func NewProxy() *Proxy {
	p := &Proxy{
		websites: make(map[string]config.Website),
		sslCache: make(map[string]*tls.Certificate),
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	p.reverseProxy = &httputil.ReverseProxy{
		Director:     p.director,
		Transport:    transport,
		ErrorHandler: p.errorHandler,
	}

	return p
}

func (p *Proxy) UpdateConfig(websites map[string]config.Website, globalSSL *config.SSL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.websites = websites
	p.globalSSL = globalSSL
	p.sslCache = make(map[string]*tls.Certificate)
}

func (p *Proxy) director(req *http.Request) {
	host := req.Host
	if strings.Contains(host, ":") {
		host, _, _ = net.SplitHostPort(host)
	}

	p.mu.RLock()
	website, exists := p.resolveWebsite(host)
	p.mu.RUnlock()

	if !exists {
		return
	}

	targetIP := "127.0.0.1"
	targetPort := strconv.Itoa(website.Port)

	if website.ContainerIP != "" {
		targetIP = website.ContainerIP
		// Fix: Use internal container port (1071) instead of host-mapped port (60000+)
		// when communicating via Docker network IP
		targetPort = "1071"
	}
	
	// Important: req.URL.Scheme is often empty for incoming server requests
	req.URL.Scheme = "http"
	req.URL.Host = net.JoinHostPort(targetIP, targetPort)
	
	if _, ok := req.Header["User-Agent"]; !ok {
		// explicitly disable User-Agent so it's not set to default value
		req.Header.Set("User-Agent", "")
	}

	// Add ODAC headers
	remoteIP, _, err := net.SplitHostPort(req.RemoteAddr)
	if err == nil {
		req.Header.Set("X-Odac-Connection-RemoteAddress", remoteIP)
	}
	
	if req.TLS != nil {
		req.Header.Set("X-Odac-Connection-Ssl", "true")
	}

	if strings.ToLower(req.Header.Get("Connection")) == "upgrade" &&
		strings.ToLower(req.Header.Get("Upgrade")) == "websocket" {
		req.Header.Set("X-Odac-Websocket", "true")
	}
}

func (p *Proxy) resolveWebsite(host string) (config.Website, bool) {
	if site, ok := p.websites[host]; ok {
		return site, true
	}

	parts := strings.Split(host, ".")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[i:], ".")
		if site, ok := p.websites[parent]; ok {
			return site, true
		}
	}
	return config.Website{}, false
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if err == context.Canceled {
		return
	}
	// Ignore some errors like in Node.js
	if strings.Contains(err.Error(), "connection reset by peer") {
		return
	}
	
	log.Printf("Proxy error for %s: %v", r.Host, err)
	w.WriteHeader(http.StatusBadGateway)
	w.Write([]byte("Bad Gateway"))
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if strings.Contains(host, ":") {
		host, _, _ = net.SplitHostPort(host)
	}
	
	// Remove www.
	if strings.HasPrefix(host, "www.") {
		host = host[4:]
	}

	p.mu.RLock()
	_, exists := p.resolveWebsite(host)
	p.mu.RUnlock()

	if !exists {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Odac Server"))
		return
	}

	p.reverseProxy.ServeHTTP(w, r)
}

// GetCertificate implements tls.Config.GetCertificate
func (p *Proxy) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	log.Printf("[DEBUG] TLS Handshake for SNI: %s", host)

	if host == "" {
		log.Printf("[DEBUG] SNI is empty")
		return nil, nil // Fallback to default cert if any
	}
	
	p.mu.RLock()
	website, exists := p.resolveWebsite(host)
	// Check cache
	if cert, ok := p.sslCache[host]; ok {
		p.mu.RUnlock()
		log.Printf("[DEBUG] Found cached cert for %s", host)
		return cert, nil
	}
	p.mu.RUnlock()
	
	var certKey, certFile string
	var source string

	if !exists || website.Cert.SSL.Key == "" || website.Cert.SSL.Cert == "" {
		// Fallback to Global SSL
		if p.globalSSL != nil && p.globalSSL.Key != "" && p.globalSSL.Cert != "" {
			log.Printf("[DEBUG] Fallback to Global SSL for %s (Key: %s, Cert: %s)", host, p.globalSSL.Key, p.globalSSL.Cert)
			certKey = p.globalSSL.Key
			certFile = p.globalSSL.Cert
			source = "global"
		} else {
			log.Printf("[DEBUG] No cert found for %s and no global fallback available", host)
			return nil, nil // No specific cert found and no global fallback
		}
	} else {
		log.Printf("[DEBUG] Found specific cert for %s (Key: %s, Cert: %s)", host, website.Cert.SSL.Key, website.Cert.SSL.Cert)
		certKey = website.Cert.SSL.Key
		certFile = website.Cert.SSL.Cert
		source = "site"
	}

	// Load certificate
	p.mu.Lock()
	defer p.mu.Unlock()
	
	// Double check
	if cert, ok := p.sslCache[host]; ok {
		return cert, nil
	}
	
	log.Printf("[DEBUG] Loading cert files for %s from %s...", host, source)
	cert, err := tls.LoadX509KeyPair(certFile, certKey)
	if err != nil {
		log.Printf("[ERROR] Failed to load SSL for %s (Key: %s, Cert: %s): %v", host, certKey, certFile, err)
		return nil, err
	}
	
	p.sslCache[host] = &cert
	log.Printf("[DEBUG] Successfully loaded and cached cert for %s", host)
	return &cert, nil
}
