package proxy

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"odac-proxy/config"
)

const (
	// internalContainerPort is the port used for inter-container communication
	// when routing requests to Docker containers via their network IP
	internalContainerPort = "1071"
)

// Define buffer pools to reduce GC pressure and memory allocation
var (
	gzipPool = sync.Pool{
		New: func() interface{} {
			return gzip.NewWriter(io.Discard)
		},
	}
	brotliPool = sync.Pool{
		New: func() interface{} {
			return brotli.NewWriterLevel(io.Discard, 4)
		},
	}
)

// debugMode enables verbose debug logging when PROXY_DEBUG environment variable is set
var debugMode = os.Getenv("PROXY_DEBUG") != ""

func debugLog(format string, args ...interface{}) {
	if debugMode {
		log.Printf(format, args...)
	}
}

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
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // Timeout if backend server doesn't send headers in time
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
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Real-IP")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("Proxy")
	req.Header.Del("Client-IP")
	req.Header.Del("X-Remote-IP")
	req.Header.Del("X-Remote-Addr")
	req.Header.Del("X-Client-IP")
	req.Header.Del("X-Originating-IP")

	if req.Header.Get("X-Request-ID") == "" {
		uuid := make([]byte, 16)
		if _, err := rand.Read(uuid); err == nil {
			req.Header.Set("X-Request-ID", hex.EncodeToString(uuid))
		}
	}

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
		targetPort = internalContainerPort
	}
	
	req.URL.Scheme = "http"
	req.URL.Host = net.JoinHostPort(targetIP, targetPort)
	
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", "")
	}

	remoteIP, _, err := net.SplitHostPort(req.RemoteAddr)
	if err == nil {
		req.Header.Set("X-Odac-Connection-RemoteAddress", remoteIP)
		req.Header.Set("X-Real-IP", remoteIP)
	}
	
	if req.TLS != nil {
		req.Header.Set("X-Odac-Connection-Ssl", "true")
		req.Header.Set("X-Forwarded-Proto", "https")
	} else {
		req.Header.Set("X-Forwarded-Proto", "http")
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
	website, exists := p.resolveWebsite(host)
	// Check SSL availability (Site-specific or Global)
	hasSSL := (website.Cert.SSL.Key != "" && website.Cert.SSL.Cert != "") || 
			  (p.globalSSL != nil && p.globalSSL.Key != "" && p.globalSSL.Cert != "")
	p.mu.RUnlock()

	if !exists {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ODAC Server"))
		return
	}

	// Security: Force HTTPS if SSL is configured and available
	// Exception: Do not force HTTPS for IPs and localhost
	isIP := net.ParseIP(host) != nil
	isLocalhost := host == "localhost"

	if r.TLS == nil && hasSSL && !isIP && !isLocalhost {
		targetHost := r.Host
		if strings.Contains(targetHost, ":") {
			targetHost, _, _ = net.SplitHostPort(targetHost)
		}
		url := "https://" + targetHost + r.URL.RequestURI()
		http.Redirect(w, r, url, http.StatusMovedPermanently)
		return
	}

	// Compression negotiation
	acceptEncoding := r.Header.Get("Accept-Encoding")
	var encoding string

	// Prioritize Brotli
	if strings.Contains(acceptEncoding, "br") {
		encoding = "br"
	} else if strings.Contains(acceptEncoding, "gzip") {
		encoding = "gzip"
	}

	// Use compression wrapper if supported
	if encoding != "" {
		cw := newCompressionResponseWriter(w, encoding)
		// Ensure compressor is closed to flush remaining bytes and write footer
		defer cw.Close()

		p.reverseProxy.ServeHTTP(cw, r)
		return
	}

	p.reverseProxy.ServeHTTP(w, r)
}

// compressionResponseWriter wraps http.ResponseWriter to handle compression
type compressionResponseWriter struct {
	w              http.ResponseWriter
	encoding       string
	compressor     io.WriteCloser
	wroteHeader    bool
	shouldCompress bool
	code           int
}

func newCompressionResponseWriter(w http.ResponseWriter, encoding string) *compressionResponseWriter {
	return &compressionResponseWriter{
		w:        w,
		encoding: encoding,
	}
}

func (cw *compressionResponseWriter) Header() http.Header {
	return cw.w.Header()
}

