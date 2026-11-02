package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac/internal/mtls"
	"odac/internal/proxy/config"
)

// maxCertVerdicts bounds each generation of the verdict cache: in optional mode
// anyone can present any certificate, so its keys are attacker-chosen. It has to
// exceed the certificates in use at once, or every request verifies again.
const maxCertVerdicts = 1 << 16

// failedVerdictTTL re-checks a refused chain after a minute, since the only
// thing that can change sooner is the trust store, and that changes the key.
const failedVerdictTTL = time.Minute

// errClientAuthOverTCP refuses QUIC for a host with a policy so the client falls
// back to TCP, where a certificate request and 0-RTT cannot interact.
var errClientAuthOverTCP = errors.New("client certificate hosts are served over TCP")

type clientAuthKey struct{}

// clientAuthState is what the gate decided about one request. It reaches the
// director and every response path through the request context, so a state is
// present exactly when the host has a policy.
type clientAuthState struct {
	fingerprint string
	issuer      string
	key         string
	notAfter    string
	presented   bool // a certificate was shown, valid or not: never touch shared caches
	serial      string
	subject     string
	until       time.Time
	verify      string
}

func clientAuthFrom(r *http.Request) *clientAuthState {
	state, _ := r.Context().Value(clientAuthKey{}).(*clientAuthState)
	return state
}

// presentsClientCert reports a request whose response may depend on who is asking.
func presentsClientCert(r *http.Request) bool {
	state := clientAuthFrom(r)
	return state != nil && state.presented
}

// certVerdict is the cached result of verifying one presented chain.
type certVerdict struct {
	root   string
	status string
	until  time.Time
}

type verdictKey [sha256.Size]byte

// verdictCache remembers verified chains across every trust store and config push.
// A key covers the anchors as well as the chain, so a change of CAs cannot reuse an
// old answer and everything else keeps its verdicts: revocation and scope are read
// from the table per request and never cached here. Two generations replace a
// wholesale reset, which sent every client back to verification each time the cache
// filled. A verdict in use moves forward, so only what went unused for a full
// generation is lost, and a flood of junk costs nobody more than one re-check.
type verdictCache struct {
	mu     sync.Mutex
	older  map[verdictKey]certVerdict
	recent map[verdictKey]certVerdict
}

func newVerdictCache() *verdictCache {
	return &verdictCache{recent: map[verdictKey]certVerdict{}}
}

func (c *verdictCache) get(key verdictKey) (certVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if verdict, ok := c.recent[key]; ok {
		return verdict, true
	}
	verdict, ok := c.older[key]
	if ok {
		c.addLocked(key, verdict)
	}
	return verdict, ok
}

func (c *verdictCache) put(key verdictKey, verdict certVerdict) {
	c.mu.Lock()
	c.addLocked(key, verdict)
	c.mu.Unlock()
}

func (c *verdictCache) addLocked(key verdictKey, verdict certVerdict) {
	if len(c.recent) >= maxCertVerdicts {
		c.older = c.recent
		c.recent = map[verdictKey]certVerdict{}
	}
	c.recent[key] = verdict
}

// trustStore is one set of CAs. Verification is cached because optional mode cannot
// let the handshake do it (an expired certificate would lose access to a public
// site) and running x509 verification per request is too slow.
type trustStore struct {
	anchors  [sha256.Size]byte
	managed  map[string]bool // anchor fingerprint -> needs an ODAC scope record
	pool     *x509.CertPool
	verdicts *verdictCache
}

func newTrustStore(ids []string, cas map[string]config.MTLSCA, verdicts *verdictCache) *trustStore {
	// Never a nil pool: crypto/tls reads nil ClientCAs as "trust the host's roots".
	ts := &trustStore{managed: map[string]bool{}, pool: x509.NewCertPool(), verdicts: verdicts}
	for _, id := range ids {
		ca, ok := cas[id]
		if !ok {
			log.Printf("[mTLS] CA %s is referenced but not defined", id)
			continue
		}
		rest := []byte(ca.Cert)
		found := false
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			ts.pool.AddCert(cert)
			ts.managed[mtls.Fingerprint(cert.Raw)] = ca.Managed
			found = true
		}
		if !found {
			log.Printf("[mTLS] CA %s holds no usable certificate", id)
		}
	}
	anchors := make([]string, 0, len(ts.managed))
	for fingerprint := range ts.managed {
		anchors = append(anchors, fingerprint)
	}
	sort.Strings(anchors)
	ts.anchors = sha256.Sum256([]byte(strings.Join(anchors, ",")))
	return ts
}

