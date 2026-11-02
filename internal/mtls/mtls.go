// Package mtls is the client certificate contract shared by the server, which
// owns the records, and the proxy, which enforces them. It stays free of the
// config store so the proxy binary does not inherit the server's dependencies.
package mtls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Modes a host can run in. Off exists only as a per-host override: a record
// with no mode stores nothing, because the dashboard diffs rows by serialization.
const (
	ModeOff      = "off"
	ModeOptional = "optional"
	ModeRequired = "required"
)

// Verify values an app reads from HeaderVerify. Only VerifySuccess proves
// anything, so apps must compare against it rather than rule out the others.
const (
	VerifyExpired    = "expired"
	VerifyNone       = "none"
	VerifyOutOfScope = "out_of_scope"
	VerifyRevoked    = "revoked"
	VerifySuccess    = "success"
	VerifyUntrusted  = "untrusted"
)

// Headers the proxy sets on requests to a host with a client certificate
// policy. A client sending any of them has them removed on every path.
const (
	HeaderFingerprint = "X-Odac-Client-Fingerprint"
	HeaderIssuer      = "X-Odac-Client-Issuer"
	HeaderNotAfter    = "X-Odac-Client-Not-After"
	HeaderSerial      = "X-Odac-Client-Serial"
	HeaderSignature   = "X-Odac-Client-Signature"
	HeaderSubject     = "X-Odac-Client-Subject"
	HeaderVerify      = "X-Odac-Client-Verify"
)

// KeyEnv names the variable holding the key an app verifies HeaderSignature with.
const KeyEnv = "ODAC_CLIENT_CERT_KEY"

// Headers lists every client certificate header, for stripping.
var Headers = []string{
	HeaderFingerprint, HeaderIssuer, HeaderNotAfter, HeaderSerial,
	HeaderSignature, HeaderSubject, HeaderVerify,
}

// Assertion is what the proxy vouches for on one request. Method, host and URI
// are signed too, so a signature captured on one route proves nothing on another.
type Assertion struct {
	Fingerprint string
	Host        string
	Issuer      string
	Method      string
	NotAfter    string
	Serial      string
	Subject     string
	Timestamp   int64
	URI         string
	Verify      string
}

// Sign returns the HeaderSignature value. Field order is part of the contract:
// app-side verifiers rebuild this exact string.
func (a Assertion) Sign(key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	ts := strconv.FormatInt(a.Timestamp, 10)
	mac.Write([]byte(strings.Join([]string{
		"v1", ts, a.Method, a.Host, a.URI, a.Verify,
		a.Subject, a.Issuer, a.Fingerprint, a.Serial, a.NotAfter,
	}, "\n")))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// AppKey derives an app's signing key from the API key, the same root every
// other token hangs off, so nothing per-app has to be stored or diffed.
func AppKey(authKey, app string) string {
	mac := hmac.New(sha256.New, []byte(authKey))
	mac.Write([]byte("odac-client-cert:" + app))
	return hex.EncodeToString(mac.Sum(nil))
}

// AppIdentity is the name an app's signing key is derived from. It survives a
// Blue-Green rename, which is what keeps the container's copy of the key matching
// the proxy's across a deploy.
func AppIdentity(app map[string]any) string {
	if identity, _ := app["_appIdentity"].(string); identity != "" {
		return identity
	}
	return fmt.Sprint(app["name"])
}

// Fingerprint identifies a certificate by the SHA-256 of its DER. Serials are
// not used because an external CA chooses its own and they can collide.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// WildcardOf returns the one-label wildcard covering host, or "" when host has
// no parent. Only one label matches, the same reach as a TLS wildcard.
func WildcardOf(host string) string {
	_, parent, ok := strings.Cut(host, ".")
	if !ok || parent == "" {
		return ""
	}
	return "*." + parent
}

// Covers reports whether a certificate scope opens host.
func Covers(scope []string, host string) bool {
	wildcard := WildcardOf(host)
	for _, entry := range scope {
		if entry == host || (wildcard != "" && entry == wildcard) {
			return true
		}
	}
	return false
}

// AppIDs returns the appId of every record with a host under a policy. An app
// in this set loses the proxy's parent walk on all its records, because any
// other way to reach its code would route around the policy.
func AppIDs(domains map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, rec := range domains {
		record, _ := rec.(map[string]any)
		if record == nil {
			continue
		}
		if recordHasPolicy(record) {
			out[fmt.Sprint(record["appId"])] = true
		}
	}
	return out
}

func recordHasPolicy(record map[string]any) bool {
	cfg, _ := record["mtls"].(map[string]any)
	if cfg == nil {
		return false
	}
	if isActiveMode(cfg["mode"]) {
		return true
	}
	hosts, _ := cfg["hosts"].(map[string]any)
	for _, h := range hosts {
		override, _ := h.(map[string]any)
		if override != nil && isActiveMode(override["mode"]) {
			return true
		}
	}
	return false
}

func isActiveMode(mode any) bool {
	return mode == ModeRequired || mode == ModeOptional
}

// DomainPayload builds a record's proxy entry for an app in the AppIDs set.
// Forgetting this on any record would leave it silently public, since the proxy
// ignores what it is not sent.
func DomainPayload(record map[string]any, appKey string) map[string]any {
	out := map[string]any{"key": appKey}
	cfg, _ := record["mtls"].(map[string]any)
	if cfg == nil {
		return out
	}
	if isActiveMode(cfg["mode"]) {
		out["mode"] = cfg["mode"]
	}
	if ca := cfg["ca"]; ca != nil {
		out["ca"] = ca
	}
	if hosts, _ := cfg["hosts"].(map[string]any); len(hosts) > 0 {
		out["hosts"] = hosts
	}
	return out
}

// CertSnapshot is the proxy's view of the issued certificates. Scopes is already
// JSON, an object of fingerprint to host list, since a push can carry a very large
// one and building it as a map only to marshal it would be most of the cost.
type CertSnapshot struct {
	Revoked []string
	Scopes  json.RawMessage
}

// StorePayload builds the proxy's mtls member from config.mtls, which holds the
// CAs, and the certificates. Private keys are in neither, so they cannot reach this
// payload. It returns nil when empty.
func StorePayload(store map[string]any, certs CertSnapshot) map[string]any {
	cas, _ := store["cas"].(map[string]any)
	if len(cas) == 0 && !certs.hasEntries() {
		return nil
	}

	outCAs := map[string]any{}
	for id, rec := range cas {
		ca, _ := rec.(map[string]any)
		if ca != nil {
			outCAs[id] = map[string]any{"cert": ca["cert"], "managed": ca["managed"] == true}
		}
	}

	scopes := certs.Scopes
	if len(scopes) == 0 {
		scopes = json.RawMessage("{}")
	}
	revoked := certs.Revoked
	if revoked == nil {
		revoked = []string{}
	}
	return map[string]any{"cas": outCAs, "certs": scopes, "revoked": revoked}
}

func (c CertSnapshot) hasEntries() bool {
	return len(c.Revoked) > 0 || (len(c.Scopes) > 0 && string(c.Scopes) != "{}")
}
