package domains

import (
	"bytes"
	"crypto"
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

// mtlsListView is a record's mtls object as domain.list reports it: a copy made
// under the config lock, since the rows are encoded after it is released, with
// `external` naming the CAs of each `ca` list that ODAC holds no key for. A
// dashboard needs that to know a host may be required with no certificate record
// behind it, which canEnter allows for an external CA. The field is computed, never
// stored, and left out when empty because the dashboard diffs these rows.
func mtlsListView(raw any, cas map[string]any) any {
	cfg, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	out := withExternal(cfg, cas)
	if hosts, _ := cfg["hosts"].(map[string]any); hosts != nil {
		copied := make(map[string]any, len(hosts))
		for host, h := range hosts {
			if override, ok := h.(map[string]any); ok {
				copied[host] = withExternal(override, cas)
			} else {
				copied[host] = h
			}
		}
		out["hosts"] = copied
	}
	return out
}

func withExternal(obj, cas map[string]any) map[string]any {
	out := make(map[string]any, len(obj)+1)
	for key, value := range obj {
		out[key] = value
	}
	delete(out, "external")
	if ca, ok := obj["ca"].([]any); ok {
		out["ca"] = append([]any(nil), ca...)
	}
	var external []any
	for _, id := range stringList(obj["ca"]) {
		if ca, _ := cas[id].(map[string]any); ca != nil && ca["managed"] != true {
			external = append(external, id)
		}
	}
	if len(external) > 0 {
		out["external"] = external
	}
	return out
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
// ids, replacing the trusted set), "caPem" (an external CA to add) and, on a
// record's own name, "issuer" (mtls.IssuerRecord or mtls.IssuerPerHost; absent
// keeps the current one).
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
	issuer := ""
	if raw := opts["issuer"]; raw != nil && raw != any("") {
		issuer, _ = raw.(string)
		if issuer != mtls.IssuerRecord && issuer != mtls.IssuerPerHost {
			return api.Res(false, __("Issuer must be record or per-host.")), false
		}
	}

	var record string
	var found, unknownCA bool
	var appID any
	var trusted []string
	var hadPolicy bool
	var published []string
	var siblings []string
	var wasPerHost bool
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
		wasPerHost = recordConfig(domains, record)["issuer"] == mtls.IssuerPerHost
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
	if issuer != "" && host != record {
		return api.Res(false, __("The issuer is a setting of the whole record. Set it on %s.", strings.TrimPrefix(record, "*."))), false
	}
	if issuer != "" && mode == mtls.ModeOff {
		return api.Res(false, __("Set the issuer together with --mode optional or required.")), false
	}
	perHost := issuer == mtls.IssuerPerHost || (issuer == "" && wasPerHost)

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
	if perHost {
		if refusal := m.perHostRefusal(record, host, trusted, external != nil); refusal != "" {
			return api.Res(false, refusal), false
		}
	}
	if issuer == mtls.IssuerRecord && wasPerHost {
		if refusal := m.leavePerHostRefusal(record); refusal != "" {
			return api.Res(false, refusal), false
		}
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
		m.writeIssuerLocked(record, issuer)
		m.cfg.Touch("domains")
	})
	m.saveLocked()
	m.log.Log("Client certificates set to %s for %s", mode, host)
	if issuer != "" && (issuer == mtls.IssuerPerHost) != wasPerHost {
		m.log.Log("Client certificate issuer of %s set to %s", record, issuer)
		if store, err := m.certs(); err == nil {
			m.sweepIssuersLocked(store)
		}
	}

	var notes []string
	notes = append(notes, __("Client certificates are %s for %s.", mode, host))
	if issuer == mtls.IssuerPerHost && !wasPerHost {
		notes = append(notes, __("A new certificate for a single host of %s is signed by that host's own issuer. Existing certificates keep working.", record))
	}
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

// writeIssuerLocked stores a record's issuer mode. The default is stored as its
// absence, the same serialization rule as an unset mode.
func (m *MTLS) writeIssuerLocked(record, issuer string) {
	rec, _ := m.domainsLocked()[record].(map[string]any)
	cfg, _ := rec["mtls"].(map[string]any)
	switch {
	case cfg == nil:
	case issuer == mtls.IssuerPerHost:
		cfg["issuer"] = mtls.IssuerPerHost
	case issuer == mtls.IssuerRecord:
		delete(cfg, "issuer")
	}
}

// perHostRefusal checks that a per-host record trusts only CAs ODAC holds the key
// for, on the record and on every host override as they will be after this write:
// an issuer is something this server signs, and an external CA's certificates have
// no record to tell a per-host chain from any other. Each CA must also allow one
// intermediate, or every chain through an issuer under it is invalid.
func (m *MTLS) perHostRefusal(record, host string, trusted []string, addsExternal bool) string {
	if addsExternal {
		return __("%s uses per-host issuers, which need a CA ODAC holds the key for. Switch it to --issuer record first to trust an external CA.", record)
	}
	refusal := ""
	m.cfg.View(func() {
		domains := m.domainsLocked()
		ids := append([]string(nil), trusted...)
		cfg := recordConfig(domains, record)
		if host != record {
			ids = append(ids, stringList(cfg["ca"])...)
		}
		hosts, _ := cfg["hosts"].(map[string]any)
		for name, h := range hosts {
			if name == host {
				continue
			}
			override, _ := h.(map[string]any)
			ids = append(ids, stringList(override["ca"])...)
		}
		cas := m.casLocked(false)
		for _, id := range ids {
			ca, _ := cas[id].(map[string]any)
			if ca == nil {
				continue
			}
			if ca["managed"] != true {
				refusal = __("%s uses per-host issuers, which need a CA ODAC holds the key for, and CA %s is external.", record, id)
				return
			}
			certPEM, _ := ca["cert"].(string)
			block, _ := pem.Decode([]byte(certPEM))
			if block == nil {
				continue
			}
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil && !mtls.AllowsHostIssuer(cert) {
				refusal = __("CA %s has a path length of 0, so it cannot sign a per-host issuer. Import a CA that allows one intermediate (basicConstraints pathlen:1), or create a new one, and trust it on %s.", id, record)
				return
			}
		}
	})
	return refusal
}

// leavePerHostRefusal keeps a record per-host while a live certificate was signed
// by one of its hosts' issuers: in record mode nothing accepts that chain, so
// switching would lock its holder out.
func (m *MTLS) leavePerHostRefusal(record string) string {
	store, err := m.certs()
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return __("The certificate store is unavailable.")
	}
	hosts, err := store.liveIssuerHosts(float64(m.now().UnixMilli()))
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return __("The certificate store is unavailable.")
	}
	var blocking []string
	m.cfg.View(func() {
		domains := m.domainsLocked()
		for _, host := range hosts {
			if owner, ok := locateHost(domains, host); ok && owner == record {
				blocking = append(blocking, host)
			}
		}
	})
	if len(blocking) == 0 {
		return ""
	}
	sort.Strings(blocking)
	return __("Certificates for %s were signed by per-host issuers, which only --issuer per-host accepts. Revoke them first, or keep the record per-host.", strings.Join(blocking, ", "))
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
		// One intermediate: room for per-host issuers (mtls.AllowsHostIssuer).
		MaxPathLen: 1,
		KeyUsage:   x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
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

