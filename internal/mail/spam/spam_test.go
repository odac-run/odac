package spam

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"

	"odac/internal/mail/config"
)

// fakeDNS answers from maps; a missing name is NXDOMAIN, and a name in
// tempFail times out.
type fakeDNS struct {
	txt      map[string][]string
	tempFail map[string]bool
}

func key(name string) string { return strings.TrimSuffix(strings.ToLower(name), ".") }

func (d *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if d.tempFail[key(name)] {
		return nil, &net.DNSError{Err: "timeout", Name: name, IsTimeout: true}
	}
	if v, ok := d.txt[key(name)]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *fakeDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *fakeDNS) LookupIPAddr(_ context.Context, name string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *fakeDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
}

const senderIP = "198.51.100.25"

func newDNS() *fakeDNS {
	return &fakeDNS{
		txt: map[string][]string{
			"sender.example":        {"v=spf1 ip4:" + senderIP + " -all"},
			"bounce.sender.example": {"v=spf1 ip4:" + senderIP + " -all"},
			"_dmarc.sender.example": {"v=DMARC1; p=reject"},
		},
		tempFail: map[string]bool{},
	}
}

func newFilter(dns *fakeDNS, mode Mode) *Filter {
	cfg := config.Config{
		Hostname: "mx.local.test",
		Domains:  map[string]config.Domain{"local.test": {}},
	}
	f := New(func() config.Config { return cfg }, mode, WithResolver(dns))
	f.roll = func() int { return 0 }
	return f
}

func env(from string) Envelope {
	return Envelope{IP: net.ParseIP(senderIP), Helo: "mail.sender.example", MailFrom: from}
}

func message(from string) []byte {
	return []byte("From: Sender <" + from + ">\r\n" +
		"To: user@local.test\r\n" +
		"Subject: hello\r\n" +
		"Date: Thu, 19 Nov 2026 10:00:00 +0000\r\n" +
		"Message-ID: <1@sender.example>\r\n" +
		"\r\n" +
		"body\r\n")
}

// sign DKIM-signs raw for domain and publishes the key in dns.
func sign(t *testing.T, dns *fakeDNS, domain string, raw []byte) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dns.txt["sel._domainkey."+domain] = []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}
	var out bytes.Buffer
	if err := dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "sel", Signer: priv}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func hasTest(v Verdict, name string) bool { return slices.Contains(v.Tests, name) }

func TestAuthenticatedMailGoesToInbox(t *testing.T) {
	dns := newDNS()
	raw := sign(t, dns, "sender.example", message("news@sender.example"))
	v := newFilter(dns, ModeEnforce).Check(context.Background(), env("news@sender.example"), raw)

	if v.Action != Inbox || !hasTest(v, "DMARC_PASS") {
		t.Fatalf("got %s %v, want inbox with DMARC_PASS", v.Action, v.Tests)
	}
	stamped := string(v.Stamp(raw))
	for _, want := range []string{"Authentication-Results: mx.local.test;", "spf=pass", "dkim=pass", "header.d=sender.example", "dmarc=pass", "X-Spam-Status: No"} {
		if !strings.Contains(stamped, want) {
			t.Errorf("stamped message lacks %q:\n%s", want, stamped)
		}
	}
	if !strings.HasSuffix(stamped, string(raw)) {
		t.Error("Stamp must only prepend, the original bytes stay intact")
	}
}

func TestDMARCRejectSpoof(t *testing.T) {
	spoof := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	raw := message("ceo@sender.example")

	cases := map[Mode]Action{ModeEnforce: Reject, ModeJunk: Junk, ModeTag: Inbox}
	for mode, want := range cases {
		v := newFilter(newDNS(), mode).Check(context.Background(), spoof, raw)
		if v.Action != want {
			t.Errorf("mode %s: got %s, want %s (tests %v)", mode, v.Action, want, v.Tests)
		}
		if !hasTest(v, "DMARC_REJECT") {
			t.Errorf("mode %s: missing DMARC_REJECT in %v", mode, v.Tests)
		}
	}

	v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), spoof, raw)
	if !strings.Contains(v.Reason, "sender.example") {
		t.Errorf("reject reason %q should name the domain", v.Reason)
	}
}