func (ts *trustStore) verify(certs []*x509.Certificate, now time.Time) certVerdict {
	hash := sha256.New()
	hash.Write(ts.anchors[:])
	for _, cert := range certs {
		hash.Write(cert.Raw)
	}
	var key verdictKey
	hash.Sum(key[:0])

	if cached, ok := ts.verdicts.get(key); ok && now.Before(cached.until) {
		return cached
	}
	verdict := ts.check(certs, now)
	ts.verdicts.put(key, verdict)
	return verdict
}

func (ts *trustStore) check(certs []*x509.Certificate, now time.Time) certVerdict {
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	chains, err := certs[0].Verify(x509.VerifyOptions{
		CurrentTime:   now,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Roots:         ts.pool,
	})
	if err != nil {
		status := mtls.VerifyUntrusted
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
			status = mtls.VerifyExpired
		}
		return certVerdict{status: status, until: now.Add(failedVerdictTTL)}
	}
	chain := chains[0]
	until := chain[0].NotAfter
	for _, cert := range chain[1:] {
		if cert.NotAfter.Before(until) {
			until = cert.NotAfter
		}
	}
	return certVerdict{root: mtls.Fingerprint(chain[len(chain)-1].Raw), status: mtls.VerifySuccess, until: until}
}

// authPolicy is one host's requirement. Two hosts with the same id negotiate
// identically, so a connection opened for one is good enough for the other.
type authPolicy struct {
	id       string
	required bool
	tlsOnce  sync.Once
	tlsConf  *tls.Config
	trust    *trustStore
}

// handshakeConfig is built on first use because the control API can deliver a
// config before main has finished assembling the TLS base.
func (pol *authPolicy) handshakeConfig(base *tls.Config) *tls.Config {
	pol.tlsOnce.Do(func() {
		if base == nil {
			return
		}
		conf := base.Clone()
		conf.GetConfigForClient = nil
		conf.ClientCAs = pol.trust.pool
		if pol.required {
			conf.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			conf.ClientAuth = tls.RequestClientCert
		}
		pol.tlsConf = conf
	})
	return pol.tlsConf
}

// authRecord is one domain record of an app with a policy.
type authRecord struct {
	base      string // parent a wildcard entry covers, "" when the record has none
	domain    string
	exact     map[string]bool
	key       string
	overrides map[string]*authPolicy // a nil policy turns the record's off for that host
	policy    *authPolicy
}

// allows is the strict replacement for the parent walk. The walk would send
// admin.anything.x.com to x.com's app, and frameworks that route on the first
// label would then serve the admin area past admin.x.com's policy.
func (rec *authRecord) allows(host string) bool {
	if host == rec.domain || rec.exact[host] {
		return true
	}
	if rec.base == "" {
		return false
	}
	return mtls.WildcardOf(host) == "*."+rec.base
}

func (rec *authRecord) policyFor(host string) *authPolicy {
	if pol, ok := rec.overrides[host]; ok {
		return pol
	}
	return rec.policy
}

// clientAuthTable is the compiled, immutable form of a config push. It is
// swapped whole, so a request sees one consistent generation.
type clientAuthTable struct {
	certs   map[string]map[string]bool
	records map[string]*authRecord
	revoked map[string]bool
}