// ImportCA adds a CA together with its signing key as a managed CA, so several
// servers can sign certificates that each of them accepts. The id comes from the
// certificate as it does for every CA, so the CA gets the same id on each server
// it is imported to and a second import changes nothing. It is the operator's
// alone and only the local API registers it: a CA key must never cross the hub
// or reach an app. The key is stored like a created CA's, 0600 and before the
// record, and is never logged or echoed.
func (m *MTLS) ImportCA(caller api.Caller, certArg, keyArg any) api.Result {
	if caller.App != "" || !caller.Root {
		return permissionDenied()
	}
	certPEM, _ := certArg.(string)
	keyPEM, _ := keyArg.(string)
	cert, cleaned, errText := parseExternalCA(certPEM)
	if errText != "" {
		return api.Res(false, errText)
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return api.Res(false, __("That CA certificate is not allowed to sign certificates."))
	}
	key, errText := parseCAKey(keyPEM, cert)
	if errText != "" {
		return api.Res(false, errText)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return api.Res(false, __("Invalid CA key."))
	}
	id := mtls.Fingerprint(cert.Raw)[:16]
	name := strings.TrimSpace(cert.Subject.CommonName)
	if name == "" {
		name = "Client CA " + id
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var existing map[string]any
	m.cfg.View(func() { existing, _ = m.casLocked(false)[id].(map[string]any) })
	if existing != nil && existing["managed"] != true {
		// Managed means a certificate needs a record here, so switching would lock
		// out every certificate that CA issued outside ODAC.
		return api.Res(false, __("CA %s is already trusted here as an external CA, so its certificates need no record. Importing it as managed would refuse every one of them.", id))
	}
	if err := writeSecret(m.caKeyPath(id), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		m.log.Error("Failed to store the key of client CA %s: %s", id, err.Error())
		return api.Res(false, __("Failed to store the CA key."))
	}
	reply := map[string]any{"id": id, "name": name}
	if existing != nil {
		return api.Res(true, __("Client CA %s is already managed here.", id), reply)
	}
	m.cfg.Mutate(func() {
		m.casLocked(true)[id] = map[string]any{
			"cert":    cleaned,
			"created": float64(m.now().UnixMilli()),
			"managed": true,
			"name":    name,
		}
		m.cfg.Touch("mtls")
	})
	m.saveLocked()
	m.log.Log("Imported client CA %s (%s)", id, logSafeName(name))
	return api.Res(true, __("Imported client CA %s. Trust it on a domain with: odac domain mtls set -d <domain> --mode optional --ca %s", id, id), reply)
}

// parseCAKey reads the signing key of a CA being imported and checks that it
// belongs to that CA. It accepts the PEM forms openssl writes (PKCS#8, SEC 1 and
// PKCS#1) and refuses an encrypted one rather than asking for its passphrase.
func parseCAKey(raw string, cert *x509.Certificate) (crypto.Signer, string) {
	rest := []byte(raw)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, __("Invalid CA key.")
		}
		if block.Type == "EC PARAMETERS" {
			continue
		}
		if block.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") {
			return nil, __("The CA key is encrypted. Decrypt it first, for example: openssl pkey -in ca.key -out ca-plain.key")
		}
		var parsed any
		var err error
		switch block.Type {
		case "PRIVATE KEY":
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			parsed, err = x509.ParseECPrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			return nil, __("Invalid CA key.")
		}
		if err != nil {
			return nil, __("Invalid CA key.")
		}
		switch key := parsed.(type) {
		case *ecdsa.PrivateKey:
			if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
				return nil, __("Unsupported CA key type.")
			}
		case *rsa.PrivateKey:
			if key.N.BitLen() < mtlsMinRSABits {
				return nil, __("RSA keys must be at least 2048 bits.")
			}
		case ed25519.PrivateKey:
		default:
			return nil, __("Unsupported CA key type.")
		}
		signer := parsed.(crypto.Signer)
		public, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
		if !ok || !public.Equal(cert.PublicKey) {
			return nil, __("That key does not belong to the CA certificate.")
		}
		return signer, ""
	}
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
		return api.Res(true, __("Certificate %s already exists.", str(existing["name"])), issueReply(existingFP, existing, caID, m.chainFor(existing, caID))), false, false
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
	// A certificate for one host of a per-host record is signed by that host's
	// issuer, so the handshake there names only the issuer; anything wider stays
	// with the CA, which every host of the record names when such a one is live.
	signer, signerKey, chain := caCert, caKey, caPEM
	var issuerPEM string
	issuerHost := m.issuerHostFor(hosts)
	if issuerHost != "" {
		issuer, issuerKey, certPEM, err := m.ensureIssuerLocked(store, caID, issuerHost, caCert, caKey)
		if err != nil {
			m.log.Error("Failed to set up the client certificate issuer of %s: %s", issuerHost, err.Error())
			return api.Res(false, __("Failed to sign the certificate.")), false, false
		}
		signer, signerKey, issuerPEM = issuer, issuerKey, certPEM
		chain = certPEM + caPEM
	}
	serial, err := randomSerial()
	if err != nil {
		return api.Res(false, __("Failed to sign the certificate.")), false, false
	}
	now := m.now()
	notAfter := now.Add(time.Duration(days) * 24 * time.Hour)
	if notAfter.After(signer.NotAfter) {
		notAfter = signer.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, csr.PublicKey, signerKey)
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
	if issuerHost != "" {
		record2["issuerHost"] = issuerHost
		record2["issuerCert"] = issuerPEM
	}
	if err := store.insert(fingerprint, record2); err != nil {
		return m.storeFailure(err), false, false
	}
	m.saveLocked()
	m.log.Log("Issued client certificate %s (%s)", fingerprint[:16], logSafeName(name))
	return api.Res(true, __("Certificate %s issued.", name), issueReply(fingerprint, record2, caID, chain)), true, newCA
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

