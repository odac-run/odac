// Package domains is the Go port of server/src/Domain.js + SSL.js +
// SSL/Acme.js (migration task 3.5): the domain table (config.domains) with
// its DNS record fan-out and app binding, plus ACME certificate issuance.
//
// Two managers live here, mirroring the two Node modules:
//
//   - Domain — domain CRUD, subdomain handling, app cascade (DeleteByApp
//     fills appmgr's DomainDeleter seam).
//   - SSL — the 1s-tick certificate scan, renewal with HTTP-01→DNS-01
//     fallback, error backoff, cancellation/queueing, and the self-signed
//     bootstrap cert (the `selfsigned` npm dep replaced by crypto/x509).
//
// The hand-written RFC 8555 client (SSL/Acme.js) is replaced by
// golang.org/x/crypto/acme (acme.go); the challenge tokens still flow
// through the proxy control API (HTTP-01) and the DNS zone table (DNS-01)
// exactly per contracts/proxy-control.md and dns-control.md.
//
// Deviations from Node (deliberate):
//   - Where JS object iteration order leaks (domain list output, deleteByApp
//     sweep, check() scan, renew()'s subdomain search) Go iterates sorted
//     domain names — the established 3.3/3.4 pattern.
//   - dataplane.DNS.Record cannot fail (invalid records are skipped), so
//     Add()'s "Failed to create DNS records" branch is unreachable; Node
//     only hit it on internal throw.
//   - Config mutations run under config.Store.Mutate and re-fetch the
//     record inside the lock (never through a pointer captured outside it);
//     collaborator calls (DNS/SSL/Proxy/Mail) happen outside the lock.
package domains

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/dataplane"
	"odac/internal/lang"
	"odac/internal/logx"
)

var __ = lang.T

// DNSService is what Domain/SSL borrow from the DNS service: record CRUD and
// the detected address set (Node reads all three off Odac.server('DNS')).
// *dataplane.DNS implements it.
type DNSService interface {
	Record(args ...map[string]any)
	Delete(args ...map[string]any)
	IPInfo() (ipv4, ipv6 []dataplane.IPEntry, primary string)
}

// ProxyService is the proxy surface: config sync after domain changes and
// the HTTP-01 challenge push. *dataplane.Proxy implements it.
type ProxyService interface {
	SyncConfig()
	SetACMEChallenge(token, keyAuthorization string) error
	DeleteACMEChallenge(token string)
}

// MailService clears the mail binary's per-domain TLS cache after cert
// changes. *dataplane.Mail implements it.
type MailService interface {
	ClearSSLCache(domain string)
}

// Renewer is the SSL surface Domain needs (renew after subdomain changes);
// *SSL implements it.
type Renewer interface {
	Renew(domain any) api.Result
}

// Domain is the Domain.js singleton. All collaborators are nil-tolerant,
// like the Node registry which never resolves a missing module.
type Domain struct {
	cfg   *config.Store
	log   *logx.Logger
	dns   DNSService
	ssl   Renewer
	proxy ProxyService
	mail  MailService
}

// NewDomain wires the manager. baseDir-derived paths come from cfg.
func NewDomain(cfg *config.Store, dns DNSService, ssl Renewer, proxy ProxyService, mail MailService) *Domain {
	return &Domain{cfg: cfg, log: logx.New("Domain"), dns: dns, ssl: ssl, proxy: proxy, mail: mail}
}

var ipv4Re = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)

// validate ports #validate: sanitize (protocol strip, lowercase) and check
// the format. Returns the clean domain or a user-facing error message.
func validate(raw any) (string, string) {
	domain, ok := raw.(string)
	if !ok || domain == "" {
		return "", __("Domain is required.")
	}

	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, prefix := range []string{"http://", "https://", "ftp://"} {
		if strings.HasPrefix(domain, prefix) {
			domain = strings.Replace(domain, prefix, "", 1)
		}
	}

	if len(domain) < 3 ||
		(!strings.Contains(domain, ".") && domain != "localhost") ||
		strings.Contains(domain, "/") ||
		strings.Contains(domain, "\\") ||
		strings.Contains(domain, "..") {
		return "", __("Invalid domain format.")
	}
	return domain, ""
}

