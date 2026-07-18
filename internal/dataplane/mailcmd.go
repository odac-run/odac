package dataplane

import (
	"bytes"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"odac/internal/api"
)

// Mail command handlers — the port of Mail.js create()/delete()/list()/
// password()/send() and #dkim, per contracts/mail-control.md
// "Orchestrator-side logic to port". Account data lives in the Go mail
// binary's SQLite store; these handlers validate, resolve domains against
// config, pass through to the binary's HTTP API and shape the api.Result.
//
// Where Node's map iteration order (for..in over config.domains) leaks into
// behavior — the subdomain walk in Create, the DKIM candidate scan — Go
// iterates keys sorted for determinism; Node used insertion order (recorded
// deviation).

// Create ports Mail.create(email, password, retype).
func (m *Mail) Create(email, password, retype any) api.Result {
	if !truthy(email) || !truthy(password) || !truthy(retype) {
		return api.Res(false, __("All fields are required."))
	}
	if !jsEqual(password, retype) {
		return api.Res(false, __("Passwords do not match."))
	}

	domain, found := m.resolveAccountDomain(str(email))
	if !found {
		return api.Res(false, __("Domain %s not found.", domain))
	}

	res, err := m.moduleRequest("POST", "/account", map[string]any{
		"domain": domain, "email": email, "password": password, "retype": retype,
	})
	if err != nil {
		m.log.Error("Account creation failed: %s", err.Error())
		return api.Res(false, __("Account creation failed."))
	}
	if truthy(res["success"]) {
		go m.SyncConfig() // Node fires syncConfig un-awaited after create
		return api.Res(true, __("Mail account %s created successfully.", str(email)))
	}
	if truthy(res["message"]) {
		return api.Res(false, res["message"])
	}
	return api.Res(false, __("Account creation failed."))
}

// Delete ports Mail.delete(email).
func (m *Mail) Delete(email any) api.Result {
	if !truthy(email) {
		return api.Res(false, __("Email address is required."))
	}
	res, err := m.moduleRequest("DELETE", "/account", map[string]any{"email": email})
	if err != nil {
		return api.Res(false, __("Account deletion failed."))
	}
	if truthy(res["success"]) {
		return api.Res(true, __("Mail account %s deleted successfully.", str(email)))
	}
	if truthy(res["message"]) {
		return api.Res(false, res["message"])
	}
	return api.Res(false, __("Account deletion failed."))
}

// List ports Mail.list(domain): a text blob (message + newline-joined
// addresses), not structured data.
func (m *Mail) List(domain any) api.Result {
	if !truthy(domain) {
		return api.Res(false, __("Domain is required."))
	}
	known := false
	m.cfg.View(func() {
		known = truthy(m.cfg.Map("domains")[str(domain)])
	})
	if !known {
		return api.Res(false, __("Domain %s not found.", str(domain)))
	}

	res, err := m.moduleRequest("GET", "/accounts?domain="+url.QueryEscape(str(domain)), nil)
	if err != nil {
		return api.Res(false, __("Account list failed."))
	}
	if truthy(res["success"]) {
		accounts, _ := res["accounts"].([]any)
		lines := make([]string, len(accounts))
		for i, a := range accounts {
			lines[i] = jsString(a)
		}
		return api.Res(true, __("Mail accounts for domain %s.", str(domain))+"\n"+strings.Join(lines, "\n"))
	}
	if truthy(res["message"]) {
		return api.Res(false, res["message"])
	}
	return api.Res(false, __("Account list failed."))
}

// Password ports Mail.password(email, password, retype).
func (m *Mail) Password(email, password, retype any) api.Result {
	if !truthy(email) || !truthy(password) || !truthy(retype) {
		return api.Res(false, __("All fields are required."))
	}
	if !jsEqual(password, retype) {
		return api.Res(false, __("Passwords do not match."))
	}
	res, err := m.moduleRequest("PUT", "/account/password", map[string]any{
		"email": email, "password": password, "retype": retype,
	})
	if err != nil {
		return api.Res(false, __("Password update failed."))
	}
	if truthy(res["success"]) {
		return api.Res(true, __("Mail account %s password updated successfully.", str(email)))
	}
	if truthy(res["message"]) {
		return api.Res(false, res["message"])
	}
	return api.Res(false, __("Password update failed."))
}

