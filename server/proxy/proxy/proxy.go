package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
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
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/ocsp"

	"odac-proxy/config"
)

const (
	// internalContainerPort is the port used for inter-container communication
	// when routing requests to Docker containers via their network IP
	internalContainerPort = "1071"

	// proxyBufferSize is the buffer size for proxying request/response bodies.
	// 32KB is the Go default for io.Copy, but we pool these to achieve zero-allocation.
	proxyBufferSize = 32 * 1024
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
	zstdPool = sync.Pool{
		New: func() interface{} {
			w, _ := zstd.NewWriter(io.Discard, zstd.WithEncoderLevel(zstd.SpeedDefault))
			return w
		},
	}

	// proxyBufferPool is used by httputil.ReverseProxy for zero-allocation proxying.
	// This dramatically reduces GC pressure under high concurrency (10K+ connections).
	proxyBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, proxyBufferSize)
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

// bufferPool implements httputil.BufferPool interface for zero-allocation proxying.
// This is used by ReverseProxy to reuse buffers instead of allocating new ones per request.
type bufferPool struct{}

func (bufferPool) Get() []byte {
	return proxyBufferPool.Get().([]byte)
}

func (bufferPool) Put(buf []byte) {
	proxyBufferPool.Put(buf)
}

type Proxy struct {
	websites     map[string]config.Website
	sslCache     map[string]*tls.Certificate
	ocspCache    map[string]*ocspCacheEntry // OCSP response cache
	globalSSL    *config.SSL
	mu           sync.RWMutex
	reverseProxy *httputil.ReverseProxy
	httpClient   *http.Client // For OCSP requests
}

// ocspCacheEntry stores OCSP response with expiration
type ocspCacheEntry struct {
	response  []byte
	expiresAt time.Time
}

func NewProxy() *Proxy {
	p := &Proxy{
		websites:  make(map[string]config.Website),
		sslCache:  make(map[string]*tls.Certificate),
		ocspCache: make(map[string]*ocspCacheEntry),
		httpClient: &http.Client{
			Timeout: 5 * time.Second, // OCSP requests should be fast
		},
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		// MaxIdleConns: 10000 ensures we can reuse many connections in high-throughput scenarios
		// (Performance > Memory for this Enterprise Proxy)
		MaxIdleConns:          10000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // Timeout if backend server doesn't send headers in time
	}

	p.reverseProxy = &httputil.ReverseProxy{
		Director:     p.director,
		Transport:    transport,
		BufferPool:   bufferPool{}, // Zero-allocation: reuse buffers from pool
		ErrorHandler: p.errorHandler,
		ModifyResponse: func(r *http.Response) error {
			// Branding: Always force "ODAC" as the server header (User Rule: Server cannot be changed by upstream)
			r.Header.Set("Server", "ODAC")
			// Advertise HTTP/3 (QUIC) support
			r.Header.Set("Alt-Svc", `h3=":443"; ma=2592000`)

			// Security Headers: Apply defaults only if upstream didn't set them
			// This allows apps to override these (e.g. allowing iframes via X-Frame-Options)
			if r.Header.Get("X-Frame-Options") == "" {
				r.Header.Set("X-Frame-Options", "SAMEORIGIN")
			}
			if r.Header.Get("X-Content-Type-Options") == "" {
				r.Header.Set("X-Content-Type-Options", "nosniff")
			}
			if r.Header.Get("X-XSS-Protection") == "" {
				r.Header.Set("X-XSS-Protection", "1; mode=block")
			}
			if r.Header.Get("Referrer-Policy") == "" {
				r.Header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			}

			// Security: Minimize information leakage from frameworks
			r.Header.Del("X-Powered-By")
			r.Header.Del("X-AspNet-Version")
			r.Header.Del("X-Runtime")
			return nil
		},
	}

	return p
}

