package domains

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/logx"
	"odac/internal/mtls"
	"odac/internal/ports"
)

const (
	mtlsCALifetime      = 10 * 365 * 24 * time.Hour
	mtlsDefaultCertDays = 365
	mtlsMaxCertDays     = 3650
	mtlsMaxNameLen      = 64
	mtlsMinRSABits      = 2048
)

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MTLSProxy is the proxy surface MTLS needs: pushing a change, and spotting a
// tunnel, which dials the backend directly and so would route around any policy.
// SyncClientCerts carries what only certificates changed, which leaves every other
// site's state alone. SyncConfig is for changes to a domain record.
// *dataplane.Proxy implements it.
type MTLSProxy interface {
	SyncClientCerts()
	SyncConfig()
	TunnelDomains(app string) []string
}

// MTLS owns client certificate policy and issuance. CA private keys stay on this
// server, next to the TLS keys protecting the same traffic, and the proxy only
// ever receives public material. Client private keys never reach it at all: the
// caller holds the key and sends a CSR, so a dashboard or a hub log cannot leak one.
type MTLS struct {
	cfg     *config.Store
	log     *logx.Logger
	proxy   MTLSProxy
	mu      sync.Mutex // serializes check-then-write sequences across the config lock
	now     func() time.Time
	certDir string

	storeMu sync.Mutex // guards store only: CertSnapshot runs without mu
	store   *certStore
}

// NewMTLS wires the manager.
func NewMTLS(cfg *config.Store, proxy MTLSProxy) *MTLS {
	return &MTLS{
		cfg:     cfg,
		log:     logx.New("mTLS"),
		proxy:   proxy,
		now:     time.Now,
		certDir: filepath.Join(cfg.BaseDir(), "cert", "mtls"),
	}
}

// casLocked returns config.mtls.cas under an already-held lock, creating it when
// asked (write requires Mutate).
func (m *MTLS) casLocked(create bool) map[string]any {
	store, _ := m.cfg.Get("mtls").(map[string]any)
	if store == nil {
		if !create {
			return nil
		}
		store = map[string]any{}
		m.cfg.Set("mtls", store)
	}
	cas, _ := store["cas"].(map[string]any)
	if create && cas == nil {
		cas = map[string]any{}
		store["cas"] = cas
	}
	return cas
}

// certs returns the certificate store, opening it on first use and again after a
// failure, so a disk problem at boot does not last until the next restart.
func (m *MTLS) certs() (*certStore, error) {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	if m.store != nil {
		return m.store, nil
	}
	store, err := openCertStore(filepath.Join(m.certDir, "certs.db"))
	if err != nil {
		return nil, err
	}
	m.store = store
	return store, nil
}

// CertSnapshot is the proxy's view of the certificates. *dataplane.Proxy reads it
// through a narrow interface because the two would otherwise import each other.
func (m *MTLS) CertSnapshot(now time.Time) (mtls.CertSnapshot, error) {
	store, err := m.certs()
	if err != nil {
		return mtls.CertSnapshot{}, err
	}
	return store.snapshot(now)
}

// Stop releases the certificate store.
func (m *MTLS) Stop() {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	if m.store != nil {
		m.store.close()
		m.store = nil
	}
}

func (m *MTLS) storeFailure(err error) api.Result {
	m.log.Error("Client certificate store failed: %s", err.Error())
	return api.Res(false, __("The certificate store is unavailable."))
}

func (m *MTLS) domainsLocked() map[string]any {
	domains, _ := m.cfg.Get("domains").(map[string]any)
	return domains
}

// locateHost finds the record serving a host, using the same rules the proxy
// enforces once a policy exists: exact record, listed subdomain, or a wildcard
// reaching one label.
func locateHost(domains map[string]any, host string) (string, bool) {
	if _, ok := domains[host]; ok {
		return host, true
	}
	labels := strings.Split(host, ".")
	for i := 1; i < len(labels); i++ {
		parent := strings.Join(labels[i:], ".")
		if i == 1 {
			if _, ok := domains["*."+parent]; ok {
				return "*." + parent, true
			}
		}
		record, _ := domains[parent].(map[string]any)
		if record == nil {
			continue
		}
		sub := strings.Join(labels[:i], ".")
		if listContains(record["subdomain"], sub) || (i == 1 && listContains(record["subdomain"], "*")) {
			return parent, true
		}
	}
	return "", false
}

// reach decides which records a caller acts on. The operator reaches every
// record, reported as app "". An app token reaches only the records routed to it,
// matched by the name its token carries, which is also what its signing key is
// derived from. Anything else (a domain token, a caller nobody filled in) is not
// ok and reaches nothing.
func reach(caller api.Caller) (app string, ok bool) {
	if caller.App != "" {
		return caller.App, true
	}
	return "", caller.Root
}

func permissionDenied() api.Result {
	return api.Res(false, "permission_denied")
}

// notFoundForApp is what an app hears about a certificate it cannot act on,
// whether it exists or belongs to another app, so a fingerprint cannot be probed.
// The code lets a caller close a queued revocation without parsing the message.
func notFoundForApp() api.Result {
	return api.Res(false, __("Certificate not found."), map[string]any{"code": "not_found"})
}

// ownedLocked reports whether app may act on a record. The operator ("") may act
// on any.
func ownedLocked(domains map[string]any, record, app string) bool {
	if app == "" {
		return true
	}
	rec, _ := domains[record].(map[string]any)
	return rec != nil && rec["appId"] == any(app)
}