var boundaryRe = regexp.MustCompile(`boundary="?([^";\s]+)"?`)

// Send ports Mail.send(data): assemble an RFC 2822 message from the
// parsed-mail shape ({from:{value:[{address}]}, to:{...}, header:{...},
// subject?, html?, text?}) and hand it to the binary's /send. rawData is
// the request argument's untouched JSON — header lines are emitted in
// document order, matching Node's for..in over the parsed object (nil is
// tolerated: internal callers without raw JSON get sorted header order).
func (m *Mail) Send(data any, rawData json.RawMessage) api.Result {
	dm, _ := data.(map[string]any)
	if !truthy(data) || dm == nil || !truthy(dm["from"]) || !truthy(dm["to"]) || !truthy(dm["header"]) {
		return api.Res(false, __("All fields are required."))
	}
	fromAddr := mailAddr(dm["from"])
	if fromAddr == "" {
		return api.Res(false, __("Invalid email address."))
	}
	toAddr := mailAddr(dm["to"])
	if toAddr == "" {
		return api.Res(false, __("Invalid email address."))
	}

	// Sender domain: split on dots, strip leading labels while more than two
	// remain and no configured domain matches.
	domainPart := ""
	if parts := strings.Split(fromAddr, "@"); len(parts) > 1 {
		domainPart = parts[1]
	}
	labels := strings.Split(domainPart, ".")
	var domain string
	m.cfg.View(func() {
		domains := m.cfg.Map("domains")
		for len(labels) > 2 && !truthy(domains[strings.Join(labels, ".")]) {
			labels = labels[1:]
		}
		domain = strings.Join(labels, ".")
		if !truthy(domains[domain]) {
			domain = ""
		}
	})
	if domain == "" {
		return api.Res(false, __("Domain %s not found.", strings.Join(labels, ".")))
	}

	headerMap, _ := dm["header"].(map[string]any)
	contentType := ""
	if headerMap != nil && truthy(headerMap["Content-Type"]) {
		contentType = jsString(headerMap["Content-Type"])
	}
	isMultipart := strings.Contains(contentType, "multipart/alternative")

	boundary := ""
	if isMultipart {
		if match := boundaryRe.FindStringSubmatch(contentType); match != nil {
			boundary = match[1]
		}
	}

	headers := ""
	for _, key := range headerKeysInOrder(rawData, headerMap) {
		if isMultipart && key == "Content-Type" {
			continue // rebuilt below with the extracted boundary
		}
		headers += key + ": " + jsString(headerMap[key]) + "\r\n"
	}
	if !strings.Contains(strings.ToLower(headers), "from:") {
		headers += "From: " + fromAddr + "\r\n"
	}
	if !strings.Contains(strings.ToLower(headers), "to:") {
		headers += "To: " + toAddr + "\r\n"
	}
	if !strings.Contains(strings.ToLower(headers), "subject:") {
		subject := ""
		if v, ok := dm["subject"]; ok && v != nil { // data.subject ?? ''
			subject = jsString(v)
		}
		headers += "Subject: " + subject + "\r\n"
	}
	if !strings.Contains(strings.ToLower(headers), "mime-version:") {
		headers += "MIME-Version: 1.0\r\n"
	}

	body := ""
	switch {
	case isMultipart && boundary != "" && truthy(dm["html"]):
		// RFC 2046 multipart/alternative with boundary-delimited parts.
		headers += `Content-Type: multipart/alternative; boundary="` + boundary + `"` + "\r\n"
		body = headers + "\r\n"
		if truthy(dm["text"]) {
			body += "--" + boundary + "\r\n"
			body += "Content-Type: text/plain; charset=UTF-8\r\n\r\n"
			body += jsString(dm["text"]) + "\r\n"
		}
		body += "--" + boundary + "\r\n"
		body += "Content-Type: text/html; charset=UTF-8\r\n\r\n"
		body += jsString(dm["html"]) + "\r\n"
		body += "--" + boundary + "--\r\n"
	case truthy(dm["html"]):
		if !strings.Contains(strings.ToLower(headers), "content-type:") {
			headers += "Content-Type: text/html; charset=UTF-8\r\n"
		}
		body = headers + "\r\n" + jsString(dm["html"])
	case truthy(dm["text"]):
		if !strings.Contains(strings.ToLower(headers), "content-type:") {
			headers += "Content-Type: text/plain; charset=UTF-8\r\n"
		}
		body = headers + "\r\n" + jsString(dm["text"])
	default:
		body = headers + "\r\n"
	}

	res, err := m.moduleRequest("POST", "/send", map[string]any{
		"body": body, "from": fromAddr, "to": toAddr,
	})
	if err != nil {
		m.log.Error("mail.send failed: %s", err.Error())
		return api.Res(false, __("Mail sending failed."))
	}
	if truthy(res["success"]) {
		return api.Res(true, __("Mail sent successfully."))
	}
	if truthy(res["message"]) {
		return api.Res(false, res["message"])
	}
	return api.Res(false, __("Mail sending failed."))
}

