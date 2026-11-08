package domains

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/api"
	"odac/internal/mtls"
)

func (fx *mtlsFixture) caIDs() map[string]bool {
	out := map[string]bool{}
	fx.cfg.View(func() {
		for id := range fx.m.casLocked(false) {
			out[id] = true
		}
	})
	return out
}

func (fx *mtlsFixture) caKeyExists(id string) bool {
	_, err := os.Stat(filepath.Join(fx.base, "cert", "mtls", id+".key"))
	return err == nil
}

func (fx *mtlsFixture) importCA(t *testing.T) string {
	t.Helper()
	ca := newSharedCA(t, 0)
	return mustOK(t, fx.m.ImportCA(api.Operator, ca.certPEM, ca.keyPEM)).Data.(map[string]any)["id"].(string)
}

func onlyCA(t *testing.T, fx *mtlsFixture, record string) string {
	t.Helper()
	ids := stringList(fx.record(record)["ca"])
	if len(ids) != 1 {
		t.Fatalf("%s trusts %v", record, ids)
	}
	return ids[0]
}

func TestReplacingACAWithNoCertificatesRemovesIt(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	old := onlyCA(t, fx, "x.com")
	shared := fx.importCA(t)

	r := mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{shared}}))
	if !strings.Contains(str(r.Message), "Removed client CA "+old) {
		t.Fatalf("message = %q", r.Message)
	}
	if ids := fx.caIDs(); ids[old] || !ids[shared] {
		t.Fatalf("cas = %v", ids)
	}
	if fx.caKeyExists(old) {
		t.Fatal("the key of a removed CA is still on disk")
	}
}

func TestReplacingACAWithValidCertificatesKeepsIt(t *testing.T) {
	fx := newMTLSFixture(t)
	issued := fx.issue(t, "x.com", "ali", "auth.x.com")
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	old := issued["ca"].(string)
	shared := fx.importCA(t)

	r := mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{shared}}))
	if msg := str(r.Message); !strings.Contains(msg, "Kept client CA "+old) || !strings.Contains(msg, "--force") || !strings.HasSuffix(msg, "The certificates: ali.") {
		t.Fatalf("message = %q", r.Message)
	}
	if !fx.caIDs()[old] || !fx.caKeyExists(old) {
		t.Fatal("a CA with a valid certificate was removed")
	}

	// The mistake is undone by trusting it again, and the certificate still opens its host.
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{old}}))
	if store, _ := fx.proxyPayload(t); len(store.Certs[issued["fingerprint"].(string)]) != 1 {
		t.Fatalf("certs = %v", store.Certs)
	}
}

func TestAChangeNeverRemovesACAThatWasNotTrustedBefore(t *testing.T) {
	fx := newMTLSFixture(t)
	waiting := fx.importCA(t)
	mustOK(t, fx.m.Set("y.com", "optional", nil))
	mustOK(t, fx.m.Set("y.com", "off", nil))
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	if !fx.caIDs()[waiting] || !fx.caKeyExists(waiting) {
		t.Fatal("an imported CA waiting to be trusted was removed")
	}
}

func TestOffKeepsTheCA(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	id := onlyCA(t, fx, "x.com")
	mustOK(t, fx.m.Set("x.com", "off", nil))
	if !fx.caIDs()[id] {
		t.Fatal("off removed the CA it keeps for turning back on")
	}
}

func TestDroppingAnOverrideCARemovesIt(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	own := fx.importCA(t)
	mustOK(t, fx.m.Set("auth.x.com", "optional", map[string]any{"ca": []any{own}}))
	mustOK(t, fx.m.Set("auth.x.com", "off", nil))
	if fx.caIDs()[own] {
		t.Fatal("a CA only an override trusted outlived the override")
	}
}

func TestRemoveCAIsTheOperatorsAlone(t *testing.T) {
	fx := newMTLSFixture(t)
	id := fx.importCA(t)
	for _, caller := range []api.Caller{{App: "myapp"}, {}, {Root: true, App: "myapp"}} {
		mustFail(t, fx.m.RemoveCA(caller, id, nil), "permission_denied")
	}
	if !fx.caIDs()[id] {
		t.Fatal("a refused remove deleted the CA")
	}
	if api.AppMayCall("domain.mtls.ca.remove") {
		t.Fatal("no grant may ever reach a CA removal")
	}
}

func TestRemoveCARefusesWhatIsInUse(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.m.RemoveCA(api.Operator, "not-an-id", nil), "Unknown CA")
	mustFail(t, fx.m.RemoveCA(api.Operator, strings.Repeat("a", 16), nil), "Unknown CA")

	mustOK(t, fx.m.Set("x.com", "optional", nil))
	recordCA := onlyCA(t, fx, "x.com")
	mustFail(t, fx.m.RemoveCA(api.Operator, recordCA, nil), "still trusted on x.com")

	own := fx.importCA(t)
	mustOK(t, fx.m.Set("auth.x.com", "optional", map[string]any{"ca": []any{own}}))
	mustFail(t, fx.m.RemoveCA(api.Operator, own, map[string]any{"force": true}), "still trusted on auth.x.com")
	if ids := fx.caIDs(); !ids[recordCA] || !ids[own] {
		t.Fatalf("cas = %v", ids)
	}
}

func TestRemoveCANeedsForceForValidCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	issued := fx.issue(t, "x.com", "ali", "auth.x.com")
	old := issued["ca"].(string)
	revoked := fx.issue(t, "x.com", "veli", "auth.x.com")
	mustOK(t, fx.m.Revoke(api.Operator, revoked["fingerprint"]))
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{fx.importCA(t)}}))

	mustFail(t, fx.m.RemoveCA(api.Operator, old, nil), "1 valid certificate(s): ali.")
	before := fx.proxy.syncCount()
	r := mustOK(t, fx.m.RemoveCA(api.Operator, strings.ToUpper(old), map[string]any{"force": true}))
	if !strings.Contains(str(r.Message), "1 certificate(s) it signed no longer work: ali.") {
		t.Fatalf("message = %q", r.Message)
	}
	if fx.caIDs()[old] || fx.caKeyExists(old) {
		t.Fatal("a forced remove left the CA behind")
	}
	if fx.proxy.syncCount() == before {
		t.Fatal("the proxy was not told")
	}
	if rec := storedCert(t, fx.m, issued["fingerprint"].(string)); rec != nil {
		t.Fatal("a certificate of a removed CA is still recorded")
	}
	// A revocation outlives its CA, so importing the CA again cannot revive it.
	if rec := storedCert(t, fx.m, revoked["fingerprint"].(string)); rec == nil || rec["revoked"] == nil {
		t.Fatalf("revocation = %v", rec)
	}
}

func TestRemoveCAClearsARecordThatWasTurnedOff(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	id := onlyCA(t, fx, "x.com")
	mustOK(t, fx.m.Set("x.com", "off", nil))

	mustOK(t, fx.m.RemoveCA(api.Operator, id, nil))
	if cfg := fx.record("x.com"); cfg != nil {
		t.Fatalf("record = %v", cfg)
	}
	// Turning it on again makes a new CA rather than naming the removed one.
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	if got := onlyCA(t, fx, "x.com"); got == id || !fx.caIDs()[got] {
		t.Fatalf("x.com trusts %s", got)
	}
}

func TestRemovingACATakesItsPerHostIssuers(t *testing.T) {
	fx := newMTLSFixture(t)
	first := fx.importCA(t)
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{first}, "issuer": mtls.IssuerPerHost}))
	issued := fx.issue(t, "x.com", "ali", "auth.x.com")
	issuerKey := fx.m.issuerKeyPath(first, "auth.x.com")
	if _, err := os.Stat(issuerKey); err != nil {
		t.Fatalf("no issuer was made: %v", err)
	}
	mustOK(t, fx.m.Revoke(api.Operator, issued["fingerprint"]))

	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{fx.importCA(t)}}))
	if fx.caIDs()[first] {
		t.Fatal("the replaced CA is still here")
	}
	if _, err := os.Stat(issuerKey); !os.IsNotExist(err) {
		t.Fatal("the issuer key of a removed CA is still on disk")
	}
	if _, raw := fx.proxyPayload(t); strings.Contains(raw, `"issuers"`) {
		t.Fatalf("payload = %s", raw)
	}
}

func TestRemoveCANamesAtMostTenCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	var id string
	for i := 0; i < 12; i++ {
		id = fx.issue(t, "x.com", fmt.Sprintf("user%02d", i), "auth.x.com")["ca"].(string)
	}
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{fx.importCA(t)}}))
	r := fx.m.RemoveCA(api.Operator, id, nil)
	msg := str(r.Message)
	if r.Status || !strings.Contains(msg, "12 valid certificate(s): user00, user01,") || !strings.Contains(msg, "user09 and 2 more.") || strings.Contains(msg, "user10") {
		t.Fatalf("message = %q", msg)
	}
}

// A name is the caller's text, so it must not be able to steer the message.
func TestCANamesCannotReachIntoTheMessage(t *testing.T) {
	fx := newMTLSFixture(t)
	id := fx.issue(t, "x.com", "50% off %s", "auth.x.com")["ca"].(string)
	r := mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{fx.importCA(t)}}))
	if msg := str(r.Message); !strings.Contains(msg, "--ca "+id+" --force") || !strings.HasSuffix(msg, "The certificates: 50% off %s.") {
		t.Fatalf("message = %q", msg)
	}
}

func TestPerHostSwitchPromisesNothingWithoutCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	shared := fx.importCA(t)
	r := mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{shared}, "issuer": mtls.IssuerPerHost}))
	if msg := str(r.Message); !strings.Contains(msg, "own issuer.") || strings.Contains(msg, "Existing certificates") {
		t.Fatalf("message = %q", msg)
	}

	fx = newMTLSFixture(t)
	shared = fx.importCA(t)
	mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"ca": []any{shared}}))
	fx.issue(t, "x.com", "ali", "auth.x.com")
	r = mustOK(t, fx.m.Set("x.com", "optional", map[string]any{"issuer": mtls.IssuerPerHost}))
	if !strings.Contains(str(r.Message), "Existing certificates keep working.") {
		t.Fatalf("message = %q", r.Message)
	}
}