func TestDMARCQuarantineGoesToJunk(t *testing.T) {
	dns := newDNS()
	dns.txt["_dmarc.sender.example"] = []string{"v=DMARC1; p=quarantine"}
	spoof := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	v := newFilter(dns, ModeEnforce).Check(context.Background(), spoof, message("ceo@sender.example"))
	if v.Action != Junk {
		t.Fatalf("got %s %v, want junk", v.Action, v.Tests)
	}
}

// A message outside the pct sample gets the next weaker policy.
func TestDMARCPercentDowngrades(t *testing.T) {
	dns := newDNS()
	dns.txt["_dmarc.sender.example"] = []string{"v=DMARC1; p=reject; pct=10"}
	f := newFilter(dns, ModeEnforce)
	f.roll = func() int { return 50 }
	spoof := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	v := f.Check(context.Background(), spoof, message("ceo@sender.example"))
	if v.Action != Junk || !hasTest(v, "DMARC_QUARANTINE") {
		t.Fatalf("got %s %v, want junk via quarantine", v.Action, v.Tests)
	}
}

// A subdomain with no record of its own takes the organizational domain's sp.
func TestDMARCSubdomainPolicy(t *testing.T) {
	dns := newDNS()
	dns.txt["_dmarc.sender.example"] = []string{"v=DMARC1; p=none; sp=reject"}
	spoof := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	v := newFilter(dns, ModeEnforce).Check(context.Background(), spoof, message("ceo@billing.sender.example"))
	if v.Action != Reject {
		t.Fatalf("got %s %v, want reject from sp", v.Action, v.Tests)
	}
}

func TestSPFAlignment(t *testing.T) {
	dns := newDNS()
	v := newFilter(dns, ModeEnforce).Check(context.Background(), env("b@bounce.sender.example"), message("news@sender.example"))
	if v.Action != Inbox || !hasTest(v, "DMARC_PASS") {
		t.Fatalf("relaxed: got %s %v, want an aligned pass", v.Action, v.Tests)
	}

	dns.txt["_dmarc.sender.example"] = []string{"v=DMARC1; p=reject; aspf=s"}
	v = newFilter(dns, ModeEnforce).Check(context.Background(), env("b@bounce.sender.example"), message("news@sender.example"))
	if v.Action != Reject {
		t.Fatalf("strict: got %s %v, want reject", v.Action, v.Tests)
	}
}

// A resolver failure must never be read as a policy failure.
func TestDMARCTempErrorIsNotEvidence(t *testing.T) {
	dns := newDNS()
	dns.tempFail["_dmarc.sender.example"] = true
	spoof := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	v := newFilter(dns, ModeEnforce).Check(context.Background(), spoof, message("ceo@sender.example"))
	if v.Action == Reject || hasTest(v, "DMARC_REJECT") {
		t.Fatalf("got %s %v, a temperror must not apply the policy", v.Action, v.Tests)
	}
	if !strings.Contains(string(v.Stamp(nil)), "dmarc=temperror") {
		t.Error("the header should report dmarc=temperror")
	}
}

func TestTamperedDKIM(t *testing.T) {
	dns := newDNS()
	delete(dns.txt, "_dmarc.sender.example")
	raw := sign(t, dns, "sender.example", message("news@sender.example"))
	raw = bytes.Replace(raw, []byte("body"), []byte("evil"), 1)
	other := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "relay.other.example", MailFrom: "x@other.example"}
	v := newFilter(dns, ModeEnforce).Check(context.Background(), other, raw)
	if !hasTest(v, "DKIM_INVALID") || !strings.Contains(string(v.Stamp(nil)), "dkim=fail") {
		t.Fatalf("tests %v, want DKIM_INVALID and dkim=fail", v.Tests)
	}
}

// Unauthenticated mail claiming one of this server's domains, sent with a
// HELO naming it, is the commonest spoof aimed at local users.
func TestLocalDomainSpoofRejected(t *testing.T) {
	forged := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mx.local.test", MailFrom: "boss@local.test"}
	v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), forged, message("boss@local.test"))
	if v.Action != Reject || !strings.Contains(v.Reason, "local.test") {
		t.Fatalf("got %s %v reason=%q, want reject", v.Action, v.Tests, v.Reason)
	}
	for _, want := range []string{"HELO_FORGED_LOCAL", "FROM_LOCAL_SPOOF", "NO_AUTH"} {
		if !hasTest(v, want) {
			t.Errorf("missing %s in %v", want, v.Tests)
		}
	}
}