// resolveAccountDomain ports Create's domain resolution: exact config.domains
// match, else walk the domains checking whether the leading label(s) appear
// in that domain's subdomain list. Returns the (possibly unresolved) domain
// for the "Domain %s not found." message.
func (m *Mail) resolveAccountDomain(email string) (string, bool) {
	domain := ""
	if parts := strings.Split(email, "@"); len(parts) > 1 {
		domain = parts[1]
	}
	found := false
	m.cfg.View(func() {
		domains := m.cfg.Map("domains")
		if truthy(domains[domain]) {
			found = true
			return
		}
		for _, d := range sortedKeys(domains) {
			if d == "" || !strings.HasSuffix(domain, d) {
				continue
			}
			label := ""
			if cut := len(domain) - len(d) - 1; cut > 0 {
				label = domain[:cut]
			}
			rec, _ := domains[d].(map[string]any)
			if rec == nil {
				continue
			}
			if list, _ := rec["subdomain"].([]any); listIncludes(list, label) {
				domain = d
				found = true
				return
			}
		}
	})
	return domain, found
}

// moduleRequest ports #apiRequest's guards: the binary must be running and
// socket-addressable, otherwise the caller lands in its catch-path message.
func (m *Mail) moduleRequest(method, path string, payload any) (map[string]any, error) {
	if !m.proc.Running() {
		return nil, errors.New("Mail process not running")
	}
	return requestJSON(m.proc.SocketPath(), method, path, payload)
}

// dkimCheck ports the DKIM half of Mail.check(): for every configured
// domain whose DNS zone (exact name) has an MX record, with cert not
// disabled and no DKIM material yet, generate keys. The first failure
// aborts the pass (Node's try wraps the whole loop); the next tick retries.
func (m *Mail) dkimCheck() {
	var candidates []string
	m.cfg.View(func() {
		dnsCfg := m.cfg.Map("dns")
		domains := m.cfg.Map("domains")
		for _, domain := range sortedKeys(domains) {
			if !zoneHasMX(dnsCfg[domain]) {
				continue
			}
			rec, _ := domains[domain].(map[string]any)
			if rec == nil {
				continue
			}
			if rec["cert"] == any(false) { // cert !== false gate
				continue
			}
			if cm, _ := rec["cert"].(map[string]any); cm != nil && truthy(cm["dkim"]) {
				continue
			}
			candidates = append(candidates, domain)
		}
	})

	for _, domain := range candidates {
		if err := m.dkim(domain); err != nil {
			m.log.Error("DKIM check failed: %s", err.Error())
			return
		}
	}
}

