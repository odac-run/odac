package mtls

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoversMatchesOneLabelOnly(t *testing.T) {
	scope := []string{"auth.x.com", "*.y.com"}
	cases := map[string]bool{
		"auth.x.com":  true,
		"admin.x.com": false,
		"x.com":       false,
		"a.y.com":     true,
		"a.b.y.com":   false,
		"y.com":       false,
		"":            false,
		"localhost":   false,
	}
	for host, want := range cases {
		if got := Covers(scope, host); got != want {
			t.Errorf("Covers(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestEmptyScopeOpensNothing(t *testing.T) {
	if Covers(nil, "auth.x.com") {
		t.Fatal("an empty scope must open nothing")
	}
}

// Computed independently with openssl dgst -sha256 -hmac.
const signatureVector = "ec8e048b03db89b4bb98eeb899e61e8085215ad6ca7e19dbfed694ac119a3e02"

// Pinned vector: app-side verifiers rebuild this string, so changing it is a
// wire break for every deployed app.
func TestAssertionSignatureVector(t *testing.T) {
	a := Assertion{
		Timestamp: 1700000000, Method: "GET", Host: "auth.x.com", URI: "/a?b=1",
		Verify: VerifySuccess, Subject: "CN=ali", Issuer: "aa", Fingerprint: "bb", Serial: "cc", NotAfter: "1800000000000",
	}
	got := a.Sign("secret")
	want := "t=1700000000,v1=" + signatureVector
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func TestAssertionSignatureCoversRoute(t *testing.T) {
	base := Assertion{Timestamp: 1, Method: "GET", Host: "auth.x.com", URI: "/", Verify: VerifySuccess}
	other := base
	other.Host = "admin.x.com"
	if base.Sign("k") == other.Sign("k") {
		t.Fatal("a signature must not carry over to another host")
	}
}

func TestAppKeyIsPerApp(t *testing.T) {
	if AppKey("root", "a") == AppKey("root", "b") {
		t.Fatal("apps must not share a signing key")
	}
	if strings.Contains(AppKey("root", "a"), "root") {
		t.Fatal("the app key must not expose the root key")
	}
}

func TestAppIDsFindsOverridesAndIgnoresOff(t *testing.T) {
	domains := map[string]any{
		"record.com":   map[string]any{"appId": "viaRecord", "mtls": map[string]any{"mode": ModeOptional}},
		"override.com": map[string]any{"appId": "viaOverride", "mtls": map[string]any{"hosts": map[string]any{"a.override.com": map[string]any{"mode": ModeRequired}}}},
		"off.com":      map[string]any{"appId": "off", "mtls": map[string]any{"ca": []any{"id"}, "hosts": map[string]any{"a.off.com": map[string]any{"mode": ModeOff}}}},
		"plain.com":    map[string]any{"appId": "plain"},
	}
	got := AppIDs(domains)
	if !got["viaRecord"] || !got["viaOverride"] || got["off"] || got["plain"] {
		t.Fatalf("AppIDs = %v", got)
	}
}

func TestStorePayloadHidesCAExtras(t *testing.T) {
	store := map[string]any{
		"cas": map[string]any{"id": map[string]any{"cert": "PEM", "managed": true, "name": "n", "created": float64(1)}},
	}
	out := StorePayload(store, CertSnapshot{Revoked: []string{"gone"}, Scopes: json.RawMessage(`{"live":["a.x.com"]}`)})
	if string(out["certs"].(json.RawMessage)) != `{"live":["a.x.com"]}` {
		t.Fatalf("certs = %s", out["certs"])
	}
	if revoked := out["revoked"].([]string); len(revoked) != 1 || revoked[0] != "gone" {
		t.Fatalf("revoked = %v", revoked)
	}
	ca := out["cas"].(map[string]any)["id"].(map[string]any)
	if _, leaked := ca["name"]; leaked || len(ca) != 2 {
		t.Fatalf("ca payload = %v", ca)
	}
}

// A revocation with no CA behind it still has to reach the proxy.
func TestStorePayloadKeepsRevocationsWithoutCAs(t *testing.T) {
	out := StorePayload(map[string]any{}, CertSnapshot{Revoked: []string{"gone"}, Scopes: json.RawMessage("{}")})
	if out == nil {
		t.Fatal("payload dropped a revocation")
	}
}

// A CA with no certificates must still decode on the proxy side.
func TestStorePayloadFillsEmptyMembers(t *testing.T) {
	raw, err := json.Marshal(StorePayload(map[string]any{"cas": map[string]any{"id": map[string]any{"cert": "PEM"}}}, CertSnapshot{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"certs":{}`) || !strings.Contains(string(raw), `"revoked":[]`) {
		t.Fatalf("payload = %s", raw)
	}
}

func TestStorePayloadEmptyIsNil(t *testing.T) {
	if out := StorePayload(map[string]any{}, CertSnapshot{Scopes: json.RawMessage("{}")}); out != nil {
		t.Fatalf("payload = %v", out)
	}
}