// The classic impersonation: the attacker's own domain passes SPF and only
// the From header names a local address. Without a DMARC record nothing else
// would stop it short of the inbox.
func TestLocalDomainSpoofWithForeignSPFPass(t *testing.T) {
	dns := newDNS()
	cases := map[string]struct {
		env  Envelope
		raw  []byte
		test string
	}{
		"from":      {env("x@sender.example"), message("founder@local.test"), "FROM_LOCAL_SPOOF"},
		"envelope":  {env("founder@local.test"), sign(t, dns, "sender.example", message("news@sender.example")), "MAILFROM_LOCAL_SPOOF"},
		"subdomain": {env("x@sender.example"), message("news@mail.local.test"), "FROM_LOCAL_SPOOF"},
	}
	for name, tc := range cases {
		v := newFilter(dns, ModeEnforce).Check(context.Background(), tc.env, tc.raw)
		if v.Action != Reject || !hasTest(v, tc.test) {
			t.Errorf("%s: got %s %v, want reject with %s", name, v.Action, v.Tests, tc.test)
		}
	}

	for mode, want := range map[Mode]Action{ModeJunk: Junk, ModeTag: Inbox} {
		v := newFilter(dns, mode).Check(context.Background(), env("x@sender.example"), message("founder@local.test"))
		if v.Action != want {
			t.Errorf("mode %s: got %s, want %s", mode, v.Action, want)
		}
	}
}

// A service the domain's owner authorized, through SPF or a DKIM key on the
// domain, may send as an address with no account behind it.
func TestAuthorizedServiceSendsAsLocalDomain(t *testing.T) {
	dns := newDNS()
	dns.txt["local.test"] = []string{"v=spf1 ip4:" + senderIP + " -all"}
	service := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "out.service.example", MailFrom: "bounce@service.example"}

	cases := map[string]struct {
		env Envelope
		raw []byte
	}{
		"spf":  {env("noreply@local.test"), message("noreply@local.test")},
		"dkim": {service, sign(t, dns, "local.test", message("noreply@local.test"))},
	}
	for name, tc := range cases {
		v := withAccounts(newFilter(dns, ModeEnforce), "ali@local.test").Check(context.Background(), tc.env, tc.raw)
		if v.Action != Inbox || hasTest(v, "FROM_LOCAL_SPOOF") || hasTest(v, "MAILFROM_LOCAL_SPOOF") {
			t.Errorf("%s: got %s %v, want inbox", name, v.Action, v.Tests)
		}
	}
}

// A DNS timeout that may be hiding the pass defers instead of refusing.
func TestLocalDomainTempErrorDefers(t *testing.T) {
	dns := newDNS()
	dns.tempFail["local.test"] = true
	v := newFilter(dns, ModeEnforce).Check(context.Background(), env("noreply@local.test"), message("noreply@local.test"))
	if v.Action != Defer || v.Reason == "" {
		t.Fatalf("got %s %v reason=%q, want defer", v.Action, v.Tests, v.Reason)
	}

	// A timeout on an unrelated domain hides nothing.
	dns = newDNS()
	dns.tempFail["other.example"] = true
	other := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "relay.other.example", MailFrom: "x@other.example"}
	if v := newFilter(dns, ModeEnforce).Check(context.Background(), other, message("noreply@local.test")); v.Action != Reject {
		t.Fatalf("got %s %v, want reject", v.Action, v.Tests)
	}
}

// DMARC p=reject defers when the DKIM key lookup that could pass timed out.
func TestDMARCRejectDefersOnDKIMTempError(t *testing.T) {
	dns := newDNS()
	raw := sign(t, dns, "sender.example", message("ceo@sender.example"))
	dns.tempFail["sel._domainkey.sender.example"] = true
	relay := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "relay.other.example", MailFrom: "x@other.example"}
	v := newFilter(dns, ModeEnforce).Check(context.Background(), relay, raw)
	if v.Action != Defer || hasTest(v, "DMARC_REJECT") {
		t.Fatalf("got %s %v, want defer", v.Action, v.Tests)
	}
}