// dkim ports #dkim: RSA-2048 keys (PKCS#1 private, SPKI public), persisted
// under <base>/cert/dkim, published as a default._domainkey TXT record and
// recorded in the domain's cert.dkim config.
func (m *Mail) dkim(domain string) error {
	key, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})

	const selector = "default"
	dkimDir := filepath.Join(m.cfg.BaseDir(), "cert", "dkim")
	if err := os.MkdirAll(dkimDir, 0o755); err != nil {
		return err
	}
	keyPath := filepath.Join(dkimDir, domain+".key")
	pubPath := filepath.Join(dkimDir, domain+".pub")
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(keyPath, 0o600); err != nil { // Node chmods after write
		return err
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return err
	}

	// The TXT payload strips the PEM armor — i.e. base64 of the SPKI DER.
	publicKeyBase64 := base64.StdEncoding.EncodeToString(spki)

	m.cfg.Mutate(func() {
		domains := m.cfg.Map("domains")
		rec, _ := domains[domain].(map[string]any)
		if rec == nil {
			return
		}
		cert, _ := rec["cert"].(map[string]any)
		if cert == nil { // Node: if (!cert) cert = {}
			cert = map[string]any{}
			rec["cert"] = cert
		}
		cert["dkim"] = map[string]any{"private": keyPath, "public": pubPath, "selector": selector}
		m.cfg.Touch("domains")
	})

	if m.dns != nil {
		m.dns.Record(map[string]any{
			"type":  "TXT",
			"name":  selector + "._domainkey." + domain,
			"value": "v=DKIM1; k=rsa; p=" + publicKeyBase64,
		})
	}
	if err := m.cfg.ForceSave(); err != nil {
		return err
	}
	go m.SyncConfig() // Node fires syncConfig un-awaited after DKIM setup
	m.log.Log("DKIM 2048-bit keys generated for %s (selector: %s)", domain, selector)
	return nil
}

// mailAddr navigates {value: [{address}]} like from.value?.[0]?.address.
func mailAddr(v any) string {
	vm, _ := v.(map[string]any)
	list, _ := vm["value"].([]any)
	if len(list) == 0 {
		return ""
	}
	entry, _ := list[0].(map[string]any)
	if !truthy(entry["address"]) {
		return ""
	}
	return str(entry["address"])
}

// headerKeysInOrder returns the header object's keys in JSON document order
// (rawData = the request argument's raw JSON). Duplicate keys keep their
// first position, like JS object insertion. Without raw JSON the decoded
// map's keys are returned sorted.
func headerKeysInOrder(rawData json.RawMessage, headerMap map[string]any) []string {
	if keys := jsonFieldObjectKeys(rawData, "header"); keys != nil {
		seen := map[string]bool{}
		out := keys[:0]
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		return out
	}
	return sortedKeys(headerMap)
}

// jsonFieldObjectKeys extracts the key order of raw's top-level object
// field; nil when raw is not an object or the field is missing/non-object.
func jsonFieldObjectKeys(raw []byte, field string) []string {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := keyTok.(string)
		if key != field {
			if skipJSONValue(dec) != nil {
				return nil
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return nil
		}
		keys := []string{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil
			}
			ks, _ := kt.(string)
			keys = append(keys, ks)
			if skipJSONValue(dec) != nil {
				return nil
			}
		}
		return keys
	}
	return nil
}

func skipJSONValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); ok && (d == '{' || d == '[') {
		depth := 1
		for depth > 0 {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := t.(json.Delim); ok {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// listIncludes ports Array.prototype.includes on a decoded-JSON list.
func listIncludes(list []any, v string) bool {
	for _, e := range list {
		if e == any(v) {
			return true
		}
	}
	return false
}