func (p *Proxy) UpdateConfig(websites map[string]config.Website, globalSSL *config.SSL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.websites = websites
	p.globalSSL = globalSSL
	p.sslCache = make(map[string]*tls.Certificate)
	p.ocspCache = make(map[string]*ocspCacheEntry) // Clear OCSP cache on config update
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

	// 0-RTT (Early Data) Security Check
	// If the TLS handshake is not complete, this request was sent via 0-RTT (Early Data).
	// RFC 8446 states that 0-RTT data is subject to Replay Attacks.
	// Therefore, we MUST NOT allow non-idempotent methods (like POST, PUT, DELETE) over 0-RTT.
	if r.TLS != nil && !r.TLS.HandshakeComplete {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods are allowed in 0-RTT
		default:
			// Risky methods MUST be rejected or retried with confirmed handshake (1-RTT)
			// HTTP 425 (Too Early) tells the client to retry after handshake completion.
			w.WriteHeader(http.StatusTooEarly)
			return
		}
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

	// Security: Strict Host Validation
	// If the host is not in our configuration, drop the connection immediately (Status 444).
	// This prevents IP-based scanners from fingerprinting the server.
	if !exists {
		debugLog("[SECURITY] Unknown host '%s' - rejecting request (anti-scan)", host)
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		http.Error(w, "", http.StatusNotFound)
		return
	}

	// Security: Prevent Domain Fronting (SNI Mismatch)
	// If TLS SNI exists but Host header differs significantly, it might be an attack.
	// We allow case-insensitive match.
	if r.TLS != nil && r.TLS.ServerName != "" && !strings.EqualFold(r.TLS.ServerName, host) {
		debugLog("[SECURITY] Host header '%s' mismatch with SNI '%s', possible domain fronting", host, r.TLS.ServerName)
		// Option: Return 421 Misdirected Request, or just 403.
		// For strict security, we should reject.
		// However, to support wildcard certs nicely, we just log for now as 'website' resolution handled wildcard logic.
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

	// Security Headers
	// Note: Other security headers (X-Frame-Options, etc.) are handled in ModifyResponse
	// to allow upstream overrides. HSTS is forced here because we handle SSL termination.

	// HSTS: Only send on HTTPS and valid domains (not IP or localhost)
	if r.TLS != nil && !isIP && !isLocalhost {
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
	}

	// Compression negotiation
	acceptEncoding := r.Header.Get("Accept-Encoding")
	var encoding string

	var wantsZstd, wantsBr, wantsGzip bool
	for _, v := range strings.Split(acceptEncoding, ",") {
		v = strings.TrimSpace(v)
		parts := strings.SplitN(v, ";", 2)
		enc := parts[0]

		// Respect q=0, which means "not acceptable"
		if len(parts) > 1 && strings.Contains(parts[1], "q=0") {
			continue
		}

		switch enc {
		case "zstd":
			wantsZstd = true
		case "br":
			wantsBr = true
		case "gzip":
			wantsGzip = true
		}
	}

	// Priority: zstd > br > gzip
	if wantsZstd {
		encoding = "zstd"
	} else if wantsBr {
		encoding = "br"
	} else if wantsGzip {
		encoding = "gzip"
	}

	// Skip compression for WebSocket connections
	isWebSocket := strings.EqualFold(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

	// Use compression wrapper if supported (skip for WebSocket)
	if encoding != "" && !isWebSocket {
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

	// Skip compression for SSE (Server-Sent Events) streams
	isSSE := strings.HasPrefix(contentType, "text/event-stream")

	// Decide whether to compress based on type and size (skip for SSE)
	if isCompressible(contentType) && !isSSE && (contentLength == -1 || contentLength > 1024) {
		cw.shouldCompress = true

		cw.Header().Del("Content-Length")
		cw.Header().Set("Vary", "Accept-Encoding")

		switch cw.encoding {
		case "zstd":
			cw.Header().Set("Content-Encoding", "zstd")
			w := zstdPool.Get().(*zstd.Encoder)
			w.Reset(cw.w)
			cw.compressor = w
		case "br":
			cw.Header().Set("Content-Encoding", "br")
			w := brotliPool.Get().(*brotli.Writer)
			w.Reset(cw.w)
			cw.compressor = w
		case "gzip":
			cw.Header().Set("Content-Encoding", "gzip")
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
		switch w := cw.compressor.(type) {
		case *zstd.Encoder:
			w.Flush()
		case *brotli.Writer:
			w.Flush()
		case *gzip.Writer:
			w.Flush()
		}
	}
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *compressionResponseWriter) Close() error {
	if !cw.shouldCompress || cw.compressor == nil {
		return nil
	}

	err := cw.compressor.Close()

	// Reset and return to pool
	switch w := cw.compressor.(type) {
	case *zstd.Encoder:
		w.Reset(io.Discard)
		zstdPool.Put(w)
	case *brotli.Writer:
		w.Reset(io.Discard)
		brotliPool.Put(w)
	case *gzip.Writer:
		w.Reset(io.Discard)
		gzipPool.Put(w)
	}

	return err
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

// GetCertificate implements tls.Config.GetCertificate with OCSP Stapling support.
// OCSP Stapling eliminates the need for clients to contact the CA's OCSP responder,
// reducing handshake latency by ~100ms and improving privacy.
//
// Security: Strict SNI validation is enforced to prevent domain discovery attacks.
// Requests without SNI or with unknown SNI are rejected to prevent IP-based scanning
// (Shodan, Censys, etc.) from discovering which domains are hosted on this server.
func (p *Proxy) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	debugLog("[DEBUG] TLS Handshake for SNI: %s", host)

	// Security: Reject empty SNI to prevent IP-based domain discovery
	// Scanners like Shodan/Censys probe IPs directly without SNI
	if host == "" {
		debugLog("[DEBUG] SNI is empty - rejecting connection (anti-scan protection)")
		return nil, nil // Returns TLS alert: unrecognized_name
	}

	p.mu.RLock()
	website, exists := p.resolveWebsite(host)

	// Security: Reject unknown SNI to prevent domain enumeration
	// Only serve certificates for explicitly configured domains
	if !exists {
		p.mu.RUnlock()
		debugLog("[DEBUG] Unknown SNI '%s' - rejecting connection (anti-scan protection)", host)
		return nil, nil // Returns TLS alert: unrecognized_name
	}

	// Check cache
	if cert, ok := p.sslCache[host]; ok {
		// Check if OCSP response needs refresh (background refresh)
		if entry, hasOCSP := p.ocspCache[host]; hasOCSP {
			if time.Now().After(entry.expiresAt.Add(-1 * time.Hour)) {
				// Refresh in background if expiring within 1 hour
				go p.refreshOCSPStaple(host, cert)
			}
		}
		p.mu.RUnlock()
		debugLog("[DEBUG] Found cached cert for %s", host)
		return cert, nil
	}
	p.mu.RUnlock()

	var certKey, certFile string
	var source string

	// Use site-specific cert or fallback to global for KNOWN websites only
	if website.Cert.SSL.Key != "" && website.Cert.SSL.Cert != "" {
		debugLog("[DEBUG] Found specific cert for %s (Key: %s, Cert: %s)", host, website.Cert.SSL.Key, website.Cert.SSL.Cert)
		certKey = website.Cert.SSL.Key
		certFile = website.Cert.SSL.Cert
		source = "site"
	} else if p.globalSSL != nil && p.globalSSL.Key != "" && p.globalSSL.Cert != "" {
		// Global SSL fallback - only for known websites without specific certs
		debugLog("[DEBUG] Using Global SSL for known website %s", host)
		certKey = p.globalSSL.Key
		certFile = p.globalSSL.Cert
		source = "global"
	} else {
		debugLog("[DEBUG] No cert available for known website %s", host)
		return nil, nil
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

	// Fetch OCSP staple (non-blocking, best-effort)
	p.fetchAndStapleOCSP(host, &cert)

	p.sslCache[host] = &cert
	debugLog("[DEBUG] Successfully loaded and cached cert for %s", host)
	return &cert, nil
}

// fetchAndStapleOCSP fetches OCSP response and attaches it to the certificate.
// This is a best-effort operation - if it fails, the certificate works without stapling.
// fetchAndStapleOCSP fetches OCSP response and attaches it to the certificate.
// It assumes p.mu is held by the caller.
func (p *Proxy) fetchAndStapleOCSP(host string, cert *tls.Certificate) {
	ocspResp, nextUpdate, err := p.fetchOCSP(host, cert)
	if err != nil {
		debugLog("[DEBUG] OCSP: Fetch failed for %s: %v", host, err)
		return
	}

	// Staple the response
	cert.OCSPStaple = ocspResp

	// Cache with expiration
	p.ocspCache[host] = &ocspCacheEntry{
		response:  ocspResp,
		expiresAt: *nextUpdate,
	}

	debugLog("[DEBUG] OCSP: Successfully stapled for %s (valid until %s)", host, nextUpdate.Format(time.RFC3339))
}

// fetchOCSP performs the network request and parsing for OCSP.
// It is safe to call without holding p.mu.
func (p *Proxy) fetchOCSP(host string, cert *tls.Certificate) ([]byte, *time.Time, error) {
	if len(cert.Certificate) < 2 {
		return nil, nil, fmt.Errorf("certificate chain too short")
	}

	// Parse leaf and issuer certificates
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse leaf cert: %v", err)
	}

	issuer, err := x509.ParseCertificate(cert.Certificate[1])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse issuer cert: %v", err)
	}

	if len(leaf.OCSPServer) == 0 {
		return nil, nil, fmt.Errorf("no OCSP server URL in cert")
	}

	ocspURL := leaf.OCSPServer[0]
	// debugLog("[DEBUG] OCSP: Fetching staple from %s for %s", ocspURL, host) // Optional: avoid spamming logs

	// Create OCSP request
	ocspReq, err := ocsp.CreateRequest(leaf, issuer, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Send POST request (more reliable than GET for large requests)
	resp, err := p.httpClient.Post(ocspURL, "application/ocsp-request", bytes.NewReader(ocspReq))
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("bad status %d", resp.StatusCode)
	}

	ocspResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response: %v", err)
	}

	// Parse and validate OCSP response
	parsedResp, err := ocsp.ParseResponse(ocspResp, issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse response: %v", err)
	}

	if parsedResp.Status != ocsp.Good {
		return nil, nil, fmt.Errorf("certificate status is not good: %d", parsedResp.Status)
	}

	return ocspResp, &parsedResp.NextUpdate, nil
}

// refreshOCSPStaple refreshes OCSP staple in background when nearing expiration.
func (p *Proxy) refreshOCSPStaple(host string, cert *tls.Certificate) {
	// Performance: Do network I/O WITHOUT holding the lock
	ocspResp, nextUpdate, err := p.fetchOCSP(host, cert)
	if err != nil {
		debugLog("[DEBUG] OCSP: Update failed for %s: %v", host, err)
		return
	}

	// Safety: Clone the certificate struct to avoid Data Race.
	// We only modify OCSPStaple (slice header), so shallow copy is safe.
	// The active connections will continue to use the old pointer.
	newCert := *cert
	newCert.OCSPStaple = ocspResp

	p.mu.Lock()
	defer p.mu.Unlock()

	// Update caches with the new pointer
	p.sslCache[host] = &newCert
	p.ocspCache[host] = &ocspCacheEntry{
		response:  ocspResp,
		expiresAt: *nextUpdate,
	}
	debugLog("[DEBUG] OCSP: Refreshed staple for %s", host)
}