// issueReply answers an issue. chain is every certificate above the leaf, the
// issuer first when one signed it, which is what a p12 has to carry.
func issueReply(fingerprint string, cert map[string]any, caID, chain string) map[string]any {
	return map[string]any{
		"ca":          caID,
		"cert":        cert["cert"],
		"chain":       chain,
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
	// A per-host issuer's name is its host's, and only that host accepts it.
	if issuerHost, _ := cert["issuerHost"].(string); issuerHost != "" && (len(hosts) != 1 || hosts[0] != issuerHost) {
		m.mu.Unlock()
		return api.Res(false, __("Certificate %s was signed for %s alone, so it cannot open other hosts. Issue a new one for them.", name, issuerHost))
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
	m.sweepIssuersLocked(store)
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

// mtlsMaxCertPEM bounds an imported certificate. ODAC's are well under 2 KiB.
const mtlsMaxCertPEM = 64 << 10

// Import records a certificate another server signed with a CA both share, so
// this server's proxy accepts it too: a managed CA's certificate with no record
// is refused. Nothing is signed here. The authority is the signature of a managed
// CA that every target host trusts, and the record only adds the scope, under the
// same rules as Issue (and, for an app, the same reach). opts carries cert (PEM),
// hosts and name, which, when given, must be the certificate's subject CN.
// Importing a recorded certificate again returns its row, but a revoked one
// stays revoked: an import must never bring a certificate back.
func (m *MTLS) Import(caller api.Caller, optsArg any) api.Result {
	app, ok := reach(caller)
	if !ok {
		return permissionDenied()
	}
	opts := optionsOf(optsArg)
	leaf, certPEM, presented, errMsg := parseClientCert(opts["cert"])
	if errMsg != "" {
		return api.Res(false, errMsg)
	}
	name := leaf.Subject.CommonName
	if raw := opts["name"]; raw != nil {
		given, isString := raw.(string)
		if !isString || (given != "" && given != name) {
			return api.Res(false, __("The name does not match the certificate's subject, %s.", logSafeName(name)))
		}
	}
	if !validName(name) {
		return api.Res(false, __("The certificate's name must be 1 to 64 printable characters."))
	}
	now := m.now()
	if !now.Before(leaf.NotAfter) {
		return api.Res(false, __("The certificate has expired."))
	}

	m.mu.Lock()
	result, imported := m.importLocked(app, leaf, presented, certPEM, name, opts["hosts"], now)
	m.mu.Unlock()

	if imported && m.proxy != nil {
		m.proxy.SyncClientCerts()
	}
	return result
}

func (m *MTLS) importLocked(app string, leaf *x509.Certificate, presented []*x509.Certificate, certPEM, name string, rawHosts any, now time.Time) (api.Result, bool) {
	var candidates map[string]importCandidate
	var failure string
	m.cfg.View(func() {
		candidates, failure = importCandidatesLocked(m.domainsLocked(), m.casLocked(false), rawHosts, app)
	})
	if failure != "" {
		return api.Res(false, failure), false
	}
	caID, managed := signingCA(leaf, candidates, now)
	var issuerHost string
	var issuer *x509.Certificate
	if caID == "" {
		var refusal string
		caID, issuerHost, issuer, refusal = m.perHostSigner(leaf, presented, candidates, rawHosts, now)
		if refusal != "" {
			return api.Res(false, refusal), false
		}
		managed = caID != ""
	}
	switch {
	case caID == "":
		return api.Res(false, __("The certificate was not signed by a client CA its hosts trust.")), false
	case !managed:
		return api.Res(false, __("That certificate comes from an external CA, which needs no record: its hosts already trust it.")), false
	}
	var hosts []string
	m.cfg.View(func() {
		hosts, failure = cleanScope(m.domainsLocked(), rawHosts, caID, app)
	})
	if failure != "" {
		return api.Res(false, failure), false
	}

	store, err := m.certs()
	if err != nil {
		return m.storeFailure(err), false
	}
	fingerprint := mtls.Fingerprint(leaf.Raw)
	existing, err := store.get(fingerprint)
	if err != nil {
		return m.storeFailure(err), false
	}
	if existing != nil {
		if existing["revoked"] != nil {
			return api.Res(false, __("This certificate has been revoked, and a revoked certificate cannot be imported.")), false
		}
		if !m.ownsCert(app, existing) {
			return api.Res(false, __("This certificate is already registered.")), false
		}
		return m.importReply(store, fingerprint, __("Certificate %s is already registered.", name)), false
	}

	if issuerHost != "" {
		// The handshake on the host can only name an issuer this server holds.
		caCert, caKey, _, err := m.loadCA(caID)
		if err == nil {
			_, _, _, err = m.ensureIssuerLocked(store, caID, issuerHost, caCert, caKey)
		}
		if err != nil {
			m.log.Error("Failed to set up the client certificate issuer of %s: %s", issuerHost, err.Error())
			return api.Res(false, __("Failed to set up the issuer of %s.", issuerHost)), false
		}
	}

	record := map[string]any{
		"ca":       caID,
		"cert":     certPEM,
		"created":  float64(now.UnixMilli()),
		"hosts":    anyList(hosts),
		"name":     name,
		"notAfter": float64(leaf.NotAfter.UnixMilli()),
		"serial":   hex.EncodeToString(leaf.SerialNumber.Bytes()),
		"spki":     mtls.Fingerprint(leaf.RawSubjectPublicKeyInfo),
	}
	if issuerHost != "" {
		record["issuerHost"] = issuerHost
		record["issuerCert"] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw}))
	}
	if err := store.insert(fingerprint, record); err != nil {
		return m.storeFailure(err), false
	}
	m.log.Log("Imported client certificate %s (%s)", fingerprint[:16], logSafeName(name))
	return m.importReply(store, fingerprint, __("Certificate %s imported.", name)), true
}

