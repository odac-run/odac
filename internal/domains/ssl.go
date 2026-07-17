package domains

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/logx"
)

// acmeOrderer is the client surface SSL drives — Acme.js's order(). The real
// implementation wraps golang.org/x/crypto/acme (acme.go); tests inject a
// fake, exactly like the jest suite mocks the Acme module.
type acmeOrderer interface {
	Order(o orderOpts) (string, error)
}

// orderOpts ports the options object of Acme.order(). The callbacks receive
// (identifier, challengeType, token, authValue) — authValue is the http-01
// key authorization or the dns-01 TXT value, computed by the client.
type orderOpts struct {
	ChallengeType   string
	CSR             []byte
	Domains         []string
	ChallengeCreate func(identifier, challengeType, token, authValue string) error
	ChallengeRemove func(identifier, challengeType, token, authValue string)
}

// sslRun is Node's cancellation context ({cancelled: boolean}).
type sslRun struct {
	mu        sync.Mutex
	cancelled bool
}

func (r *sslRun) cancel()           { r.mu.Lock(); r.cancelled = true; r.mu.Unlock() }
func (r *sslRun) isCancelled() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.cancelled }

// domainState is one #checked entry: SAN-check throttle + error backoff.
type domainState struct {
	lastSanCheck time.Time
	errorCount   int
	interval     time.Time // backoff: no new attempt before this instant
}

// SSL is the SSL.js singleton: certificate lifecycle over config.domains.
type SSL struct {
	cfg     *config.Store
	log     *logx.Logger
	certDir string // <base>/cert/ssl — account key, per-domain key/crt pairs
	dns     DNSService
	proxy   ProxyService
	mail    MailService

	// newClient is Acme.create(); tests swap in a fake orderer.
	newClient func() (acmeOrderer, error)
	now       func() time.Time

	// bg tracks detached renewals (Node's un-awaited #ssl calls) so tests
	// can drain them deterministically.
	bg sync.WaitGroup

	mu         sync.Mutex
	checking   bool
	checked    map[string]*domainState
	processing map[string]*sslRun
	queued     map[string]bool
}

// acmeDirectory picks the ACME directory URL. ODAC_ACME_URL overrides it
// (no Node equivalent — Node hardcodes production). It exists for the 3.8
// staging host, where cert issue/renew must run for real against the Let's
// Encrypt staging endpoint without burning production rate limits. Unset in
// production.
func acmeDirectory() string {
	if url := os.Getenv("ODAC_ACME_URL"); url != "" {
		return url
	}
	return letsEncryptURL
}

// NewSSL wires the manager; certDir derives from the config base dir.
func NewSSL(cfg *config.Store, dns DNSService, proxy ProxyService, mail MailService) *SSL {
	s := &SSL{
		cfg:        cfg,
		log:        logx.New("SSL"),
		certDir:    filepath.Join(cfg.BaseDir(), "cert", "ssl"),
		dns:        dns,
		proxy:      proxy,
		mail:       mail,
		now:        time.Now,
		checked:    map[string]*domainState{},
		processing: map[string]*sslRun{},
		queued:     map[string]bool{},
	}
	directory := acmeDirectory()
	s.newClient = func() (acmeOrderer, error) {
		return newACMEClient(s.certDir, directory, nil, s.log)
	}
	hardenCertDir(s.certDir)
	return s
}

