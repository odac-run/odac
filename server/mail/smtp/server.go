package smtp

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"odac-mail/auth"
	"odac-mail/config"
	"odac-mail/limits"
	"odac-mail/storage"
)

// Server manages the inbound SMTP listeners (port 25 plaintext, port 465 implicit TLS).
// Uses go-smtp library with a custom Backend for authentication and delivery.
type Server struct {
	inboundBackend    *Backend
	submissionBackend *Backend
	dkimSigner        interface{ Sign(string, []byte) ([]byte, error) }
	getConfig         func() config.Config
	mu                sync.Mutex
	secure            *gosmtp.Server // Port 465 (implicit TLS)
	insecure          *gosmtp.Server // Port 25 (STARTTLS)
	sslCache          sync.Map       // map[string]*tls.Certificate
}

// NewServer creates a new SMTP server with the given dependencies.
//
// Two backends are created with separate limiter instances so port 25
// (inbound MTA traffic) and port 465 (authenticated submission) get
// independent ceilings; a flood from one cannot starve the other.
func NewServer(store *storage.Store, fw *auth.Firewall, getConfig func() config.Config, dkimSigner interface{ Sign(string, []byte) ([]byte, error) }) *Server {
	return &Server{
		inboundBackend:    NewBackend(store, fw, getConfig, limits.New(limits.SMTPInboundProfile()), "inbound"),
		submissionBackend: NewBackend(store, fw, getConfig, limits.New(limits.SMTPSubmissionProfile()), "submission"),
		dkimSigner:        dkimSigner,
		getConfig:         getConfig,
	}
}

// Start begins listening on SMTP ports with retry logic for zero-downtime updates.
func (s *Server) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.insecure != nil || s.secure != nil {
		return // Already started
	}

	// Initialize outbound client with DKIM signer
	InitClient(s.getConfig, s.dkimSigner)

	// Port 25 — Plaintext with STARTTLS
	// AllowInsecureAuth=false: Auth credentials only accepted after STARTTLS upgrade.
	// Unauthenticated inbound mail delivery still works (no auth required for receiving).
	s.insecure = gosmtp.NewServer(s.inboundBackend)
	s.insecure.Addr = ":25"
	s.insecure.AllowInsecureAuth = false
	s.insecure.Domain = "ODAC"
	s.insecure.MaxMessageBytes = 10 * 1024 * 1024 // 10MB
	s.insecure.MaxRecipients = 100
	s.insecure.ReadTimeout = 60 * time.Second
	s.insecure.WriteTimeout = 60 * time.Second

	// Port 465 — Implicit TLS (auth always over TLS)
	s.secure = gosmtp.NewServer(s.submissionBackend)
	s.secure.Addr = ":465"
	s.secure.Domain = "ODAC"
	s.secure.MaxMessageBytes = 10 * 1024 * 1024
	s.secure.MaxRecipients = 100
	s.secure.ReadTimeout = 60 * time.Second
	s.secure.WriteTimeout = 60 * time.Second

	// Configure TLS with SNI callback for per-domain certificates
	tlsCfg := s.buildTLSConfig()
	s.insecure.TLSConfig = tlsCfg
	s.secure.TLSConfig = tlsCfg

	// Start listeners with retry
	go s.listenWithRetry(s.insecure, 25, "SMTP Insecure")
	go s.listenWithRetry(s.secure, 465, "SMTP Secure")
}

// Stop gracefully shuts down both SMTP listeners and the outbound client.
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
	if client := GetClient(); client != nil {
		client.Stop()
	}
	log.Println("[SMTP] Servers stopped")
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

// buildTLSConfig creates a TLS configuration with SNI callback for per-domain certificates.
// Mirrors the Node.js Mail.js SNICallback logic exactly.
func (s *Server) buildTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			hostname := hello.ServerName

			// Check cache
			if val, ok := s.sslCache.Load(hostname); ok {
				return val.(*tls.Certificate), nil
			}

			cfg := s.getConfig()

			// Walk up domain hierarchy to find matching certificate
			h := hostname
			for {
				if domain, ok := cfg.Domains[h]; ok {
					cert, err := s.loadDomainCert(domain)
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

			// Fallback to default SSL certificate
			cert, err := s.loadDefaultCert(cfg.SSL)
			if err != nil {
				return nil, fmt.Errorf("no TLS certificate for %s: %w", hostname, err)
			}
			s.sslCache.Store(hostname, cert)
			return cert, nil
		},
	}
}

// loadDomainCert loads a TLS certificate from domain-specific paths.
func (s *Server) loadDomainCert(domain config.Domain) (*tls.Certificate, error) {
	ssl := domain.Cert.SSL
	if ssl.Key == "" || ssl.Cert == "" {
		return nil, fmt.Errorf("no SSL config for domain")
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

// loadDefaultCert loads the fallback TLS certificate.
func (s *Server) loadDefaultCert(ssl config.SSL) (*tls.Certificate, error) {
	if ssl.Key == "" || ssl.Cert == "" {
		return nil, fmt.Errorf("no default SSL config")
	}
	cert, err := tls.LoadX509KeyPair(ssl.Cert, ssl.Key)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// listenWithRetry starts an SMTP server with retry logic for EADDRINUSE.
// Used during zero-downtime updates when the old instance hasn't released the port.
func (s *Server) listenWithRetry(srv *gosmtp.Server, port int, name string) {
	const maxRetries = 15
	const retryDelay = time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[SMTP] %s port %d in use. Retrying (%d/%d)...", name, port, attempt, maxRetries)
			time.Sleep(retryDelay)
		}

		var err error
		if port == 465 {
			err = srv.ListenAndServeTLS()
		} else {
			err = srv.ListenAndServe()
		}

		if err == nil {
			return
		}

		if !strings.Contains(err.Error(), "address already in use") {
			log.Printf("[SMTP] %s error: %v", name, err)
			return
		}
	}

	log.Printf("[SMTP] %s failed to bind port %d after %d retries", name, port, maxRetries)
}