// importReply answers with the certificate's list row, the shape the caller
// already mirrors from domain.mtls.cert.list.
func (m *MTLS) importReply(store *certStore, fingerprint, message string) api.Result {
	row, err := store.row(fingerprint)
	if err != nil {
		return m.storeFailure(err)
	}
	if row == nil {
		return api.Res(false, __("Certificate not found."))
	}
	m.cfg.View(func() { row = m.withCANameLocked(row) })
	return api.Res(true, message, row)
}

type importCandidate struct {
	pem     string
	managed bool
}

// importCandidatesLocked validates the target hosts the way cleanScope does,
// before any CA is known, and returns the CAs they trust: the certificate's
// issuer has to be one of them.
func importCandidatesLocked(domains, cas map[string]any, rawHosts any, app string) (map[string]importCandidate, string) {
	entries := stringList(rawHosts)
	if len(entries) == 0 {
		return nil, __("At least one host is required.")
	}
	out := map[string]importCandidate{}
	for _, entry := range entries {
		host, errMsg := cleanHost(entry)
		if errMsg != "" {
			return nil, errMsg
		}
		record, ok := locateHost(domains, host)
		if !ok || !ownedLocked(domains, record, app) {
			return nil, __("Host %s is not registered.", host)
		}
		for _, id := range effectiveCAs(domains, record, host) {
			if ca, _ := cas[id].(map[string]any); ca != nil {
				certPEM, _ := ca["cert"].(string)
				out[id] = importCandidate{pem: certPEM, managed: ca["managed"] == true}
			}
		}
	}
	return out, ""
}

