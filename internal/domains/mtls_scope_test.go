package domains

import (
	"strings"
	"testing"
	"time"

	"odac/internal/api"
)

// The fixture routes x.com and y.com to myapp, z.com and *.w.com to other.
var (
	asApp   = api.Caller{App: "myapp"}
	asOther = api.Caller{App: "other"}
)

func wantNotFound(t *testing.T, r api.Result) {
	t.Helper()
	data, _ := r.Data.(map[string]any)
	if r.Status || data["code"] != "not_found" {
		t.Fatalf("want not_found, got status=%v message=%v data=%v", r.Status, r.Message, r.Data)
	}
}

func (fx *mtlsFixture) issueAs(t *testing.T, caller api.Caller, domain, name string, hosts ...string) api.Result {
	t.Helper()
	return fx.m.Issue(caller, domain, map[string]any{"name": name, "hosts": anyList(hosts), "csr": newCSR(t)})
}

func listNames(t *testing.T, r api.Result) []string {
	t.Helper()
	var names []string
	for _, row := range mustOK(t, r).Data.([]any) {
		names = append(names, row.(map[string]any)["name"].(string))
	}
	return names
}

// sharedCA binds x.com's CA to z.com as well, so one CA spans two apps.
func (fx *mtlsFixture) sharedCA(t *testing.T) string {
	t.Helper()
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	id := stringList(fx.record("x.com")["ca"])[0]
	mustOK(t, fx.m.Set("z.com", "optional", map[string]any{"ca": []any{id}}))
	return id
}

func TestAppIssuesOnlyOnItsOwnRecords(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	mustOK(t, fx.m.Set("z.com", "optional", nil))

	mustOK(t, fx.issueAs(t, asApp, "x.com", "dev1", "auth.x.com"))
	mustFail(t, fx.issueAs(t, asApp, "z.com", "dev2", "z.com"), "not found")
	mustFail(t, fx.issueAs(t, asApp, "x.com", "dev3", "z.com"), "Host z.com is not registered")
	mustFail(t, fx.issueAs(t, asApp, "foo.w.com", "dev4", "foo.w.com"), "not found")

	if names := listNames(t, fx.m.List(api.Operator, nil)); len(names) != 1 || names[0] != "dev1" {
		t.Fatalf("certificates = %v", names)
	}
}

// One foreign host poisons the whole request, wherever it sits in the list, and
// it reads as unregistered even though it trusts the very CA being used.
func TestAppCannotMixInAnotherAppsHost(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.sharedCA(t)

	mustFail(t, fx.issueAs(t, asApp, "x.com", "dev1", "x.com", "z.com"), "Host z.com is not registered")
	mustFail(t, fx.issueAs(t, asApp, "x.com", "dev1", "z.com", "x.com"), "Host z.com is not registered")
	if names := listNames(t, fx.m.List(api.Operator, nil)); len(names) != 0 {
		t.Fatalf("a refused request left %v behind", names)
	}
	// The operator is not narrowed.
	mustOK(t, fx.issueAs(t, api.Operator, "x.com", "both", "x.com", "z.com"))
}

func TestAppCannotCreateACA(t *testing.T) {
	fx := newMTLSFixture(t)
	mustFail(t, fx.issueAs(t, asApp, "y.com", "dev1", "y.com"), "not set up")
	if fx.record("y.com") != nil {
		t.Fatal("an app's issue must not bind a CA to its record")
	}
	var cas map[string]any
	fx.cfg.View(func() { cas = fx.m.casLocked(false) })
	if len(cas) != 0 {
		t.Fatalf("an app's issue created a CA: %v", cas)
	}
}

func TestAppRenewalStaysOnItsHosts(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.sharedCA(t)
	old := mustOK(t, fx.issueAs(t, api.Operator, "x.com", "both", "x.com", "z.com")).Data.(map[string]any)

	// Without hosts a renewal takes the old scope, which reaches z.com.
	mustFail(t, fx.m.Issue(asApp, "x.com", map[string]any{"renews": old["fingerprint"], "csr": newCSR(t)}), "Host z.com is not registered")
	mustOK(t, fx.m.Issue(asApp, "x.com", map[string]any{"renews": old["fingerprint"], "csr": newCSR(t), "hosts": []any{"x.com"}}))
}

func TestAppRevokesOnlyWhatItOwns(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	mustOK(t, fx.m.Set("z.com", "optional", nil))
	mine := mustOK(t, fx.issueAs(t, asApp, "x.com", "dev1", "x.com")).Data.(map[string]any)["fingerprint"].(string)
	theirs := mustOK(t, fx.issueAs(t, asOther, "z.com", "dev2", "z.com")).Data.(map[string]any)["fingerprint"].(string)

	wantNotFound(t, fx.m.Revoke(asApp, theirs))
	if fx.certRecord(t, theirs)["revoked"] != nil {
		t.Fatal("an app revoked another app's certificate")
	}

	// The unknown-fingerprint branch is the operator's: an app must not be able
	// to plant a revocation for a certificate it never saw.
	unknown := strings.Repeat("cd", 32)
	wantNotFound(t, fx.m.Revoke(asApp, unknown))
	if fx.certRecord(t, unknown) != nil {
		t.Fatal("an app's revoke left a record for an unknown fingerprint")
	}

	mustOK(t, fx.m.Revoke(asApp, mine))
	if fx.certRecord(t, mine)["revoked"] == nil {
		t.Fatal("not revoked")
	}

	// The operator keeps revoking what ODAC has no record of; the app still
	// cannot touch that record, which has no CA to tie it to anyone.
	mustOK(t, fx.m.Revoke(api.Operator, unknown))
	wantNotFound(t, fx.m.Revoke(asApp, unknown))
}