// appCAsLocked returns the CAs bound to the records routed to app, record-wide
// or on a host override.
func appCAsLocked(domains map[string]any, app string) map[string]bool {
	out := map[string]bool{}
	for name := range domains {
		if !ownedLocked(domains, name, app) {
			continue
		}
		cfg := recordConfig(domains, name)
		for _, id := range stringList(cfg["ca"]) {
			out[id] = true
		}
		hosts, _ := cfg["hosts"].(map[string]any)
		for _, h := range hosts {
			override, _ := h.(map[string]any)
			for _, id := range stringList(override["ca"]) {
				out[id] = true
			}
		}
	}
	return out
}

// ownsCertLocked reports whether a certificate record is app's: its CA is bound
// to one of app's records and every host it opens is one of app's. The second
// half matters when a CA is shared with another app's record, which would
// otherwise hand that app's certificates over too. cas is appCAsLocked's answer.
func ownsCertLocked(domains map[string]any, app string, cas map[string]bool, cert map[string]any) bool {
	if app == "" {
		return true
	}
	ca, _ := cert["ca"].(string)
	if ca == "" || !cas[ca] {
		return false
	}
	for _, host := range stringList(cert["hosts"]) {
		record, ok := locateHost(domains, host)
		if !ok || !ownedLocked(domains, record, app) {
			return false
		}
	}
	return true
}

func recordConfig(domains map[string]any, name string) map[string]any {
	record, _ := domains[name].(map[string]any)
	cfg, _ := record["mtls"].(map[string]any)
	return cfg
}

// effectiveCAs returns the CAs trusted on a host: its override's, else the record's.
func effectiveCAs(domains map[string]any, record, host string) []string {
	cfg := recordConfig(domains, record)
	if cfg == nil {
		return nil
	}
	if hosts, _ := cfg["hosts"].(map[string]any); hosts != nil {
		if override, _ := hosts[host].(map[string]any); override != nil {
			if ids := stringList(override["ca"]); len(ids) > 0 {
				return ids
			}
		}
	}
	return stringList(cfg["ca"])
}