func TestHeaderShape(t *testing.T) {
	f := newFilter(newDNS(), ModeEnforce)
	cases := map[string]string{
		"FROM_MISSING":  "Subject: x\r\n\r\nbody\r\n",
		"FROM_MULTIPLE": "From: a@sender.example\r\nFrom: b@sender.example\r\n\r\nbody\r\n",
		"FROM_INVALID":  "From: not an address\r\n\r\nbody\r\n",
		"DATE_MISSING":  "From: a@sender.example\r\n\r\nbody\r\n",
		"MSGID_MISSING": "From: a@sender.example\r\n\r\nbody\r\n",
	}
	for test, raw := range cases {
		v := f.Check(context.Background(), env("a@sender.example"), []byte(raw))
		if !hasTest(v, test) {
			t.Errorf("%s: tests %v", test, v.Tests)
		}
	}
}

func TestHeloShape(t *testing.T) {
	f := newFilter(newDNS(), ModeEnforce)
	cases := map[string]string{"203.0.113.9": "HELO_BARE_IP", "localhost": "HELO_NOT_FQDN"}
	for helo, test := range cases {
		e := env("a@sender.example")
		e.Helo = helo
		if v := f.Check(context.Background(), e, message("a@sender.example")); !hasTest(v, test) {
			t.Errorf("HELO %q: tests %v, want %s", helo, v.Tests, test)
		}
	}
	e := env("a@sender.example")
	e.Helo = "[198.51.100.25]"
	if v := f.Check(context.Background(), e, message("a@sender.example")); hasTest(v, "HELO_BARE_IP") || hasTest(v, "HELO_NOT_FQDN") {
		t.Errorf("an address literal is legal HELO, got %v", v.Tests)
	}
}

func TestNotScreened(t *testing.T) {
	raw := message("a@sender.example")
	if v := newFilter(newDNS(), ModeOff).Check(context.Background(), env("a@sender.example"), raw); !bytes.Equal(v.Stamp(raw), raw) || v.Action != Inbox {
		t.Error("off mode must leave the message untouched")
	}
	local := env("a@sender.example")
	local.IP = net.ParseIP("127.0.0.1")
	if v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), local, raw); v.checked {
		t.Error("loopback must not be screened")
	}
	var nilFilter *Filter
	if v := nilFilter.Check(context.Background(), env("a@sender.example"), raw); v.checked {
		t.Error("a nil filter must not screen")
	}
}

// Every sender-controlled value is dropped from the header unless it is a
// plain token, so nothing can inject a line.
func TestStampCannotBeInjected(t *testing.T) {
	raw := []byte("From: a@sender.example\r\nDKIM-Signature: v=1; a=rsa-sha256; d=x\r\n\tevil.example; s=s; i=\"a;b\"@x; h=from; bh=; b=\r\n\r\nbody\r\n")
	v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), env("a@sender.example"), raw)
	head := string(v.Stamp(nil))
	for _, line := range strings.Split(strings.TrimSuffix(head, "\r\n"), "\r\n") {
		if !strings.HasPrefix(line, "Authentication-Results:") && !strings.HasPrefix(line, "X-Spam-Status:") && !strings.HasPrefix(line, "\t") {
			t.Fatalf("unexpected header line %q in:\n%s", line, head)
		}
	}
	if strings.Contains(head, "\n\n") || strings.Count(head, "\n") != strings.Count(head, "\r\n") {
		t.Fatalf("bare LF or empty line in header:\n%q", head)
	}
	if strings.Contains(head, " ;") || strings.Contains(head, " \r\n") {
		t.Fatalf("stray space before a separator:\n%q", head)
	}
	if safe("a b") != "" || safe("a;b") != "" || safe("ok.example") != "ok.example" {
		t.Error("safe must only pass plain tokens")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"off": ModeOff, " TAG ": ModeTag, "junk": ModeJunk, "Enforce": ModeEnforce} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := ParseMode("strict"); ok {
		t.Error("unknown modes must be refused")
	}
	t.Setenv("ODAC_MAIL_SPAM", "bogus")
	if ModeFromEnv() != ModeEnforce {
		t.Error("an invalid value falls back to enforce")
	}
	t.Setenv("ODAC_MAIL_SPAM", "tag")
	if ModeFromEnv() != ModeTag {
		t.Error("ODAC_MAIL_SPAM=tag not honoured")
	}
}