// A5: a queue retrying a revocation whose reply was lost must hear success, and
// the first revocation time stays.
func TestRevokingTwiceSucceeds(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	for _, caller := range []api.Caller{api.Operator, asApp} {
		fp := mustOK(t, fx.issueAs(t, asApp, "x.com", "dev-"+caller.App, "x.com")).Data.(map[string]any)["fingerprint"].(string)
		mustOK(t, fx.m.Revoke(caller, fp))
		first := fx.certRecord(t, fp)["revoked"]
		fx.m.now = func() time.Time { return time.Now().Add(time.Hour) }
		mustOK(t, fx.m.Revoke(caller, fp))
		fx.m.now = time.Now
		if got := fx.certRecord(t, fp)["revoked"]; got != first {
			t.Fatalf("%+v: revoked moved from %v to %v", caller, first, got)
		}
	}
}

func TestAppListsOnlyItsOwnCertificates(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	mustOK(t, fx.m.Set("z.com", "optional", nil))
	mustOK(t, fx.issueAs(t, asApp, "x.com", "dev1", "x.com"))
	mustOK(t, fx.issueAs(t, asOther, "z.com", "dev2", "z.com"))

	if names := listNames(t, fx.m.List(asApp, nil)); len(names) != 1 || names[0] != "dev1" {
		t.Fatalf("myapp sees %v", names)
	}
	if names := listNames(t, fx.m.List(asOther, nil)); len(names) != 1 || names[0] != "dev2" {
		t.Fatalf("other sees %v", names)
	}
	if names := listNames(t, fx.m.List(api.Operator, nil)); len(names) != 2 {
		t.Fatalf("operator sees %v", names)
	}
	if names := listNames(t, fx.m.List(asApp, "www.x.com")); len(names) != 1 {
		t.Fatalf("filtered = %v", names)
	}
	mustFail(t, fx.m.List(asApp, "z.com"), "not found")
	if names := listNames(t, fx.m.List(api.Caller{App: "ghost"}, nil)); len(names) != 0 {
		t.Fatalf("an app with no records sees %v", names)
	}
}

// A CA bound to records of two apps must not hand either one the other's
// certificates: ownership needs the scope too, not only the CA.
func TestSharedCAKeepsEachAppToItsHosts(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.sharedCA(t)
	both := mustOK(t, fx.issueAs(t, api.Operator, "x.com", "both", "x.com", "z.com")).Data.(map[string]any)["fingerprint"].(string)
	mustOK(t, fx.issueAs(t, api.Operator, "x.com", "mine", "x.com"))
	mustOK(t, fx.issueAs(t, api.Operator, "z.com", "theirs", "z.com"))

	if names := listNames(t, fx.m.List(asApp, nil)); len(names) != 1 || names[0] != "mine" {
		t.Fatalf("myapp sees %v", names)
	}
	if names := listNames(t, fx.m.List(asOther, nil)); len(names) != 1 || names[0] != "theirs" {
		t.Fatalf("other sees %v", names)
	}
	wantNotFound(t, fx.m.Revoke(asApp, both))
	wantNotFound(t, fx.m.Revoke(asOther, both))
}

// The reply to a revoke names the other certificates on the same key; for an app
// that list stops at what it owns.
func TestSameKeyReplyStaysWithTheApp(t *testing.T) {
	fx := newMTLSFixture(t)
	fx.sharedCA(t)
	csr := newCSR(t)
	a := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"name": "ali", "hosts": []any{"x.com"}, "csr": csr})).Data.(map[string]any)
	b := mustOK(t, fx.m.Issue(api.Operator, "x.com", map[string]any{"renews": a["fingerprint"], "csr": csr, "hosts": []any{"x.com", "z.com"}})).Data.(map[string]any)

	r := mustOK(t, fx.m.Revoke(asApp, a["fingerprint"]))
	if data, _ := r.Data.(map[string]any); data != nil {
		t.Fatalf("myapp was told about %v, which also opens other's host", data["sameKey"])
	}
	if strings.Contains(r.Message.(string), b["fingerprint"].(string)) {
		t.Fatal("the message names another app's certificate")
	}
}

// A domain token reaches a handler only for mail.send, but a handler that
// narrows by caller must still refuse a caller that is neither root nor an app.
func TestUnclassifiedCallerReachesNothing(t *testing.T) {
	fx := newMTLSFixture(t)
	mustOK(t, fx.m.Set("x.com", "optional", nil))
	var nobody api.Caller
	for _, r := range []api.Result{
		fx.issueAs(t, nobody, "x.com", "dev1", "x.com"),
		fx.m.Revoke(nobody, strings.Repeat("ab", 32)),
		fx.m.List(nobody, nil),
	} {
		mustFail(t, r, "permission_denied")
	}
	if fx.certRecord(t, strings.Repeat("ab", 32)) != nil {
		t.Fatal("a refused revoke wrote a record")
	}
}