// signingCA finds which candidate CA signed leaf, checked the way the proxy will
// check it (chain and client auth usage). It verifies at the certificate's start
// when this clock is behind the signer's, since two servers' clocks differ and the
// proxy enforces the start time on every request anyway.
func signingCA(leaf *x509.Certificate, candidates map[string]importCandidate, now time.Time) (string, bool) {
	at := now
	if at.Before(leaf.NotBefore) {
		at = leaf.NotBefore
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		block, _ := pem.Decode([]byte(candidates[id].pem))
		if block == nil {
			continue
		}
		ca, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !bytes.Equal(ca.RawSubject, leaf.RawIssuer) {
			continue
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			return id, candidates[id].managed
		}
	}
	return "", false
}

// mtlsMaxImportChain bounds the certificates read after the client's: an issue
// reply's chain is at most an issuer and a CA.
const mtlsMaxImportChain = 4

// parseClientCert reads the certificate being imported. The PEM may carry the
// chain after it, as an issue reply's cert and chain concatenated would; only the
// first certificate is the client's, and the ones after it are returned as what
// it presented, which a certificate from another server's per-host issuer needs.
func parseClientCert(raw any) (*x509.Certificate, string, []*x509.Certificate, string) {
	text, _ := raw.(string)
	if len(text) > mtlsMaxCertPEM {
		return nil, "", nil, __("Invalid certificate.")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		return nil, "", nil, __("That file contains a private key. Send only the certificate.")
	}
	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", nil, __("Invalid certificate.")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", nil, __("Invalid certificate.")
	}
	if cert.IsCA {
		return nil, "", nil, __("That is a CA certificate, not a client certificate.")
	}
	var presented []*x509.Certificate
	for len(presented) < mtlsMaxImportChain {
		var next *pem.Block
		next, rest = pem.Decode(rest)
		if next == nil {
			break
		}
		if next.Type != "CERTIFICATE" {
			continue
		}
		if parsed, err := x509.ParseCertificate(next.Bytes); err == nil {
			presented = append(presented, parsed)
		}
	}
	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})), presented, ""
}