// The parsers see arbitrary sender bytes; Check recovers from a panic, so the
// fuzz target calls the unguarded path.
func FuzzEvaluate(f *testing.F) {
	f.Add([]byte("From: a@sender.example\r\n\r\nbody\r\n"), "mail.sender.example", "a@sender.example")
	f.Add([]byte("From: \"=?utf-8?q?x?=\" <a@b>, c@d\r\nFrom:\r\n\r\n"), "[1.2.3.4]", "")
	f.Add([]byte("DKIM-Signature: v=1; d=; s=;\r\n\r\n"), "x", "a@b.c")
	f.Fuzz(func(t *testing.T, raw []byte, helo, from string) {
		flt := newFilter(newDNS(), ModeEnforce)
		e := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: helo, MailFrom: from}
		v := flt.evaluate(context.Background(), e, raw)
		_ = v.Stamp(raw)
	})
}

func withAccounts(f *Filter, accounts ...string) *Filter {
	f.accounts = func(_ context.Context, email string) (bool, error) {
		return slices.Contains(accounts, email), nil
	}
	return f
}

// A local account only ever sends through an authenticated session, so mail
// from outside naming one is refused even when its SPF and DKIM pass.
func TestForgedLocalAccountRejected(t *testing.T) {
	dns := newDNS()
	dns.txt["local.test"] = []string{"v=spf1 ip4:" + senderIP + " -all"}
	signed := sign(t, dns, "local.test", message("ali@local.test"))

	cases := map[string]struct {
		env Envelope
		raw []byte
	}{
		"envelope": {env("ali@local.test"), message("news@sender.example")},
		"header":   {env("x@sender.example"), message("Ali@Local.test")},
		"verified": {env("ali@local.test"), signed},
	}
	for name, tc := range cases {
		v := withAccounts(newFilter(dns, ModeEnforce), "ali@local.test").Check(context.Background(), tc.env, tc.raw)
		if v.Action != Reject || !hasTest(v, "LOCAL_ACCOUNT_FORGED") || !strings.Contains(v.Reason, "ali@local.test") {
			t.Errorf("%s: got %s %v reason=%q, want reject", name, v.Action, v.Tests, v.Reason)
		}
	}

	for mode, want := range map[Mode]Action{ModeJunk: Junk, ModeTag: Inbox} {
		v := withAccounts(newFilter(dns, mode), "ali@local.test").Check(context.Background(), env("ali@local.test"), message("ali@local.test"))
		if v.Action != want {
			t.Errorf("mode %s: got %s, want %s", mode, v.Action, want)
		}
	}
}

// An address on a local domain with no account behind it falls under the
// domain rule, not the account one.
func TestLocalAddressWithoutAccount(t *testing.T) {
	forged := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	v := withAccounts(newFilter(newDNS(), ModeEnforce), "ali@local.test").Check(context.Background(), forged, message("ceo@local.test"))
	if v.Action != Reject || hasTest(v, "LOCAL_ACCOUNT_FORGED") || !hasTest(v, "FROM_LOCAL_SPOOF") {
		t.Fatalf("got %s %v, want reject by FROM_LOCAL_SPOOF", v.Action, v.Tests)
	}
}

// A store that cannot answer must not turn into a permanent refusal.
func TestAccountLookupFailureDefers(t *testing.T) {
	dns := newDNS()
	dns.txt["local.test"] = []string{"v=spf1 ip4:" + senderIP + " -all"}
	f := newFilter(dns, ModeEnforce)
	f.accounts = func(context.Context, string) (bool, error) { return false, errors.New("database is locked") }
	v := f.Check(context.Background(), env("ali@local.test"), message("ali@local.test"))
	if v.Action != Defer || v.Reason == "" {
		t.Fatalf("got %s reason=%q, want defer", v.Action, v.Reason)
	}
}

func headerMessage(fromLines string) []byte {
	return []byte(fromLines +
		"To: user@local.test\r\n" +
		"Subject: hello\r\n" +
		"Date: Thu, 19 Nov 2026 10:00:00 +0000\r\n" +
		"Message-ID: <1@sender.example>\r\n" +
		"\r\n" +
		"body\r\n")
}