func compileClientAuth(domains map[string]config.Website, store *config.MTLS, verdicts *verdictCache) *clientAuthTable {
	t := &clientAuthTable{
		certs:   map[string]map[string]bool{},
		records: map[string]*authRecord{},
		revoked: map[string]bool{},
	}
	var cas map[string]config.MTLSCA
	if store != nil {
		cas = store.CAs
		for fingerprint, hosts := range store.Certs {
			scope := make(map[string]bool, len(hosts))
			for _, host := range hosts {
				scope[host] = true
			}
			t.certs[fingerprint] = scope
		}
		for _, fingerprint := range store.Revoked {
			t.revoked[fingerprint] = true
		}
	}

	stores := map[string]*trustStore{}
	policies := map[string]*authPolicy{}
	policyFor := func(mode string, ids []string) *authPolicy {
		if mode != mtls.ModeRequired && mode != mtls.ModeOptional {
			return nil
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		trustID := strings.Join(sorted, ",")
		id := mode + "|" + trustID
		if pol, ok := policies[id]; ok {
			return pol
		}
		trust, ok := stores[trustID]
		if !ok {
			trust = newTrustStore(sorted, cas, verdicts)
			stores[trustID] = trust
		}
		pol := &authPolicy{id: id, required: mode == mtls.ModeRequired, trust: trust}
		policies[id] = pol
		return pol
	}

	for name, site := range domains {
		if site.MTLS == nil {
			continue
		}
		rec := &authRecord{
			domain:    name,
			exact:     map[string]bool{},
			key:       site.MTLS.Key,
			overrides: map[string]*authPolicy{},
			policy:    policyFor(site.MTLS.Mode, site.MTLS.CA),
		}
		if strings.HasPrefix(name, "*.") {
			rec.base = name[2:]
		}
		for _, sub := range site.Subdomains {
			if sub == "*" {
				rec.base = name
				continue
			}
			rec.exact[sub+"."+name] = true
		}
		for host, override := range site.MTLS.Hosts {
			ids := override.CA
			if len(ids) == 0 {
				ids = site.MTLS.CA
			}
			rec.overrides[host] = policyFor(override.Mode, ids)
		}
		t.records[name] = rec
	}
	return t
}

// UpdateClientAuth installs a config push's client certificate rules. The API
// calls it before UpdateConfig: with the table in place first, a domain that
// gains a policy is never reachable under the old rules.
func (p *Proxy) UpdateClientAuth(domains map[string]config.Website, store *config.MTLS) {
	t := compileClientAuth(domains, store, p.verdicts)
	if len(t.records) == 0 {
		p.clientAuth.Store(nil)
		return
	}
	p.clientAuth.Store(t)
}

// UpdateClientCerts applies a change to the certificates alone, against the domains
// the proxy already has. Unlike UpdateConfig it leaves the TLS and OCSP caches of
// every other site in place.
func (p *Proxy) UpdateClientCerts(store *config.MTLS) {
	p.mu.RLock()
	domains := p.domains
	p.mu.RUnlock()
	p.UpdateClientAuth(domains, store)
	p.reauthorizeLive()
}

// SetTLSBase hands over the TCP TLS config each host policy is derived from.
func (p *Proxy) SetTLSBase(base *tls.Config) {
	p.mu.Lock()
	p.tlsBase = base
	p.mu.Unlock()
}

// normalizeHost is the one spelling of a host every policy decision uses. Hosts
// are case-insensitive but the routing table is not: ADMIN.x.com used to miss
// admin.x.com's record and fall through to x.com's.
func normalizeHost(host string) string {
	return strings.TrimPrefix(strings.ToLower(host), "www.")
}

func (p *Proxy) policyForHost(t *clientAuthTable, host string) *authPolicy {
	p.mu.RLock()
	site, ok := p.resolveDomain(host)
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	rec := t.records[site.Domain]
	if rec == nil {
		return nil
	}
	return rec.policyFor(host)
}

// GetConfigForClient asks for a client certificate only on hosts with a policy.
// It cannot be global: a certificate request on every site would prompt visitors
// of unrelated public domains.
func (p *Proxy) GetConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	t := p.clientAuth.Load()
	if t == nil {
		return nil, nil
	}
	pol := p.policyForHost(t, normalizeHost(hello.ServerName))
	if pol == nil {
		return nil, nil
	}
	p.mu.RLock()
	base := p.tlsBase
	p.mu.RUnlock()
	conf := pol.handshakeConfig(base)
	if conf == nil {
		return nil, errors.New("client certificate policy has no TLS base")
	}
	return conf, nil
}

// GetHTTP3ConfigForClient keeps policy hosts off QUIC.
func (p *Proxy) GetHTTP3ConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	t := p.clientAuth.Load()
	if t != nil && p.policyForHost(t, normalizeHost(hello.ServerName)) != nil {
		return nil, errClientAuthOverTCP
	}
	return nil, nil
}