func (cw *compressionResponseWriter) WriteHeader(code int) {
	if cw.wroteHeader {
		return
	}
	cw.wroteHeader = true
	cw.code = code

	// Avoid double compression if backend already compressed the response
	if cw.Header().Get("Content-Encoding") != "" {
		cw.w.WriteHeader(code)
		return
	}

	contentType := cw.Header().Get("Content-Type")
	contentLengthStr := cw.Header().Get("Content-Length")

	var contentLength int64 = -1
	if contentLengthStr != "" {
		contentLength, _ = strconv.ParseInt(contentLengthStr, 10, 64)
	}

	// Decide whether to compress based on type and size
	if isCompressible(contentType) && (contentLength == -1 || contentLength > 1024) {
		cw.shouldCompress = true

		cw.Header().Del("Content-Length")
		cw.Header().Set("Vary", "Accept-Encoding")

		if cw.encoding == "br" {
			cw.Header().Set("Content-Encoding", "br")
			
			// Get Brotli writer from pool
			w := brotliPool.Get().(*brotli.Writer)
			w.Reset(cw.w)
			cw.compressor = w
		} else if cw.encoding == "gzip" {
			cw.Header().Set("Content-Encoding", "gzip")
			
			// Get Gzip writer from pool
			w := gzipPool.Get().(*gzip.Writer)
			w.Reset(cw.w)
			cw.compressor = w
		}
	}

	cw.w.WriteHeader(code)
}

func (cw *compressionResponseWriter) Write(b []byte) (int, error) {
	if !cw.wroteHeader {
		cw.WriteHeader(http.StatusOK)
	}

	if cw.shouldCompress && cw.compressor != nil {
		return cw.compressor.Write(b)
	}
	return cw.w.Write(b)
}

func (cw *compressionResponseWriter) Flush() {
	if cw.shouldCompress && cw.compressor != nil {
		if f, ok := cw.compressor.(http.Flusher); ok {
			f.Flush()
		} else if f, ok := cw.compressor.(*gzip.Writer); ok {
			f.Flush()
		}
		
		if f, ok := cw.compressor.(*brotli.Writer); ok {
			f.Flush()
		}
	}
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *compressionResponseWriter) Close() error {
	if cw.shouldCompress && cw.compressor != nil {
		err := cw.compressor.Close()

		// Reset and return to pool
		if cw.encoding == "br" {
			if w, ok := cw.compressor.(*brotli.Writer); ok {
				w.Reset(io.Discard)
				brotliPool.Put(w)
			}
		} else if cw.encoding == "gzip" {
			if w, ok := cw.compressor.(*gzip.Writer); ok {
				w.Reset(io.Discard)
				gzipPool.Put(w)
			}
		}
		
		return err
	}
	return nil
}

// isCompressible checks if the content type is suitable for compression
func isCompressible(contentType string) bool {
	// Clean up content type
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = contentType[:idx]
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))

	// Allowlist
	switch contentType {
	case "text/html", "text/css", "text/javascript", "application/javascript",
		"application/json", "application/xml", "image/svg+xml", "text/plain":
		return true
	}

	// Check prefixes
	if strings.HasPrefix(contentType, "text/") || strings.HasPrefix(contentType, "font/") {
		return true
	}

	return false
}

// GetCertificate implements tls.Config.GetCertificate
func (p *Proxy) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	debugLog("[DEBUG] TLS Handshake for SNI: %s", host)

	if host == "" {
		debugLog("[DEBUG] SNI is empty")
		return nil, nil // Fallback to default cert if any
	}
	
	p.mu.RLock()
	website, exists := p.resolveWebsite(host)
	// Check cache
	if cert, ok := p.sslCache[host]; ok {
		p.mu.RUnlock()
		debugLog("[DEBUG] Found cached cert for %s", host)
		return cert, nil
	}
	p.mu.RUnlock()
	
	var certKey, certFile string
	var source string

	if !exists || website.Cert.SSL.Key == "" || website.Cert.SSL.Cert == "" {
		// Fallback to Global SSL
		if p.globalSSL != nil && p.globalSSL.Key != "" && p.globalSSL.Cert != "" {
			debugLog("[DEBUG] Fallback to Global SSL for %s (Key: %s, Cert: %s)", host, p.globalSSL.Key, p.globalSSL.Cert)
			certKey = p.globalSSL.Key
			certFile = p.globalSSL.Cert
			source = "global"
		} else {
			debugLog("[DEBUG] No cert found for %s and no global fallback available", host)
			return nil, nil // No specific cert found and no global fallback
		}
	} else {
		debugLog("[DEBUG] Found specific cert for %s (Key: %s, Cert: %s)", host, website.Cert.SSL.Key, website.Cert.SSL.Cert)
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
	
	debugLog("[DEBUG] Loading cert files for %s from %s...", host, source)
	cert, err := tls.LoadX509KeyPair(certFile, certKey)
	if err != nil {
		log.Printf("[ERROR] Failed to load SSL for %s (Key: %s, Cert: %s): %v", host, certKey, certFile, err)
		return nil, err
	}
	
	p.sslCache[host] = &cert
	debugLog("[DEBUG] Successfully loaded and cached cert for %s", host)
	return &cert, nil
}