func stringList(v any) []string {
	switch list := v.(type) {
	case string:
		if list != "" {
			return []string{list}
		}
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func anyList(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func optionsOf(v any) map[string]any {
	opts, _ := v.(map[string]any)
	if opts == nil {
		return map[string]any{}
	}
	return opts
}

// cleanHost validates a host argument. www. is the same host to the proxy.
func cleanHost(raw any) (string, string) {
	host, errMsg := validate(raw)
	if errMsg != "" {
		return "", errMsg
	}
	return strings.TrimPrefix(host, "www."), ""
}

func cleanFingerprint(raw any) (string, string) {
	fingerprint, _ := raw.(string)
	fingerprint = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
	if !fingerprintRe.MatchString(fingerprint) {
		return "", __("Invalid certificate fingerprint.")
	}
	return fingerprint, ""
}

func (m *MTLS) saveLocked() {
	if err := m.cfg.SaveDirty(); err != nil {
		m.log.Error("Failed to persist client certificate state: %s", err.Error())
	}
}

// Set changes a host's mode. On a record's own name it applies to the record and
// "off" clears the record entirely, so one command is always enough to recover
// from a lockout. On a subdomain it stores an override. opts may carry "ca" (CA
// ids, replacing the trusted set) and "caPem" (an external CA to add).
func (m *MTLS) Set(domainArg, modeArg, optsArg any) api.Result {
	host, errMsg := cleanHost(domainArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}
	mode, _ := modeArg.(string)
	if mode != mtls.ModeOff && mode != mtls.ModeOptional && mode != mtls.ModeRequired {
		return api.Res(false, __("Mode must be required, optional or off."))
	}
	opts := optionsOf(optsArg)

	m.mu.Lock()
	result, changed := m.setLocked(host, mode, opts)
	m.mu.Unlock()

	if changed && m.proxy != nil {
		m.proxy.SyncConfig()
	}
	return result
}

func (m *MTLS) setLocked(host, mode string, opts map[string]any) (api.Result, bool) {
	var external *x509.Certificate
	var externalPEM string
	if raw, _ := opts["caPem"].(string); raw != "" {
		cert, cleaned, errText := parseExternalCA(raw)
		if errText != "" {
			return api.Res(false, errText), false
		}
		external, externalPEM = cert, cleaned
	}
	requested := stringList(opts["ca"])

	var record string
	var found, unknownCA bool
	var appID any
	var trusted []string
	var hadPolicy bool
	var published []string
	var siblings []string
	m.cfg.View(func() {
		domains := m.domainsLocked()
		record, found = locateHost(domains, host)
		if !found {
			return
		}
		rec, _ := domains[record].(map[string]any)
		appID = rec["appId"]
		hadPolicy = mtls.AppIDs(domains)[fmt.Sprint(appID)]
		trusted = effectiveCAs(domains, record, host)
		cas := m.casLocked(false)
		for _, id := range requested {
			if _, ok := cas[id]; !ok {
				unknownCA = true
			}
		}
		published, siblings = m.bypassesLocked(domains, appID, record)
	})
	if !found {
		return api.Res(false, __("Domain %s not found.", host)), false
	}
	if record == "localhost" || ipv4Re.MatchString(record) || net.ParseIP(record) != nil {
		return api.Res(false, __("%s has no TLS, so it cannot ask for a client certificate.", record)), false
	}
	if strings.HasPrefix(host, "*.") && host != record {
		return api.Res(false, __("Set the mode on %s: a wildcard is part of that record.", strings.TrimPrefix(host, "*."))), false
	}
	if unknownCA {
		return api.Res(false, __("Unknown CA.")), false
	}

	if mode == mtls.ModeOff {
		m.cfg.Mutate(func() {
			m.writeOffLocked(record, host)
			m.cfg.Touch("domains")
		})
		m.saveLocked()
		m.log.Log("Client certificates turned off for %s", host)
		return api.Res(true, __("Client certificates are off for %s.", host), setReply(nil, nil, false)), true
	}

	if tunnels := m.tunnelsFor(appID); len(tunnels) > 0 {
		return api.Res(false, __("App %s is exposed through a tunnel (%s), which reaches it directly and would bypass client certificates. Remove the tunnel first.", str(appID), strings.Join(tunnels, ", "))), false
	}

	if len(requested) > 0 {
		trusted = requested
	}
	if external != nil {
		id := mtls.Fingerprint(external.Raw)[:16]
		m.cfg.Mutate(func() {
			cas := m.casLocked(true)
			if _, exists := cas[id]; !exists {
				cas[id] = map[string]any{
					"cert":    externalPEM,
					"created": float64(m.now().UnixMilli()),
					"managed": false,
					"name":    external.Subject.CommonName,
				}
			}
			m.cfg.Touch("mtls")
		})
		if !contains(trusted, id) {
			trusted = append(append([]string(nil), trusted...), id)
		}
	}
	if len(trusted) == 0 {
		// Checked before creating anything: a refused attempt must not leave a CA behind.
		if mode == mtls.ModeRequired {
			return api.Res(false, __("No certificate can open %s yet, so requiring one would lock everyone out. Issue one first: odac domain mtls issue -d %s --name <name> --hosts %s", host, host, host)), false
		}
		id, err := m.createCA(record)
		if err != nil {
			m.log.Error("Failed to create a client CA for %s: %s", record, err.Error())
			return api.Res(false, __("Failed to create a client CA.")), false
		}
		trusted = []string{id}
	}

	if mode == mtls.ModeRequired && !m.canEnter(record, host, trusted) {
		return api.Res(false, __("No certificate can open %s yet, so requiring one would lock everyone out. Issue one first: odac domain mtls issue -d %s --name <name> --hosts %s", host, host, host)), false
	}

	overrideCA := len(requested) > 0 || external != nil
	m.cfg.Mutate(func() {
		m.writeModeLocked(record, host, mode, trusted, overrideCA)
		m.cfg.Touch("domains")
	})
	m.saveLocked()
	m.log.Log("Client certificates set to %s for %s", mode, host)

	var notes []string
	notes = append(notes, __("Client certificates are %s for %s.", mode, host))
	if len(published) > 0 {
		notes = append(notes, __("Warning: %s publishes host port(s) %s, which reach it without passing the proxy and are not protected.", str(appID), strings.Join(published, ", ")))
	}
	if len(siblings) > 0 {
		notes = append(notes, __("Warning: %s is also reachable with no certificate through %s.", str(appID), strings.Join(siblings, ", ")))
	}
	if !hadPolicy {
		notes = append(notes, __("Restart %s so it receives %s and can verify the signed identity headers.", str(appID), mtls.KeyEnv))
	}
	return api.Res(true, strings.Join(notes, " "), setReply(published, siblings, !hadPolicy)), true
}

// setReply carries the warnings of a mode change as data, next to the message
// that spells them out, so a dashboard can show each one on its own and offer the
// restart instead of parsing a sentence. Empty lists mean nothing to warn about.
func setReply(published, siblings []string, restart bool) map[string]any {
	ports := []int{}
	for _, raw := range published {
		if port, err := strconv.Atoi(raw); err == nil && port > 0 {
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	hosts := append([]string{}, siblings...)
	return map[string]any{"published": ports, "siblings": hosts, "restart": restart}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func (m *MTLS) tunnelsFor(appID any) []string {
	if m.proxy == nil {
		return nil
	}
	return m.proxy.TunnelDomains(str(appID))
}

// bypassesLocked lists what stays reachable around a policy on this app, so the
// caller hears it at the moment it is relevant rather than discovering it later.
func (m *MTLS) bypassesLocked(domains map[string]any, appID any, record string) (published, siblings []string) {
	apps, _ := m.cfg.Get("apps").([]any)
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app == nil || (app["name"] != appID && app["id"] != appID) {
			continue
		}
		list, _ := app["ports"].([]any)
		for _, p := range list {
			entry, _ := p.(map[string]any)
			if ports.IsPublished(entry) {
				published = append(published, str(entry["host"]))
			}
		}
	}
	for name, rec := range domains {
		other, _ := rec.(map[string]any)
		if other == nil || other["appId"] != appID || name == record {
			continue
		}
		cfg, _ := other["mtls"].(map[string]any)
		if cfg == nil || cfg["mode"] == nil {
			siblings = append(siblings, name)
		}
	}
	sort.Strings(published)
	sort.Strings(siblings)
	return published, siblings
}

// canEnter reports whether requiring a certificate on host would leave anyone
// able to get in. An external CA issues certificates ODAC cannot see, so it
// counts as yes. On a record's own name any live certificate scoped to one of
// its hosts counts, since the point is to stop an empty rollout, not to police
// which host each person holds.
func (m *MTLS) canEnter(record, host string, trusted []string) bool {
	external := false
	m.cfg.View(func() {
		cas := m.casLocked(false)
		for _, id := range trusted {
			ca, _ := cas[id].(map[string]any)
			if ca != nil && ca["managed"] != true {
				external = true
				return
			}
		}
	})
	if external {
		return true
	}

	store, err := m.certs()
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return false
	}
	now := float64(m.now().UnixMilli())
	match := func(hosts []string) bool {
		if host != record {
			return mtls.Covers(hosts, host)
		}
		found := false
		m.cfg.View(func() {
			domains := m.domainsLocked()
			for _, h := range hosts {
				if owner, ok := locateHost(domains, h); ok && owner == record {
					found = true
					return
				}
			}
		})
		return found
	}
	for _, id := range trusted {
		can, err := store.anyLive(id, now, match)
		if err != nil {
			m.log.Error("Client certificate store failed: %s", err.Error())
			return false
		}
		if can {
			return true
		}
	}
	return false
}

func (m *MTLS) writeModeLocked(record, host, mode string, trusted []string, overrideCA bool) {
	rec, _ := m.domainsLocked()[record].(map[string]any)
	if rec == nil {
		return
	}
	cfg, _ := rec["mtls"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
		rec["mtls"] = cfg
	}
	if host == record {
		cfg["mode"] = mode
		cfg["ca"] = anyList(trusted)
		return
	}
	hosts, _ := cfg["hosts"].(map[string]any)
	if hosts == nil {
		hosts = map[string]any{}
		cfg["hosts"] = hosts
	}
	override := map[string]any{"mode": mode}
	if overrideCA {
		override["ca"] = anyList(trusted)
	} else if stringList(cfg["ca"]) == nil {
		cfg["ca"] = anyList(trusted)
	}
	hosts[host] = override
}

func (m *MTLS) writeOffLocked(record, host string) {
	rec, _ := m.domainsLocked()[record].(map[string]any)
	cfg, _ := rec["mtls"].(map[string]any)
	if cfg == nil {
		return
	}
	if host == record {
		delete(cfg, "mode")
		delete(cfg, "hosts")
	} else {
		hosts, _ := cfg["hosts"].(map[string]any)
		delete(hosts, host)
		// Under a record-wide mode, removing the override would apply that mode.
		if cfg["mode"] != nil {
			if hosts == nil {
				hosts = map[string]any{}
				cfg["hosts"] = hosts
			}
			hosts[host] = map[string]any{"mode": mtls.ModeOff}
		} else if len(hosts) == 0 {
			delete(cfg, "hosts")
		}
	}
	if len(stringList(cfg["ca"])) == 0 && cfg["mode"] == nil && cfg["hosts"] == nil {
		delete(rec, "mtls")
	}
}

// parseExternalCA checks a CA the user brings. A public root is refused because
// it would admit anyone holding a certificate from it, and a private key in the
// paste is refused rather than silently ignored because it should not be here.
func parseExternalCA(raw string) (*x509.Certificate, string, string) {
	if strings.Contains(raw, "PRIVATE KEY") {
		return nil, "", __("That file contains a private key. Send only the CA certificate.")
	}
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", __("Invalid CA certificate.")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", __("Invalid CA certificate.")
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return nil, "", __("That certificate is not a CA.")
	}
	if time.Now().After(cert.NotAfter) {
		return nil, "", __("That CA certificate has expired.")
	}
	if roots, err := x509.SystemCertPool(); err == nil && roots != nil {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
			return nil, "", __("That is a public CA: anyone holding a certificate from it would be let in. Use a private CA.")
		}
	}
	return cert, string(pem.EncodeToMemory(block)), ""
}

// createCA generates a managed CA. The key is written before the record so a
// crash can only orphan a file, never leave a CA nothing can sign with.
func (m *MTLS) createCA(domain string) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := randomSerial()
	if err != nil {
		return "", err
	}
	now := m.now()
	name := "ODAC Client CA " + strings.TrimPrefix(domain, "*.")
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(mtlsCALifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	id := mtls.Fingerprint(der)[:16]
	if err := writeSecret(m.caKeyPath(id), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return "", err
	}
	m.cfg.Mutate(func() {
		cas := m.casLocked(true)
		cas[id] = map[string]any{
			"cert":    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			"created": float64(now.UnixMilli()),
			"managed": true,
			"name":    name,
		}
		m.cfg.Touch("mtls")
	})
	m.log.Log("Created client CA %s for %s", id, domain)
	return id, nil
}

func (m *MTLS) caKeyPath(id string) string {
	return filepath.Join(m.certDir, id+".key")
}

func writeSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// loadCA reads a managed CA's certificate and signing key.
func (m *MTLS) loadCA(id string) (*x509.Certificate, any, string, error) {
	var certPEM string
	m.cfg.View(func() {
		ca, _ := m.casLocked(false)[id].(map[string]any)
		certPEM, _ = ca["cert"].(string)
	})
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, nil, "", errors.New("CA certificate is missing")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, "", err
	}
	raw, err := os.ReadFile(m.caKeyPath(id))
	if err != nil {
		return nil, nil, "", err
	}
	keyBlock, _ := pem.Decode(raw)
	if keyBlock == nil {
		return nil, nil, "", errors.New("CA key is unreadable")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, "", err
	}
	return cert, key, certPEM, nil
}

func parseCSR(raw string) (*x509.CertificateRequest, string) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, __("Invalid certificate request.")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return nil, __("Invalid certificate request.")
	}
	switch key := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
			return nil, __("Unsupported key type in the certificate request.")
		}
	case *rsa.PublicKey:
		if key.N.BitLen() < mtlsMinRSABits {
			return nil, __("RSA keys must be at least 2048 bits.")
		}
	case ed25519.PublicKey:
	default:
		return nil, __("Unsupported key type in the certificate request.")
	}
	return csr, ""
}