// Every address a client may show as the sender is checked. An ambiguous
// From used to return no author at all, which skipped DMARC and both local
// checks while the mailbox still listed the local address.
func TestAmbiguousFromIsChecked(t *testing.T) {
	cases := map[string]string{
		"second address":   "From: x@sender.example, Boss <boss@local.test>\r\n",
		"second field":     "From: x@sender.example\r\nFrom: Boss <boss@local.test>\r\n",
		"after a bad line": "Subject: x\r\nnot a header\r\nFrom: Boss <boss@local.test>\r\n",
		"unparseable":      "From: Support: Accounts <boss@local.test>\r\n",
		"trailing dot":     "From: <boss@local.test.>\r\n",
		"folded":           "From: Boss\r\n <boss@local.test>\r\n",
	}
	for name, from := range cases {
		v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), env("x@sender.example"), headerMessage(from))
		if v.Action != Reject || !hasTest(v, "FROM_LOCAL_SPOOF") {
			t.Errorf("%s: got %s %v, want reject with FROM_LOCAL_SPOOF", name, v.Action, v.Tests)
		}
	}
}

// RFC 7489 6.6.1: with several author domains the strictest failing policy
// applies.
func TestDMARCAppliesToEveryAuthorDomain(t *testing.T) {
	attacker := Envelope{IP: net.ParseIP("203.0.113.9"), Helo: "mail.attacker.example", MailFrom: "x@attacker.example"}
	for _, from := range []string{
		"From: x@attacker.example, CEO <ceo@sender.example>\r\n",
		"From: x@attacker.example\r\nFrom: CEO <ceo@sender.example>\r\n",
	} {
		v := newFilter(newDNS(), ModeEnforce).Check(context.Background(), attacker, headerMessage(from))
		if v.Action != Reject || !hasTest(v, "DMARC_REJECT") || !strings.Contains(v.Reason, "sender.example") {
			t.Errorf("%q: got %s %v reason=%q, want DMARC reject", from, v.Action, v.Tests, v.Reason)
		}
	}
}

// Several passing domains of one sender earn one DMARC_PASS, not one each.
func TestDMARCPassCountsOnce(t *testing.T) {
	dns := newDNS()
	raw := sign(t, dns, "sender.example", headerMessage("From: a@sender.example, b@bounce.sender.example\r\n"))
	v := newFilter(dns, ModeEnforce).Check(context.Background(), env("a@sender.example"), raw)
	n := 0
	for _, test := range v.Tests {
		if test == "DMARC_PASS" {
			n++
		}
	}
	if n != 1 || v.Action != Inbox {
		t.Fatalf("got %s %v, want inbox with one DMARC_PASS", v.Action, v.Tests)
	}
}

// An absolute domain names the same account, so its trailing dot cannot
// hide one from the account check.
func TestForgedAccountBehindAmbiguity(t *testing.T) {
	dns := newDNS()
	dns.txt["local.test"] = []string{"v=spf1 ip4:" + senderIP + " -all"}
	cases := map[string]struct {
		env Envelope
		raw []byte
	}{
		"envelope trailing dot": {env("ali@local.test."), message("noreply@local.test")},
		"header trailing dot":   {env("noreply@local.test"), headerMessage("From: <ali@local.test.>\r\n")},
		"second address":        {env("noreply@local.test"), headerMessage("From: noreply@local.test, Ali <ali@local.test>\r\n")},
		"second field":          {env("noreply@local.test"), headerMessage("From: noreply@local.test\r\nFrom: Ali <ali@local.test>\r\n")},
	}
	for name, tc := range cases {
		v := withAccounts(newFilter(dns, ModeEnforce), "ali@local.test").Check(context.Background(), tc.env, tc.raw)
		if v.Action != Reject || !hasTest(v, "LOCAL_ACCOUNT_FORGED") {
			t.Errorf("%s: got %s %v, want reject with LOCAL_ACCOUNT_FORGED", name, v.Action, v.Tests)
		}
	}
}

func TestTooManyAuthorsRejected(t *testing.T) {
	var addrs []string
	for i := range maxAuthors + 1 {
		addrs = append(addrs, fmt.Sprintf("a%d@sender.example", i))
	}
	dns := newDNS()
	raw := sign(t, dns, "sender.example", headerMessage("From: "+strings.Join(addrs, ", ")+"\r\n"))
	v := newFilter(dns, ModeEnforce).Check(context.Background(), env("a0@sender.example"), raw)
	if v.Action != Reject || !hasTest(v, "FROM_TOO_MANY") {
		t.Fatalf("got %s %v, want reject with FROM_TOO_MANY", v.Action, v.Tests)
	}
}