// perHostSigner finds the per-host issuer that signed an imported leaf, checked
// the way the proxy will check it (mtls.VerifyPerHost). Such a certificate opens
// its issuer's host and nothing else, so the import must name exactly that host,
// on a per-host record, under a managed CA. The issuer has to come with the leaf:
// another server's has a key this one never sees, so without it nothing here can
// check the signature, and a record nothing could verify is refused rather than
// kept. It returns no CA and no refusal when the leaf is not of this kind.
func (m *MTLS) perHostSigner(leaf *x509.Certificate, presented []*x509.Certificate, candidates map[string]importCandidate, rawHosts any, now time.Time) (string, string, *x509.Certificate, string) {
	entries := stringList(rawHosts)
	if len(entries) != 1 {
		return "", "", nil, ""
	}
	host, errMsg := cleanHost(entries[0])
	if errMsg != "" || !mtls.PerHostIssuable(host) {
		return "", "", nil, ""
	}
	perHost := false
	m.cfg.View(func() { perHost = perHostLocked(m.domainsLocked(), host) })
	if !perHost {
		return "", "", nil, ""
	}
	at := now
	if at.Before(leaf.NotBefore) {
		at = leaf.NotBefore
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !candidates[id].managed {
			continue
		}
		block, _ := pem.Decode([]byte(candidates[id].pem))
		if block == nil {
			continue
		}
		ca, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !bytes.Equal(leaf.RawIssuer, mtls.IssuerSubject(host, ca)) {
			continue
		}
		if len(presented) == 0 {
			return "", "", nil, __("This certificate was signed by the issuer of %s. Send it together with its chain.", host)
		}
		chain := append([]*x509.Certificate{leaf}, presented...)
		if anchor, _, status := mtls.VerifyPerHost(chain, nil, []*x509.Certificate{ca}, host, at); status != mtls.VerifySuccess || anchor != ca {
			continue
		}
		for _, issuer := range presented {
			if bytes.Equal(issuer.RawSubject, leaf.RawIssuer) && leaf.CheckSignatureFrom(issuer) == nil {
				return id, host, issuer, ""
			}
		}
	}
	return "", "", nil, ""
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
	m.sweepIssuersLocked(store)
	m.saveLocked()
}