// evaluate decides one request against one policy, returning the status to
// deny with or zero. It runs on every request rather than trusting the
// handshake: the SNI can name another host, a resumed session skips the chain
// check, and a connection outlives both its certificate and any revocation.
func (p *Proxy) evaluate(t *clientAuthTable, conn *tls.ConnectionState, host string, rec *authRecord, pol *authPolicy, now time.Time) (*clientAuthState, int) {
	state := &clientAuthState{key: rec.key, verify: mtls.VerifyNone}
	if conn == nil {
		return state, 0
	}
	// 0-RTT arrives before the client has proven anything.
	if !conn.HandshakeComplete {
		return state, http.StatusTooEarly
	}

	sni := normalizeHost(conn.ServerName)
	if sni != host {
		// The connection was negotiated for a host with other rules, so what it
		// carries says nothing about this one. 421 makes a browser that coalesced
		// the two open a connection with the right SNI.
		if sniPol := p.policyForHost(t, sni); sniPol == nil || sniPol.id != pol.id {
			return state, http.StatusMisdirectedRequest
		}
	}

	if len(conn.PeerCertificates) == 0 {
		if pol.required {
			return state, http.StatusForbidden
		}
		return state, 0
	}

	state.presented = true
	leaf := conn.PeerCertificates[0]
	fingerprint := mtls.Fingerprint(leaf.Raw)

	verdict := certVerdict{status: mtls.VerifyRevoked}
	if !t.revoked[fingerprint] {
		verdict = pol.trust.verify(conn.PeerCertificates, now)
	}
	state.verify = verdict.status

	if verdict.status == mtls.VerifySuccess && pol.trust.managed[verdict.root] {
		scope, known := t.certs[fingerprint]
		switch {
		case !known:
			// A managed CA signs nothing without a record, so this cannot be ours.
			state.verify = mtls.VerifyUntrusted
		case !scope[host] && !scope[mtls.WildcardOf(host)]:
			state.verify = mtls.VerifyOutOfScope
			if sni != host {
				return state, http.StatusMisdirectedRequest
			}
		}
	}

	if state.verify != mtls.VerifySuccess {
		if pol.required {
			return state, http.StatusForbidden
		}
		return state, 0
	}

	state.fingerprint = fingerprint
	state.issuer = verdict.root
	state.notAfter = strconv.FormatInt(leaf.NotAfter.UnixMilli(), 10)
	state.serial = leaf.SerialNumber.Text(16)
	// Escaped because an external CA can put anything in a subject, and a header
	// value the transport refuses would turn into a 502 for that client.
	state.subject = url.PathEscape(leaf.Subject.String())
	state.until = verdict.until
	return state, 0
}

// liveClientAuth is one in-flight authenticated request.
type liveClientAuth struct {
	cancel context.CancelFunc
	conn   *tls.ConnectionState
	domain string
	host   string
}

// gateClientAuth enforces the policy of the host a request names. It returns the
// request to continue with and a release function, or handled=true when it has
// already answered. It must run before the caches and early hints, which key by
// host and path alone and would otherwise answer an unauthenticated client.
func (p *Proxy) gateClientAuth(w http.ResponseWriter, r *http.Request, host string, site config.Website, hasSSL bool) (*http.Request, func(), bool) {
	release := func() {}
	t := p.clientAuth.Load()
	if t == nil {
		return r, release, false
	}
	rec := t.records[site.Domain]
	if rec == nil {
		return r, release, false
	}
	if !rec.allows(host) {
		debugLog("[mTLS] Host '%s' is not registered on %s - rejecting request", host, rec.domain)
		dropConnection(w)
		return r, release, true
	}
	pol := rec.policyFor(host)
	if pol == nil {
		return r, release, false
	}

	state, deny := p.evaluate(t, r.TLS, host, rec, pol, time.Now())
	r = r.WithContext(context.WithValue(r.Context(), clientAuthKey{}, state))

	// Plain HTTP can never prove a certificate. ServeHTTP still redirects it to
	// HTTPS, but where it would not (no server certificate, or an IP or localhost,
	// which it exempts) a required host has to stop here or it is served in the clear.
	if r.TLS == nil && pol.required && (!hasSSL || net.ParseIP(host) != nil || host == "localhost") {
		deny = http.StatusForbidden
	}

	if deny != 0 {
		debugLog("[mTLS] Denied %s with %d (status: %s)", host, deny, state.verify)
		p.setEdgeHeaders(w.Header(), r)
		http.Error(w, http.StatusText(deny), deny)
		return r, release, true
	}

	if state.verify != mtls.VerifySuccess {
		return r, release, false
	}

	ctx, cancel := context.WithDeadline(r.Context(), state.until)
	live := &liveClientAuth{cancel: cancel, conn: r.TLS, domain: rec.domain, host: host}
	p.trackLive(t, live, time.Now())
	release = func() {
		p.liveAuthMu.Lock()
		delete(p.liveAuth, live)
		p.liveAuthMu.Unlock()
		cancel()
	}
	return r.WithContext(ctx), release, false
}

