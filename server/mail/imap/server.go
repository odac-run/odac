// Package imap implements a native IMAP4rev1 server (RFC 3501).
// Uses a goroutine-per-connection model with TLS support and SNI callback.
// Mirrors the Node.js mail/server.js + mail/imap.js architecture.
package imap

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"odac-mail/auth"
	"odac-mail/config"
	"odac-mail/storage"
)

// Server manages IMAP listeners on ports 143 (STARTTLS) and 993 (implicit TLS).
type Server struct {
	connPerIP sync.Map // map[string]*int32 — active connections per IP
	firewall  *auth.Firewall
	getConfig func() config.Config
	mu        sync.Mutex
	insecure  net.Listener // Port 143
	secure    net.Listener // Port 993
	sslCache  sync.Map
	store     *storage.Store
	wg        sync.WaitGroup
}

const maxConnectionsPerIP = 10

// NewServer creates a new IMAP server with the given dependencies.
func NewServer(store *storage.Store, fw *auth.Firewall, getConfig func() config.Config) *Server {
	return &Server{
		firewall:  fw,
		getConfig: getConfig,
		store:     store,
	}
}

// Start begins listening on IMAP ports with retry logic for zero-downtime updates.
func (s *Server) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.insecure != nil || s.secure != nil {
		return
	}

	// Single TLS config shared between implicit-TLS (993) and STARTTLS (143).
	tlsCfg := s.buildTLSConfig()

	// Port 993 — Implicit TLS
	go s.listenTLS(993, tlsCfg)

	// Port 143 — Plaintext with STARTTLS upgrade
	go s.listenPlain(143, tlsCfg)
}

// Stop gracefully shuts down both IMAP listeners.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.insecure != nil {
		s.insecure.Close()
		s.insecure = nil
	}
	if s.secure != nil {
		s.secure.Close()
		s.secure = nil
	}
	s.wg.Wait()
	log.Println("[IMAP] Servers stopped")
}

// ClearSSLCache removes cached TLS contexts for a domain or all domains.
func (s *Server) ClearSSLCache(domain string) {
	if domain == "" {
		s.sslCache = sync.Map{}
		return
	}
	s.sslCache.Range(func(key, _ any) bool {
		k := key.(string)
		if k == domain || strings.HasSuffix(k, "."+domain) {
			s.sslCache.Delete(k)
		}
		return true
	})
}

func (s *Server) listenTLS(port int, tlsCfg *tls.Config) {
	const maxRetries = 15

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[IMAP] TLS port %d in use. Retrying (%d/%d)...", port, attempt, maxRetries)
			time.Sleep(time.Second)
		}

		ln, err := tls.Listen("tcp", fmt.Sprintf(":%d", port), tlsCfg)
		if err != nil {
			if strings.Contains(err.Error(), "address already in use") {
				continue
			}
			log.Printf("[IMAP] TLS listen error: %v", err)
			return
		}

		s.mu.Lock()
		s.secure = ln
		s.mu.Unlock()

		log.Printf("[IMAP] TLS server listening on port %d", port)
		s.acceptLoop(ln, tlsCfg)
		return
	}

	log.Printf("[IMAP] TLS failed to bind port %d after %d retries", port, maxRetries)
}

func (s *Server) listenPlain(port int, tlsCfg *tls.Config) {
	const maxRetries = 15

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[IMAP] Plain port %d in use. Retrying (%d/%d)...", port, attempt, maxRetries)
			time.Sleep(time.Second)
		}

		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			if strings.Contains(err.Error(), "address already in use") {
				continue
			}
			log.Printf("[IMAP] Plain listen error: %v", err)
			return
		}

		s.mu.Lock()
		s.insecure = ln
		s.mu.Unlock()

		log.Printf("[IMAP] Plain server listening on port %d", port)
		s.acceptLoop(ln, tlsCfg)
		return
	}

	log.Printf("[IMAP] Plain failed to bind port %d after %d retries", port, maxRetries)
}

func (s *Server) acceptLoop(ln net.Listener, tlsCfg *tls.Config) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed") {
				return // Listener closed, shutting down
			}
			log.Printf("[IMAP] Accept error: %v", err)
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConnection(conn, tlsCfg)
		}()
	}
}

func (s *Server) handleConnection(conn net.Conn, tlsCfg *tls.Config) {
	defer conn.Close()

	ip := extractConnIP(conn)
	if s.firewall.IsBlocked(ip) {
		conn.Write([]byte("* BYE Your IP is blocked\r\n"))
		return
	}

	// Per-IP connection rate limiting
	val, _ := s.connPerIP.LoadOrStore(ip, new(int32))
	counter := val.(*int32)
	count := atomic.AddInt32(counter, 1)
	defer func() {
		if atomic.AddInt32(counter, -1) <= 0 {
			s.connPerIP.Delete(ip)
		}
	}()

	if count > maxConnectionsPerIP {
		log.Printf("[IMAP] Connection limit exceeded for %s (%d/%d)", ip, count, maxConnectionsPerIP)
		conn.Write([]byte("* BYE Too many connections\r\n"))
		return
	}

	log.Printf("[IMAP] New connection from %s (%d active)", ip, count)

	c := NewConnection(conn, tlsCfg, s.store, s.firewall, s.getConfig)
	c.Serve()
}

func (s *Server) buildTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			hostname := hello.ServerName

			if val, ok := s.sslCache.Load(hostname); ok {
				return val.(*tls.Certificate), nil
			}

			cfg := s.getConfig()

			h := hostname
			for {
				if domain, ok := cfg.Domains[h]; ok {
					cert, err := loadDomainCert(domain)
					if err == nil {
						s.sslCache.Store(hostname, cert)
						return cert, nil
					}
					break
				}
				idx := strings.Index(h, ".")
				if idx < 0 {
					break
				}
				h = h[idx+1:]
			}

			cert, err := loadDefaultCert(cfg.SSL)
			if err != nil {
				return nil, fmt.Errorf("no TLS certificate for %s: %w", hostname, err)
			}
			s.sslCache.Store(hostname, cert)
			return cert, nil
		},
	}
}

func loadDomainCert(domain config.Domain) (*tls.Certificate, error) {
	ssl := domain.Cert.SSL
	if ssl.Key == "" || ssl.Cert == "" {
		return nil, fmt.Errorf("no SSL config")
	}
	if _, err := os.Stat(ssl.Key); err != nil {
		return nil, err
	}
	if _, err := os.Stat(ssl.Cert); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(ssl.Cert, ssl.Key)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func loadDefaultCert(ssl config.SSL) (*tls.Certificate, error) {
	if ssl.Key == "" || ssl.Cert == "" {
		return nil, fmt.Errorf("no default SSL config")
	}
	cert, err := tls.LoadX509KeyPair(ssl.Cert, ssl.Key)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func extractConnIP(conn net.Conn) string {
	addr := conn.RemoteAddr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	// Strip IPv4-mapped IPv6 prefix
	return strings.TrimPrefix(host, "::ffff:")
}