// keysByLengthDesc sorts domain names longest-first (the subdomain walk must
// match the most specific parent), ties alphabetical for determinism.
func keysByLengthDesc(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// domainsMap returns config.domains under an already-held lock, creating the
// map like Node's #getDomains when absent (write requires Mutate).
func (d *Domain) domainsLocked(create bool) map[string]any {
	domains, _ := d.cfg.Get("domains").(map[string]any)
	if domains == nil && create {
		domains = map[string]any{}
		d.cfg.Set("domains", domains)
	}
	return domains
}

// Add ports add(): validate, bind the domain to an existing app, create the
// DNS record set, register the domain and kick off SSL provisioning.
func (d *Domain) Add(domainArg, appID any) api.Result {
	domain, errMsg := validate(domainArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}

	// www. is a subdomain, not a root domain.
	domain = strings.TrimPrefix(domain, "www.")

	if !truthy(appID) {
		return api.Res(false, __("App ID is required."))
	}

	// Existence + app lookup + subdomain-parent scan, snapshotted under the
	// read lock. targetName is the app the domain binds to.
	var exists, found bool
	var targetName string
	var parentDomain, sub string
	var subAlready bool
	d.cfg.View(func() {
		domains := d.domainsLocked(false)
		if _, ok := domains[domain]; ok {
			exists = true
			return
		}

		apps, _ := d.cfg.Get("apps").([]any)
		for _, a := range apps {
			app, _ := a.(map[string]any)
			if app != nil && (app["id"] == appID || app["name"] == appID) {
				found = true
				targetName, _ = app["name"].(string)
				break
			}
		}
		if !found {
			return
		}

		// Subdomain of an existing domain for the same app? Longest parent
		// wins (Node sorts keys by length desc).
		for _, parent := range keysByLengthDesc(domains) {
			record, _ := domains[parent].(map[string]any)
			if record == nil || record["appId"] != targetName {
				continue
			}
			if strings.HasSuffix(domain, "."+parent) && domain != parent {
				parentDomain = parent
				sub = domain[:len(domain)-len(parent)-1]
				subAlready = listContains(record["subdomain"], sub)
				break
			}
		}
	})

	if exists {
		return api.Res(false, __("Domain %s is already registered.", domain))
	}
	if !found {
		return api.Res(false, __("App %s not found.", str(appID)))
	}

	if parentDomain != "" {
		if subAlready {
			return api.Res(true, __("Subdomain %s already exists on %s.", sub, parentDomain))
		}
		d.cfg.Mutate(func() {
			record, _ := d.domainsLocked(false)[parentDomain].(map[string]any)
			if record == nil {
				return
			}
			subs, _ := record["subdomain"].([]any)
			if !listContains(record["subdomain"], sub) {
				record["subdomain"] = append(subs, sub)
			}
			d.cfg.Touch("domains")
		})

		// CNAME to the parent; Record cannot fail (see package doc).
		d.dns.Record(map[string]any{"name": domain, "type": "CNAME", "value": parentDomain})
		d.log.Log("Added subdomain %s to %s", sub, parentDomain)

		// Renew the parent's cert to include the new subdomain.
		if d.ssl != nil {
			d.ssl.Renew(parentDomain)
		}
		if d.proxy != nil {
			d.proxy.SyncConfig()
		}
		return api.Res(true, __("Added %s as a subdomain of %s.", domain, parentDomain))
	}

	isLocalOrIP := domain == "localhost" || ipv4Re.MatchString(domain)
	isWildcard := strings.HasPrefix(domain, "*.")

	// DNS records (skipped for localhost/IPs). A and AAAA carry no value —
	// the DNS binary resolves them dynamically via PTR matching.
	sslEnabled := false
	if !isLocalOrIP {
		var records []map[string]any
		if isWildcard {
			records = []map[string]any{
				{"name": domain, "type": "A"},
				{"name": domain, "type": "AAAA"},
			}
		} else {
			records = []map[string]any{
				{"name": domain, "type": "A"},
				{"name": domain, "type": "AAAA"},
				{"name": "www." + domain, "type": "CNAME", "value": domain},
				{"name": "mail." + domain, "type": "A"},
				{"name": domain, "type": "MX", "value": "mail." + domain},
				{"name": "_dmarc." + domain, "type": "TXT",
					"value": "v=DMARC1; p=reject; rua=mailto:postmaster@" + domain},
			}

			// SPF needs explicit IPs for external validation.
			spf := "v=spf1 a mx"
			if d.dns != nil {
				_, v6, primary := d.dns.IPInfo()
				if primary != "" && primary != "127.0.0.1" {
					spf += " ip4:" + primary
				}
				for _, entry := range v6 {
					if entry.Public {
						spf += " ip6:" + entry.Address
						break
					}
				}
			}
			spf += " ~all"
			records = append(records, map[string]any{"name": domain, "type": "TXT", "value": spf})
		}

		d.dns.Record(records...)
		d.log.Log("Created DNS records for domain %s", domain)
		sslEnabled = true
	}

	d.cfg.Mutate(func() {
		domains := d.domainsLocked(true)
		record := map[string]any{
			"appId":     targetName,
			"created":   float64(time.Now().UnixMilli()),
			"subdomain": []any{},
		}
		if !isLocalOrIP && !isWildcard {
			record["subdomain"] = []any{"www", "mail"}
		}
		if sslEnabled {
			record["cert"] = map[string]any{}
		}
		domains[domain] = record
		d.cfg.Touch("domains")
	})

	if sslEnabled && d.ssl != nil {
		d.ssl.Renew(domain)
		d.log.Log("Initiated SSL certificate provisioning for %s", domain)
	}

	d.log.Log("Domain %s added to app %s", domain, targetName)

	if d.proxy != nil {
		d.proxy.SyncConfig()
	}
	return api.Res(true, __("Domain %s added to app %s.", domain, targetName))
}

// Delete ports delete(): remove a main domain (with its full DNS record set,
// DKIM key files and mail TLS cache) or a subdomain (CNAME + parent renewal).
// skipSync suppresses the proxy sync for batch deletes (DeleteByApp).
func (d *Domain) Delete(domainArg any, skipSync bool) api.Result {
	domain, errMsg := validate(domainArg)
	if errMsg != "" {
		return api.Res(false, errMsg)
	}

	// Snapshot what we need under the read lock.
	var isMain bool
	var recordAppID any
	var dkimSelector string
	var parentDomain, sub string
	d.cfg.View(func() {
		domains := d.domainsLocked(false)
		record, _ := domains[domain].(map[string]any)
		if truthy(domains[domain]) { // Node: if (domains[domain])
			isMain = true
			if record != nil {
				recordAppID = record["appId"]
				cert, _ := record["cert"].(map[string]any)
				dkim, _ := cert["dkim"].(map[string]any)
				dkimSelector, _ = dkim["selector"].(string)
			}
			return
		}

		for _, parent := range keysByLengthDesc(domains) {
			if strings.HasSuffix(domain, "."+parent) && domain != parent {
				rec, _ := domains[parent].(map[string]any)
				candidate := domain[:len(domain)-len(parent)-1]
				if rec != nil && listContains(rec["subdomain"], candidate) {
					parentDomain = parent
					sub = candidate
					return
				}
			}
		}
	})

	if isMain {
		if domain != "localhost" && !ipv4Re.MatchString(domain) {
			if dkimSelector == "" {
				dkimSelector = "default"
			}
			var records []map[string]any
			if strings.HasPrefix(domain, "*.") {
				records = []map[string]any{
					{"name": domain, "type": "A"},
					{"name": domain, "type": "AAAA"},
				}
			} else {
				records = []map[string]any{
					{"name": domain, "type": "A"},
					{"name": domain, "type": "AAAA"},
					{"name": "www." + domain, "type": "CNAME"},
					{"name": "mail." + domain, "type": "A"},
					{"name": domain, "type": "MX"},
					{"name": domain, "type": "TXT"},
					{"name": "_dmarc." + domain, "type": "TXT"},
					{"name": dkimSelector + "._domainkey." + domain, "type": "TXT"},
				}
			}
			// One call per record, like Node's per-record try/catch loop.
			for _, rec := range records {
				d.dns.Delete(rec)
			}
			d.log.Log("Deleted DNS records for domain %s", domain)

			// DKIM key files; missing files are fine.
			dkimDir := filepath.Join(d.cfg.BaseDir(), "cert", "dkim")
			os.Remove(filepath.Join(dkimDir, domain+".key"))
			os.Remove(filepath.Join(dkimDir, domain+".pub"))

			if d.mail != nil {
				d.mail.ClearSSLCache(domain)
			}
		}

		d.cfg.Mutate(func() {
			delete(d.domainsLocked(false), domain)
			d.cfg.Touch("domains")
		})

		d.log.Log("Domain %s deleted (was assigned to app %s)", domain, str(recordAppID))

		if !skipSync && d.proxy != nil {
			d.proxy.SyncConfig()
		}
		return api.Res(true, __("Domain %s deleted successfully.", domain))
	}

	if parentDomain != "" {
		d.cfg.Mutate(func() {
			record, _ := d.domainsLocked(false)[parentDomain].(map[string]any)
			if record == nil {
				return
			}
			subs, _ := record["subdomain"].([]any)
			kept := make([]any, 0, len(subs))
			for _, s := range subs {
				if s != sub {
					kept = append(kept, s)
				}
			}
			record["subdomain"] = kept
			d.cfg.Touch("domains")
		})

		d.dns.Delete(map[string]any{"name": domain, "type": "CNAME"})
		d.log.Log("Deleted CNAME record for subdomain %s", domain)

		if d.ssl != nil {
			d.ssl.Renew(parentDomain)
		}
		if !skipSync && d.proxy != nil {
			d.proxy.SyncConfig()
		}
		return api.Res(true, __("Subdomain %s removed from %s.", sub, parentDomain))
	}

	return api.Res(false, __("Domain %s not found.", domain))
}

// DeleteByApp ports deleteByApp(): remove every domain bound to the app,
// syncing the proxy once at the end. Fills appmgr's DomainDeleter seam.
func (d *Domain) DeleteByApp(appID string) error {
	if appID == "" {
		return nil
	}

	var targets []string
	d.cfg.View(func() {
		for name, rec := range d.domainsLocked(false) {
			record, _ := rec.(map[string]any)
			if record != nil && record["appId"] == appID {
				targets = append(targets, name)
			}
		}
	})
	if len(targets) == 0 {
		return nil
	}
	sort.Strings(targets)

	d.log.Log("Deleting %d domains for app %s", len(targets), appID)
	for _, domain := range targets {
		// Reuse the full delete logic; skip the per-domain proxy sync.
		d.Delete(domain, true)
	}

	if d.proxy != nil {
		d.proxy.SyncConfig()
	}
	return nil
}

// List ports list(): all registered domains, optionally filtered by app.
// The payload keeps Node's per-record key order (domain, subdomain, app,
// created) with absent fields omitted like JS undefined; records are ordered
// by domain name (Node: config insertion order — recorded deviation).
func (d *Domain) List(appIDArg any) api.Result {
	appID := ""
	if s, ok := appIDArg.(string); ok {
		norm := strings.ToLower(strings.TrimSpace(s))
		if norm != "undefined" && norm != "null" {
			appID = s
		}
	}

	type row struct {
		domain string
		record map[string]any
	}
	var rows []row
	total := 0
	d.cfg.View(func() {
		domains := d.domainsLocked(false)
		total = len(domains)
		for _, name := range sortedKeys(domains) {
			record, _ := domains[name].(map[string]any)
			if record == nil {
				record = map[string]any{}
			}
			if appID == "" || record["appId"] == appID {
				rows = append(rows, row{domain: name, record: copyShallow(record)})
			}
		}
	})

	if total == 0 {
		return api.Res(true, []any{})
	}
	if len(rows) == 0 {
		if appID != "" {
			return api.Res(false, __("No domains found for app %s.", appID))
		}
		return api.Res(false, __("No domains found."))
	}

	out := []byte{'['}
	for i, r := range rows {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendOrderedObject(out, [][2]any{
			{"domain", r.domain},
			{"subdomain", r.record["subdomain"]},
			{"app", r.record["appId"]},
			{"created", r.record["created"]},
		}, map[string]bool{
			"subdomain": hasKey(r.record, "subdomain"),
			"app":       hasKey(r.record, "appId"),
			"created":   hasKey(r.record, "created"),
		})
	}
	out = append(out, ']')
	return api.Res(true, rawJSON(out))
}