// trackLive registers a request that passed under table t. A push landing between
// that decision and this registration would miss it, and a WebSocket opened in that
// gap would outlive a revocation, so a table that has moved on decides again.
func (p *Proxy) trackLive(t *clientAuthTable, live *liveClientAuth, now time.Time) {
	p.liveAuthMu.Lock()
	p.liveAuth[live] = struct{}{}
	p.liveAuthMu.Unlock()
	if current := p.clientAuth.Load(); current != nil && current != t && !p.stillAllowed(current, live, now) {
		live.cancel()
	}
}

func (p *Proxy) stillAllowed(t *clientAuthTable, live *liveClientAuth, now time.Time) bool {
	rec := t.records[live.domain]
	if rec == nil {
		return true
	}
	pol := rec.policyFor(live.host)
	if pol == nil {
		return true
	}
	state, deny := p.evaluate(t, live.conn, live.host, rec, pol, now)
	return deny == 0 && state.verify == mtls.VerifySuccess
}

// reauthorizeLive applies a config push to requests already in flight. Without
// this a revoked client keeps its WebSocket or stream for as long as it likes,
// since those never produce another request to check. ReverseProxy closes both
// sides of an upgraded connection when the request context ends. The checks run
// outside liveAuthMu: every authenticated request takes it, and holding it through
// a push cost them 836ms at 10k connections.
func (p *Proxy) reauthorizeLive() {
	t := p.clientAuth.Load()
	if t == nil {
		return
	}
	p.liveAuthMu.Lock()
	live := make([]*liveClientAuth, 0, len(p.liveAuth))
	for entry := range p.liveAuth {
		live = append(live, entry)
	}
	p.liveAuthMu.Unlock()

	now := time.Now()
	for _, entry := range live {
		if !p.stillAllowed(t, entry, now) {
			entry.cancel()
		}
	}
}

// applyClientAuthHeaders tells the app what the gate decided. Only the signature
// is worth trusting: other containers on odac-network and any published port can
// reach the app without passing through here.
func applyClientAuthHeaders(req *http.Request, state *clientAuthState, now time.Time) {
	assertion := mtls.Assertion{
		Fingerprint: state.fingerprint,
		Host:        req.Host,
		Issuer:      state.issuer,
		Method:      req.Method,
		NotAfter:    state.notAfter,
		Serial:      state.serial,
		Subject:     state.subject,
		Timestamp:   now.Unix(),
		URI:         req.URL.RequestURI(),
		Verify:      state.verify,
	}
	req.Header.Set(mtls.HeaderVerify, state.verify)
	if state.verify == mtls.VerifySuccess {
		req.Header.Set(mtls.HeaderFingerprint, state.fingerprint)
		req.Header.Set(mtls.HeaderIssuer, state.issuer)
		req.Header.Set(mtls.HeaderNotAfter, state.notAfter)
		req.Header.Set(mtls.HeaderSerial, state.serial)
		req.Header.Set(mtls.HeaderSubject, state.subject)
	}
	req.Header.Set(mtls.HeaderSignature, assertion.Sign(state.key))
}

// dropConnection closes without answering so a scanner learns nothing, falling
// back to a 404 on protocols that cannot be hijacked.
func dropConnection(w http.ResponseWriter) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	http.Error(w, "", http.StatusNotFound)
}