// validName keeps a certificate name usable as a login identity: it ends up in
// the subject apps match on.
func validName(name string) bool {
	if name == "" || len(name) > mtlsMaxNameLen || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// cleanScope validates a host list against what a CA may open. A scope entry
// must name a host that exists and trusts this CA, otherwise the certificate
// would look broader in the dashboard than it is. No entries is an error: the
// default must be nothing, and a certificate opening nothing should not be issued.
// For an app (app != "") every host must also be routed to it, and one that is
// not reads as unregistered, so the answer does not reveal another app's hosts.
func cleanScope(domains map[string]any, raw any, caID, app string) ([]string, string) {
	entries := stringList(raw)
	if len(entries) == 0 {
		return nil, __("At least one host is required.")
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range entries {
		host, errMsg := cleanHost(entry)
		if errMsg != "" {
			return nil, errMsg
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		record, ok := locateHost(domains, host)
		if !ok || !ownedLocked(domains, record, app) {
			return nil, __("Host %s is not registered.", host)
		}
		if !contains(effectiveCAs(domains, record, host), caID) {
			return nil, __("Host %s does not trust this certificate's CA.", host)
		}
		out = append(out, host)
	}
	sort.Strings(out)
	return out, ""
}

// Issue signs a CSR for a domain. Only the CSR's public key is used: subject,
// usage and lifetime are decided here, because a CSR's extensions are whatever
// its author asked for. A repeated request returns the certificate it already
// produced, since the reply can be lost after signing and the caller still holds
// the key it would retry with. opts carries name, hosts, csr, days and renews (the
// fingerprint being replaced, which keeps the old certificate valid until it is
// revoked). A renewal may keep its key, so it is repeated only if it names the
// same certificate and its earlier result has not expired. An app may issue
// only on records routed to it, for hosts routed to it, and only where a CA is
// already set up: creating one is the operator's decision.
func (m *MTLS) Issue(caller api.Caller, domainArg, optsArg any) api.Result {
	app, ok := reach(caller)
	if !ok {
		return permissionDenied()
	}
	host, errMsg := cleanHost(domainArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}
	opts := optionsOf(optsArg)
	rawCSR, _ := opts["csr"].(string)
	csr, errMsg := parseCSR(rawCSR)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}

	m.mu.Lock()
	result, signed, newCA := m.issueLocked(app, host, csr, opts)
	m.mu.Unlock()

	if signed && m.proxy != nil {
		if newCA {
			m.proxy.SyncConfig()
		} else {
			m.proxy.SyncClientCerts()
		}
	}
	return result
}

// issueLocked reports whether it signed a certificate, and whether that took a new
// CA, which changes a domain record and not only the certificates.
func (m *MTLS) issueLocked(app, host string, csr *x509.CertificateRequest, opts map[string]any) (result api.Result, signed, newCA bool) {
	var record string
	var found bool
	var caID string
	var appID any
	var hasExternal bool
	m.cfg.View(func() {
		domains := m.domainsLocked()
		record, found = locateHost(domains, host)
		if found && !ownedLocked(domains, record, app) {
			found = false
		}
		if !found {
			return
		}
		rec, _ := domains[record].(map[string]any)
		appID = rec["appId"]
		cas := m.casLocked(false)
		for _, id := range effectiveCAs(domains, record, host) {
			ca, _ := cas[id].(map[string]any)
			if ca == nil {
				continue
			}
			if ca["managed"] == true {
				caID = id
				return
			}
			hasExternal = true
		}
	})
	if !found {
		return api.Res(false, __("Domain %s not found.", host)), false, false
	}
	if caID == "" && hasExternal {
		return api.Res(false, __("%s trusts an external CA. Issue certificates from that CA.", host)), false, false
	}
	if tunnels := m.tunnelsFor(appID); len(tunnels) > 0 {
		return api.Res(false, __("App %s is exposed through a tunnel (%s), which reaches it directly and would bypass client certificates. Remove the tunnel first.", str(appID), strings.Join(tunnels, ", "))), false, false
	}
	if caID == "" && app != "" {
		return api.Res(false, __("Client certificates are not set up on %s.", host)), false, false
	}
	if caID == "" {
		id, err := m.createCA(record)
		if err != nil {
			m.log.Error("Failed to create a client CA for %s: %s", record, err.Error())
			return api.Res(false, __("Failed to create a client CA.")), false, false
		}
		caID = id
		newCA = true
		m.cfg.Mutate(func() {
			rec, _ := m.domainsLocked()[record].(map[string]any)
			if rec == nil {
				return
			}
			cfg, _ := rec["mtls"].(map[string]any)
			if cfg == nil {
				cfg = map[string]any{}
				rec["mtls"] = cfg
			}
			cfg["ca"] = []any{caID}
			m.cfg.Touch("domains")
		})
	}

	store, err := m.certs()
	if err != nil {
		return m.storeFailure(err), false, false
	}

	spki := mtls.Fingerprint(csr.RawSubjectPublicKeyInfo)

	name, _ := opts["name"].(string)
	name = strings.TrimSpace(name)
	nowMillis := float64(m.now().UnixMilli())
	rawHosts := opts["hosts"]
	var renewedFrom string
	if renews, _ := opts["renews"].(string); renews != "" {
		fingerprint, errMsg := cleanFingerprint(renews)
		if errMsg != "" {
			return api.Res(false, errMsg), false, false
		}
		old, err := store.get(fingerprint)
		if err != nil {
			return m.storeFailure(err), false, false
		}
		if old == nil || old["ca"] != caID || old["revoked"] != nil {
			return api.Res(false, __("The certificate being renewed was not found.")), false, false
		}
		renewedFrom = fingerprint
		name = str(old["name"])
		if len(stringList(rawHosts)) == 0 {
			rawHosts = old["hosts"]
		}
	} else if !validName(name) {
		return api.Res(false, __("A certificate name of 1 to 64 printable characters is required.")), false, false
	}

	onKey, err := store.byKey(caID, spki)
	if err != nil {
		return m.storeFailure(err), false, false
	}
	existingFP, failure := matchKey(onKey, caID, spki, name, renewedFrom, nowMillis)
	if failure != "" {
		return api.Res(false, failure), false, false
	}
	if existingFP != "" {
		existing, _ := onKey[existingFP].(map[string]any)
		return api.Res(true, __("Certificate %s already exists.", str(existing["name"])), issueReply(existingFP, existing, caID, m.caPEM(caID))), false, false
	}

	if renewedFrom == "" {
		// Apps match on issuer and subject, so two people must not share a name.
		taken, err := store.nameTaken(caID, name)
		if err != nil {
			return m.storeFailure(err), false, false
		}
		if taken {
			return api.Res(false, __("A certificate named %s already exists. Revoke it, or renew it.", name)), false, false
		}
	}

	var hosts []string
	m.cfg.View(func() {
		hosts, failure = cleanScope(m.domainsLocked(), rawHosts, caID, app)
	})
	if failure != "" {
		return api.Res(false, failure), false, false
	}

	days := mtlsDefaultCertDays
	if v, ok := opts["days"].(float64); ok {
		days = int(v)
	}
	if days < 1 || days > mtlsMaxCertDays {
		return api.Res(false, __("Lifetime must be between 1 and 3650 days.")), false, false
	}

	caCert, caKey, caPEM, err := m.loadCA(caID)
	if err != nil {
		m.log.Error("Failed to load client CA %s: %s", caID, err.Error())
		return api.Res(false, __("Failed to load the client CA.")), false, false
	}
	serial, err := randomSerial()
	if err != nil {
		return api.Res(false, __("Failed to sign the certificate.")), false, false
	}
	now := m.now()
	notAfter := now.Add(time.Duration(days) * 24 * time.Hour)
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		m.log.Error("Failed to sign a client certificate: %s", err.Error())
		return api.Res(false, __("Failed to sign the certificate.")), false, false
	}

	fingerprint := mtls.Fingerprint(der)
	record2 := map[string]any{
		"ca":       caID,
		"cert":     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"created":  float64(now.UnixMilli()),
		"hosts":    anyList(hosts),
		"name":     name,
		"notAfter": float64(notAfter.UnixMilli()),
		"serial":   hex.EncodeToString(serial.Bytes()),
		"spki":     spki,
	}
	if renewedFrom != "" {
		record2["renewedFrom"] = renewedFrom
	}
	if err := store.insert(fingerprint, record2); err != nil {
		return m.storeFailure(err), false, false
	}
	m.saveLocked()
	m.log.Log("Issued client certificate %s (%s)", fingerprint[:16], logSafeName(name))
	return api.Res(true, __("Certificate %s issued.", name), issueReply(fingerprint, record2, caID, caPEM)), true, newCA
}

// matchKey decides what a CSR's key may get. A renewal can keep its key, so one
// key can hold several records and every one of them has to be read: map order
// must not choose between returning a certificate and signing another. It returns
// the certificate a retry should receive, or a refusal, or neither to sign.
func matchKey(certs map[string]any, caID, spki, name, renewedFrom string, now float64) (fingerprint, refused string) {
	var seen, live, otherName bool
	var newest float64
	for candidate, rec := range certs {
		cert, _ := rec.(map[string]any)
		if cert == nil || cert["ca"] != caID || cert["spki"] != spki {
			continue
		}
		seen = true
		if cert["revoked"] != nil {
			continue
		}
		live = true
		if cert["name"] != name {
			otherName = true
			continue
		}
		notAfter, _ := cert["notAfter"].(float64)
		if notAfter <= now || (renewedFrom != "" && cert["renewedFrom"] != renewedFrom) {
			continue
		}
		created, _ := cert["created"].(float64)
		if fingerprint == "" || created > newest || (created == newest && candidate > fingerprint) {
			fingerprint, newest = candidate, created
		}
	}
	switch {
	case !seen:
		return "", ""
	case !live:
		return "", __("This key belongs to a revoked certificate. Generate a new key.")
	case otherName:
		return "", __("This key belongs to another certificate. Generate a new key.")
	case fingerprint == "" && renewedFrom == "":
		return "", __("The certificate for this key has expired. Renew it.")
	}
	return fingerprint, ""
}

func logSafeName(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}

func (m *MTLS) caPEM(id string) string {
	var out string
	m.cfg.View(func() {
		ca, _ := m.casLocked(false)[id].(map[string]any)
		out, _ = ca["cert"].(string)
	})
	return out
}

func issueReply(fingerprint string, cert map[string]any, caID, caPEM string) map[string]any {
	return map[string]any{
		"ca":          caID,
		"cert":        cert["cert"],
		"chain":       caPEM,
		"fingerprint": fingerprint,
		"hosts":       cert["hosts"],
		"name":        cert["name"],
		"notAfter":    cert["notAfter"],
		"serial":      cert["serial"],
	}
}

// Scope replaces the hosts a certificate opens. It lives on the server rather
// than in the certificate so a change needs no reissue and lands on the next request.
func (m *MTLS) Scope(fingerprintArg, hostsArg any) api.Result {
	fingerprint, errMsg := cleanFingerprint(fingerprintArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}

	m.mu.Lock()
	store, err := m.certs()
	if err != nil {
		m.mu.Unlock()
		return m.storeFailure(err)
	}
	cert, err := store.get(fingerprint)
	if err != nil {
		m.mu.Unlock()
		return m.storeFailure(err)
	}
	if cert == nil || cert["revoked"] != nil {
		m.mu.Unlock()
		return api.Res(false, __("Certificate not found."))
	}
	name := str(cert["name"])
	var failure string
	var hosts []string
	m.cfg.View(func() {
		hosts, failure = cleanScope(m.domainsLocked(), hostsArg, str(cert["ca"]), "")
	})
	if failure != "" {
		m.mu.Unlock()
		return api.Res(false, failure)
	}
	if err := store.setHosts(fingerprint, hosts); err != nil {
		m.mu.Unlock()
		return m.storeFailure(err)
	}
	m.mu.Unlock()

	m.log.Log("Changed the scope of client certificate %s", fingerprint[:16])
	if m.proxy != nil {
		m.proxy.SyncClientCerts()
	}
	return api.Res(true, __("Certificate %s now opens %s.", name, strings.Join(hosts, ", ")))
}

// Revoke ends a certificate on the next request and cuts its open connections.
// It accepts a fingerprint ODAC has no record of, which is the only way to
// revoke something an external CA issued. Revoking twice succeeds both times,
// so a caller retrying a lost reply cannot end up stuck. An app may revoke only
// a recorded certificate it owns (ownsCertLocked): the unknown-fingerprint branch
// would let it plant a revocation for a certificate it never saw, and anything
// it may not touch answers not_found.
func (m *MTLS) Revoke(caller api.Caller, fingerprintArg any) api.Result {
	app, ok := reach(caller)
	if !ok {
		return permissionDenied()
	}
	fingerprint, errMsg := cleanFingerprint(fingerprintArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}

	m.mu.Lock()
	store, err := m.certs()
	if err != nil {
		m.mu.Unlock()
		return m.storeFailure(err)
	}
	if app != "" {
		cert, err := store.get(fingerprint)
		if err != nil {
			m.mu.Unlock()
			return m.storeFailure(err)
		}
		if !m.ownsCert(app, cert) {
			m.mu.Unlock()
			return notFoundForApp()
		}
	}
	now := float64(m.now().UnixMilli())
	if err := store.revoke(fingerprint, now); err != nil {
		m.mu.Unlock()
		return m.storeFailure(err)
	}
	sameKey, err := sameKeyAs(store, fingerprint, now)
	if err == nil && app != "" {
		sameKey, err = m.ownedOnly(store, app, sameKey)
	}
	m.mu.Unlock()
	if err != nil {
		// The revocation itself is stored, so it still has to reach the proxy.
		m.log.Error("Client certificate store failed: %s", err.Error())
	}

	m.log.Log("Revoked client certificate %s", fingerprint[:16])
	if m.proxy != nil {
		m.proxy.SyncClientCerts()
	}
	if len(sameKey) > 0 {
		list := make([]string, len(sameKey))
		for i, fp := range sameKey {
			list[i] = str(fp)
		}
		return api.Res(true, __("Certificate revoked. The same key still works with %s. Revoke those as well if the key was exposed.", strings.Join(list, ", ")), map[string]any{"sameKey": sameKey})
	}
	return api.Res(true, __("Certificate revoked."))
}

// ownsCert is ownsCertLocked for a record read from the store. A missing record
// is nobody's.
func (m *MTLS) ownsCert(app string, cert map[string]any) bool {
	if cert == nil {
		return false
	}
	owned := false
	m.cfg.View(func() {
		domains := m.domainsLocked()
		owned = ownsCertLocked(domains, app, appCAsLocked(domains, app), cert)
	})
	return owned
}

// ownedOnly keeps the fingerprints app owns, so a reply never names another
// app's certificate that happens to share a key and a CA.
func (m *MTLS) ownedOnly(store *certStore, app string, fingerprints []any) ([]any, error) {
	var out []any
	for _, fp := range fingerprints {
		cert, err := store.get(str(fp))
		if err != nil {
			return nil, err
		}
		if m.ownsCert(app, cert) {
			out = append(out, fp)
		}
	}
	return out, nil
}

func sameKeyAs(store *certStore, fingerprint string, now float64) ([]any, error) {
	cert, err := store.get(fingerprint)
	if err != nil || cert == nil {
		return nil, err
	}
	onKey, err := store.byKey(str(cert["ca"]), str(cert["spki"]))
	if err != nil {
		return nil, err
	}
	return liveOnSameKey(onKey, fingerprint, now), nil
}

// liveOnSameKey lists other certificates that still work with a certificate's
// key. Revoking one on purpose leaves its renewal alone, since retiring the old
// certificate is the normal end of a renewal, so a caller who revoked because the
// key leaked has to be told what remains.
func liveOnSameKey(certs map[string]any, fingerprint string, now float64) []any {
	cert, _ := certs[fingerprint].(map[string]any)
	spki, _ := cert["spki"].(string)
	if spki == "" {
		return nil
	}
	var out []any
	for _, candidate := range sortedKeys(certs) {
		other, _ := certs[candidate].(map[string]any)
		if candidate == fingerprint || other == nil || other["ca"] != cert["ca"] || other["spki"] != spki || other["revoked"] != nil {
			continue
		}
		if notAfter, _ := other["notAfter"].(float64); notAfter > now {
			out = append(out, candidate)
		}
	}
	return out
}

// List returns certificate records, optionally only those a domain's record
// trusts. PEMs are left out: this runs on the hub's interval. An app sees only
// the certificates it owns (ownsCertLocked), and its domain filter must name one
// of its own records.
func (m *MTLS) List(caller api.Caller, domainArg any) api.Result {
	app, ok := reach(caller)
	if !ok {
		return permissionDenied()
	}
	filter := ""
	if s, ok := domainArg.(string); ok {
		norm := strings.ToLower(strings.TrimSpace(s))
		if norm != "" && norm != "undefined" && norm != "null" {
			filter = strings.TrimPrefix(norm, "www.")
		}
	}

	var failure string
	var ids []string
	m.cfg.View(func() {
		domains := m.domainsLocked()
		if filter == "" {
			if app != "" {
				ids = sortedSet(appCAsLocked(domains, app))
			}
			return
		}
		record, ok := locateHost(domains, filter)
		if !ok || !ownedLocked(domains, record, app) {
			failure = __("Domain %s not found.", filter)
			return
		}
		ids = []string{}
		for _, id := range stringList(recordConfig(domains, record)["ca"]) {
			if !contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if hosts, _ := recordConfig(domains, record)["hosts"].(map[string]any); hosts != nil {
			for _, h := range hosts {
				override, _ := h.(map[string]any)
				for _, id := range stringList(override["ca"]) {
					if !contains(ids, id) {
						ids = append(ids, id)
					}
				}
			}
		}
	})
	if failure != "" {
		return api.Res(false, failure)
	}
	if ids != nil && len(ids) == 0 {
		return api.Res(true, []any{})
	}

	store, err := m.certs()
	if err != nil {
		return m.storeFailure(err)
	}
	certs, err := store.list(ids)
	if err != nil {
		return m.storeFailure(err)
	}

	rows := make([]any, 0, len(certs))
	m.cfg.View(func() {
		domains := m.domainsLocked()
		var owned map[string]bool
		if app != "" {
			owned = appCAsLocked(domains, app)
		}
		for _, row := range certs {
			if !ownsCertLocked(domains, app, owned, row) {
				continue
			}
			rows = append(rows, m.withCANameLocked(row))
		}
	})
	return api.Res(true, rows)
}

// withCANameLocked adds the name of a row's CA, which a dashboard shows next to
// the certificate.
func (m *MTLS) withCANameLocked(row map[string]any) map[string]any {
	if caID, _ := row["ca"].(string); caID != "" {
		if ca, _ := m.casLocked(false)[caID].(map[string]any); ca != nil {
			row["caName"] = ca["name"]
		}
	}
	return row
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Prune drops what a deleted domain leaves behind. Without it, a domain added
// again later, possibly for someone else's app, would be opened by certificates
// issued for its previous owner. It runs on Domain's delete paths, which sync
// the proxy afterwards.
func (m *MTLS) Prune() {
	m.mu.Lock()
	defer m.mu.Unlock()

	store, err := m.certs()
	var scopes []certScope
	if err == nil {
		scopes, err = store.scopes()
	}

	var orphanCAs []string
	var remove []string
	trim := map[string][]string{}
	m.cfg.Mutate(func() {
		domains := m.domainsLocked()
		cas := m.casLocked(false)
		if cas == nil && len(scopes) == 0 {
			return
		}

		referenced := map[string]bool{}
		for name, rec := range domains {
			record, _ := rec.(map[string]any)
			cfg, _ := record["mtls"].(map[string]any)
			if cfg == nil {
				continue
			}
			for _, id := range stringList(cfg["ca"]) {
				referenced[id] = true
			}
			hosts, _ := cfg["hosts"].(map[string]any)
			for host, h := range hosts {
				if owner, ok := locateHost(domains, host); !ok || owner != name {
					delete(hosts, host)
					continue
				}
				override, _ := h.(map[string]any)
				for _, id := range stringList(override["ca"]) {
					referenced[id] = true
				}
			}
			if hosts != nil && len(hosts) == 0 {
				delete(cfg, "hosts")
			}
		}

		for _, entry := range scopes {
			if entry.ca != "" && !referenced[entry.ca] {
				remove = append(remove, entry.fingerprint)
				continue
			}
			kept := []string{}
			for _, host := range entry.hosts {
				if _, ok := locateHost(domains, host); ok {
					kept = append(kept, host)
				}
			}
			if entry.hasHosts && len(kept) != len(entry.hosts) {
				trim[entry.fingerprint] = kept
			}
		}
		for id := range cas {
			if !referenced[id] {
				orphanCAs = append(orphanCAs, id)
			}
		}
		m.cfg.Touch("domains")
	})

	if err == nil {
		err = store.applyPrune(remove, trim)
	}
	if err != nil {
		// Every decision above comes from the current domains, so the next delete
		// repeats this. Certificates of a CA we kept stay refused or scoped as before.
		m.log.Error("Client certificate store failed: %s", err.Error())
		m.saveLocked()
		return
	}

	if len(orphanCAs) > 0 {
		m.cfg.Mutate(func() {
			cas := m.casLocked(false)
			for _, id := range orphanCAs {
				delete(cas, id)
			}
			m.cfg.Touch("mtls")
		})
	}
	for _, id := range orphanCAs {
		// A CA nothing references can only be used to mint trouble later.
		if err := os.Remove(m.caKeyPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Error("Failed to remove client CA key %s: %s", id, err.Error())
		}
		m.log.Log("Removed client CA %s", id)
	}
	m.saveLocked()
}