// writeKeyFile writes a PEM private key with owner-only (0600) permissions.
// os.WriteFile applies its mode only when creating a new file, so an existing
// world-readable key rewritten in place would keep its old bits — the explicit
// Chmod makes the tightening unconditional.
func writeKeyFile(path, keyPEM string) error {
	if err := os.WriteFile(path, []byte(keyPEM), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// hardenCertDir tightens permissions on the certificate directory (0700) and
// every private key it already holds (0600). Runs once at startup so keys
// issued by an earlier build — written world-readable — become owner-only
// immediately instead of waiting for their next renewal.
func hardenCertDir(certDir string) {
	if _, err := os.Stat(certDir); err != nil {
		return
	}
	_ = os.Chmod(certDir, 0o700)
	entries, err := os.ReadDir(certDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".key") {
			continue
		}
		_ = os.Chmod(filepath.Join(certDir, e.Name()), 0o600)
	}
}

func (s *SSL) spawn(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// Check runs on the system's 1s tick, porting check(): ensure the
// self-signed bootstrap cert, then renew any domain cert that is expired
// (or expires within 30 days) and re-verify SAN coverage every 5 minutes.
// The scan itself runs detached — Node's tick never awaits check() — with
// the checking flag claimed synchronously so overlapping ticks skip.
func (s *SSL) Check() {
	s.mu.Lock()
	if s.checking || len(s.processing) > 0 || len(s.queued) > 0 {
		s.mu.Unlock()
		return
	}
	hasDomains := false
	s.cfg.View(func() {
		hasDomains = s.cfg.Get("domains") != nil
	})
	if !hasDomains {
		s.mu.Unlock()
		return
	}
	s.checking = true
	s.mu.Unlock()

	s.spawn(func() {
		defer func() {
			s.mu.Lock()
			s.checking = false
			s.mu.Unlock()
		}()
		s.self()
		s.scan()
	})
}

// scan is the domain loop of check(); renewals run sequentially like Node's
// awaited #ssl calls. Domains iterate sorted (Node: insertion order).
func (s *SSL) scan() {
	var names []string
	s.cfg.View(func() {
		domains, _ := s.cfg.Get("domains").(map[string]any)
		names = sortedKeys(domains)
	})

	for _, domain := range names {
		var skip bool
		var expiry float64
		var hasSSL bool
		s.cfg.View(func() {
			domains, _ := s.cfg.Get("domains").(map[string]any)
			record, _ := domains[domain].(map[string]any)
			if record == nil {
				skip = true
				return
			}
			if v, ok := record["cert"]; ok && v == false {
				skip = true // cert === false opts the domain out
				return
			}
			cert, _ := record["cert"].(map[string]any)
			ssl, _ := cert["ssl"].(map[string]any)
			if ssl != nil {
				hasSSL = true
				expiry, _ = ssl["expiry"].(float64)
			}
		})
		if skip {
			continue
		}

		// Expiry: renew when missing or inside the 30-day window.
		if !hasSSL || float64(s.now().UnixMilli())+1000*60*60*24*30 > expiry {
			s.ssl(domain)
			continue
		}

		// SAN mismatch (missing subdomains) — every 5 minutes per domain.
		s.mu.Lock()
		st := s.checked[domain]
		if st == nil {
			st = &domainState{}
			s.checked[domain] = st
		}
		due := st.lastSanCheck.Add(5 * time.Minute).Before(s.now())
		if due {
			st.lastSanCheck = s.now()
		}
		s.mu.Unlock()
		if due && s.checkSanMismatch(domain) {
			s.log.Log("Detected missing subdomains in SSL certificate for %s. Queuing renewal.", domain)
			s.ssl(domain)
		}
	}
}

// checkSanMismatch ports #checkSanMismatch: parse the saved cert and verify
// every expected name (domain + subdomains, wildcard-aware) is in its SANs.
func (s *SSL) checkSanMismatch(domain string) bool {
	var certPath string
	var subdomains []any
	s.cfg.View(func() {
		domains, _ := s.cfg.Get("domains").(map[string]any)
		record, _ := domains[domain].(map[string]any)
		cert, _ := record["cert"].(map[string]any)
		ssl, _ := cert["ssl"].(map[string]any)
		certPath, _ = ssl["cert"].(string)
		if subs, ok := record["subdomain"].([]any); ok {
			subdomains = append([]any(nil), subs...)
		}
	})

	if certPath == "" {
		return true // missing cert, needs generation
	}
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return true
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		s.log.Error("Failed to parse certificate for SAN check for %s: %s", domain, "no PEM block")
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		s.log.Error("Failed to parse certificate for SAN check for %s: %s", domain, err.Error())
		return false
	}

	sans := map[string]bool{}
	for _, name := range cert.DNSNames {
		sans[name] = true
	}

	expected := expectedNames(domain, subdomains)
	for _, name := range expected {
		if !sans[name] {
			s.log.Log("SSL SAN Mismatch for %s. Expected: [%s], Found: [%s]",
				domain, strings.Join(expected, ", "), strings.Join(cert.DNSNames, ", "))
			return true
		}
	}
	return false
}

// expectedNames builds the SAN list for a domain record: the domain itself
// plus each subdomain — except single-level subdomains already covered by a
// '*' wildcard entry (Let's Encrypt wildcards cover exactly one level).
func expectedNames(domain string, subdomains []any) []string {
	names := []string{domain}
	hasWildcard := listContains(subdomains, "*")
	for _, v := range subdomains {
		sub, _ := v.(string)
		if sub == "" {
			continue
		}
		if hasWildcard && sub != "*" && !strings.Contains(sub, ".") {
			continue
		}
		names = append(names, sub+"."+domain)
	}
	return names
}

// Renew ports renew(): resolve the domain (subdomain requests renew their
// parent), fire the detached generation, and answer immediately.
func (s *SSL) Renew(domainArg any) api.Result {
	domain, _ := domainArg.(string)
	if domain == "" {
		domain = str(domainArg)
	}

	if ipv4Re.MatchString(domain) {
		return api.Res(false, __("SSL renewal is not available for IP addresses."))
	}

	found := false
	s.cfg.View(func() {
		domains, _ := s.cfg.Get("domains").(map[string]any)
		if _, ok := domains[domain]; ok {
			found = true
			return
		}
		// A subdomain request renews the parent's cert.
		for _, key := range sortedKeys(domains) {
			record, _ := domains[key].(map[string]any)
			subs, _ := record["subdomain"].([]any)
			for _, v := range subs {
				if sub, _ := v.(string); sub != "" && sub+"."+key == domain {
					domain = key
					found = true
					return
				}
			}
		}
	})
	if !found {
		return api.Res(false, __("Domain %s not found.", domain))
	}

	target := domain
	s.spawn(func() { s.ssl(target) })
	return api.Res(true, __("SSL certificate for domain %s renewed successfully.", domain))
}

// ssl ports #ssl: single-flight per domain with cancellation — a second
// request marks the running one stale and queues a fresh generation; the
// finally block re-fires it. Backed-off domains (recent failures) no-op.
func (s *SSL) ssl(domain string) {
	s.mu.Lock()
	if run, ok := s.processing[domain]; ok {
		run.cancel()
		s.queued[domain] = true
		s.mu.Unlock()
		s.log.Log("SSL generation for %s is outdated due to config change. Cancelling current run and queuing fresh generation.", domain)
		return
	}
	if st := s.checked[domain]; st != nil && st.interval.After(s.now()) {
		s.mu.Unlock()
		return
	}
	run := &sslRun{}
	s.processing[domain] = run
	s.mu.Unlock()

	err := s.generate(domain, run)
	if err != nil {
		if !run.isCancelled() {
			s.handleSSLError(domain, err)
		} else {
			s.log.Log("SSL generation for %s cancelled during execution. Suppressing error backoff.", domain)
		}
	}

	s.mu.Lock()
	delete(s.processing, domain)
	requeue := s.queued[domain]
	if requeue {
		delete(s.queued, domain)
		delete(s.checked, domain)
	}
	s.mu.Unlock()
	if requeue {
		s.log.Log("Processing queued SSL generation for %s", domain)
		s.spawn(func() { s.ssl(domain) })
	}
}

// generate is the try body of #ssl: client, SAN list, key+CSR, order,
// save — with the cancellation checks between each stage.
func (s *SSL) generate(domain string, run *sslRun) error {
	client, err := s.newClient()
	if err != nil {
		return err
	}

	var subdomains []any
	s.cfg.View(func() {
		domains, _ := s.cfg.Get("domains").(map[string]any)
		record, _ := domains[domain].(map[string]any)
		if subs, ok := record["subdomain"].([]any); ok {
			subdomains = append([]any(nil), subs...)
		}
	})
	names := expectedNames(domain, subdomains)

	if run.isCancelled() {
		s.log.Log("SSL generation for %s cancelled before CSR creation.", domain)
		return nil
	}

	keyPEM, signer, err := generateKeyPair()
	if err != nil {
		return err
	}
	csr, err := createCSR(names, signer)
	if err != nil {
		return err
	}

	s.log.Log("Requesting SSL certificate for domain %s...", domain)

	if run.isCancelled() {
		s.log.Log("SSL generation for %s cancelled before ACME request.", domain)
		return nil
	}

	cert, err := s.requestCertificate(client, csr, names, run)
	if err != nil {
		return err
	}

	if run.isCancelled() {
		s.log.Log("SSL generation for %s cancelled after ACME response. Discarding stale certificate.", domain)
		return nil
	}

	if cert == "" {
		s.log.Error("SSL certificate generation failed for domain %s: No certificate returned", domain)
		return nil
	}

	s.saveCertificate(domain, keyPEM, cert)
	return nil
}

// requestCertificate ports #requestCertificate: HTTP-01 first (fast, no
// nameserver delegation needed), DNS-01 as the universal fallback; wildcard
// orders go straight to DNS-01.
func (s *SSL) requestCertificate(client acmeOrderer, csr []byte, names []string, run *sslRun) (string, error) {
	hasWildcard := false
	for _, d := range names {
		if strings.HasPrefix(d, "*.") {
			hasWildcard = true
			break
		}
	}

	if !hasWildcard {
		s.log.Log("Attempting SSL via HTTP-01 challenge...")
		cert, err := s.acmeOrder(client, csr, names, "http-01", run)
		if err == nil {
			return cert, nil
		}
		if run.isCancelled() {
			return "", err // propagate cancellation, don't fall back
		}
		s.log.Log("HTTP-01 challenge failed: %s. Falling back to DNS-01...", err.Error())
	} else {
		s.log.Log("Wildcard domain detected. Skipping HTTP-01 and using DNS-01 exclusively.")
	}

	return s.acmeOrder(client, csr, names, "dns-01", run)
}

// acmeOrder ports #acmeOrder: run the order with challenge callbacks that
// push HTTP-01 tokens to the proxy and DNS-01 TXT records to the zone table.
func (s *SSL) acmeOrder(client acmeOrderer, csr []byte, names []string, challengeType string, run *sslRun) (string, error) {
	return client.Order(orderOpts{
		ChallengeType: challengeType,
		CSR:           csr,
		Domains:       names,
		ChallengeCreate: func(identifier, chalType, token, authValue string) error {
			if run.isCancelled() {
				return nil
			}
			switch chalType {
			case "http-01":
				s.log.Log("Creating HTTP-01 challenge for %s (token: %s...)", identifier, head(token, 8))
				if s.proxy == nil {
					return errors.New("Proxy process not running")
				}
				return s.proxy.SetACMEChallenge(token, authValue)
			case "dns-01":
				authzName := strings.TrimPrefix(identifier, "*.")
				s.log.Log("Creating DNS-01 challenge for %s", authzName)
				if s.dns != nil {
					s.dns.Record(map[string]any{
						"name":   "_acme-challenge." + authzName,
						"type":   "TXT",
						"value":  authValue,
						"ttl":    float64(100),
						"unique": true,
					})
				}
			}
			return nil
		},
		ChallengeRemove: func(identifier, chalType, token, authValue string) {
			switch chalType {
			case "http-01":
				s.log.Log("Removing HTTP-01 challenge for %s (token: %s...)", identifier, head(token, 8))
				if s.proxy != nil {
					s.proxy.DeleteACMEChallenge(token)
				}
			case "dns-01":
				authzName := strings.TrimPrefix(identifier, "*.")
				s.log.Log("Removing DNS-01 challenge for %s", authzName)
				if s.dns != nil {
					s.dns.Delete(map[string]any{
						"name":  "_acme-challenge." + authzName,
						"type":  "TXT",
						"value": authValue,
					})
				}
			}
		},
	})
}

// self ports #self: keep a self-signed bootstrap certificate (CN=Odac)
// alive for the proxy's default TLS context. Node used the `selfsigned`
// npm package; this is plain crypto/x509. The 1-day config expiry against
// a 365-day certificate is Node's own quirk, kept as-is (the cert is
// simply re-checked, and files that exist are reused).
func (s *SSL) self() {
	var key, cert string
	var expiry float64
	s.cfg.View(func() {
		ssl, _ := s.cfg.Get("ssl").(map[string]any)
		key, _ = ssl["key"].(string)
		cert, _ = ssl["cert"].(string)
		expiry, _ = ssl["expiry"].(float64)
	})
	if expiry > float64(s.now().UnixMilli()) && key != "" && cert != "" &&
		fileExists(key) && fileExists(cert) {
		return
	}

	s.log.Log("Generating self-signed SSL certificate...")
	keyPEM, certPEM, err := selfSigned("Odac", 365*24*time.Hour)
	if err != nil {
		s.log.Error("Failed to generate self-signed certificate: %s", err.Error())
		return
	}
	if err := os.MkdirAll(s.certDir, 0o700); err != nil {
		s.log.Error("Failed to generate self-signed certificate: %s", err.Error())
		return
	}
	keyFile := filepath.Join(s.certDir, "odac.key")
	crtFile := filepath.Join(s.certDir, "odac.crt")
	if err := writeKeyFile(keyFile, keyPEM); err != nil {
		s.log.Error("Failed to generate self-signed certificate: %s", err.Error())
		return
	}
	if err := os.WriteFile(crtFile, []byte(certPEM), 0o644); err != nil {
		s.log.Error("Failed to generate self-signed certificate: %s", err.Error())
		return
	}

	s.cfg.Mutate(func() {
		ssl, _ := s.cfg.Get("ssl").(map[string]any)
		if ssl == nil {
			ssl = map[string]any{}
		}
		ssl["key"] = keyFile
		ssl["cert"] = crtFile
		ssl["expiry"] = float64(s.now().UnixMilli() + 86400000)
		s.cfg.Set("ssl", ssl)
		s.cfg.Touch("ssl")
	})
}

// saveCertificate ports #saveCertificate: write the key/cert pair, update
// the domain record (90-day expiry window), clear the mail TLS cache and
// sync the proxy so it reloads certificates.
func (s *SSL) saveCertificate(domain, keyPEM, certPEM string) {
	s.mu.Lock()
	delete(s.checked, domain) // success resets backoff + SAN throttle
	s.mu.Unlock()

	keyFile := filepath.Join(s.certDir, domain+".key")
	crtFile := filepath.Join(s.certDir, domain+".crt")
	if err := os.MkdirAll(s.certDir, 0o700); err != nil {
		s.log.Error("Failed to save SSL certificate for domain %s: %s", domain, err.Error())
		return
	}
	if err := writeKeyFile(keyFile, keyPEM); err != nil {
		s.log.Error("Failed to save SSL certificate for domain %s: %s", domain, err.Error())
		return
	}
	if err := os.WriteFile(crtFile, []byte(certPEM), 0o644); err != nil {
		s.log.Error("Failed to save SSL certificate for domain %s: %s", domain, err.Error())
		return
	}

	// Re-fetch the record inside the write lock — the domain may have been
	// deleted or replaced while the ACME order ran.
	saved := false
	s.cfg.Mutate(func() {
		domains, _ := s.cfg.Get("domains").(map[string]any)
		record, _ := domains[domain].(map[string]any)
		if record == nil {
			return
		}
		cert, _ := record["cert"].(map[string]any)
		if cert == nil {
			cert = map[string]any{}
			record["cert"] = cert
		}
		cert["ssl"] = map[string]any{
			"key":    keyFile,
			"cert":   crtFile,
			"expiry": float64(s.now().UnixMilli() + 1000*60*60*24*30*3),
		}
		s.cfg.Touch("domains")
		saved = true
	})
	if !saved {
		return // Node: !domainRecord → silent return
	}

	if s.mail != nil {
		s.mail.ClearSSLCache(domain)
	}
	if s.proxy != nil {
		s.proxy.SyncConfig()
	}

	s.log.Log("SSL certificate successfully generated and saved for domain %s", domain)
}

// handleSSLError ports #handleSSLError: escalating retry backoff
// (30s → 2m → 10m → 30m) with error-class-specific log lines.
func (s *SSL) handleSSLError(domain string, err error) {
	s.mu.Lock()
	st := s.checked[domain]
	if st == nil {
		st = &domainState{}
		s.checked[domain] = st
	}
	st.errorCount++
	count := st.errorCount

	backoff := 30 * time.Second
	switch {
	case count == 2:
		backoff = 2 * time.Minute
	case count == 3:
		backoff = 10 * time.Minute
	case count >= 4:
		backoff = 30 * time.Minute
	}
	st.interval = s.now().Add(backoff)
	s.mu.Unlock()

	secs := int(backoff / time.Second)
	msg := err.Error()
	var dnsErr *net.DNSError
	switch {
	case strings.Contains(msg, "validateStatus"):
		s.log.Error("SSL certificate request failed for domain %s (Attempt %d). Next retry in %ds. Reason: HTTP validation error.",
			domain, count, secs)
	case errors.As(err, &dnsErr) || errors.Is(err, syscall.ECONNREFUSED):
		s.log.Error("SSL request failed for domain %s (Attempt %d). Next retry in %ds. Network issue: %s",
			domain, count, secs, msg)
	default:
		s.log.Error("SSL request failed for domain %s (Attempt %d). Next retry in %ds. Error: %s",
			domain, count, secs, msg)
	}
}

// selfSigned generates an RSA-2048 self-signed certificate (the `selfsigned`
// npm defaults Node passed: 2048-bit key, SHA-256, given CN and lifetime).
func selfSigned(commonName string, lifetime time.Duration) (keyPEM, certPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true, // selfsigned marks its certs CA:TRUE the same way
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return keyPEM, certPEM, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// head is token.substring(0, n) without panicking on short tokens.
func head(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
