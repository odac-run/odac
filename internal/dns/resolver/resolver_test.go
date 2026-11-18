package resolver

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"odac/internal/dns/config"
)

// recorder is a dns.ResponseWriter that keeps the packed reply, so tests
// see exactly what would go on the wire.
type recorder struct {
	remote net.Addr
	wire   []byte
}

func (r *recorder) LocalAddr() net.Addr  { return r.remote }
func (r *recorder) RemoteAddr() net.Addr { return r.remote }
func (r *recorder) WriteMsg(m *dns.Msg) error {
	b, err := m.Pack()
	r.wire = b
	return err
}
func (r *recorder) Write(b []byte) (int, error) { r.wire = b; return len(b), nil }
func (r *recorder) Close() error                { return nil }
func (r *recorder) TsigStatus() error           { return nil }
func (r *recorder) TsigTimersOnly(bool)         {}
func (r *recorder) Hijack()                     {}

var (
	udpPeer = &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 5353}
	tcpPeer = &net.TCPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 5353}
)

func newTestResolver(t *testing.T, txtRecords int) *Resolver {
	t.Helper()
	records := []config.Record{{Name: "example.com", Type: "A", Value: "203.0.113.1", TTL: 300}}
	for i := 0; i < txtRecords; i++ {
		records = append(records, config.Record{
			Name: "example.com", Type: "TXT", TTL: 300,
			Value: "v=verify-" + strconv.Itoa(i) + "-" + strings.Repeat("x", 90),
		})
	}
	r := NewResolver()
	r.UpdateConfig(config.Config{Zones: map[string]config.Zone{
		"example.com": {
			Records: records,
			SOA:     config.SOARecord{Primary: "ns1.example.com", Email: "hostmaster.example.com", Serial: 1},
		},
	}})
	return r
}

func ask(t *testing.T, r *Resolver, peer net.Addr, name string, qtype uint16, edns uint16) (*dns.Msg, int) {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)
	if edns != 0 {
		req.SetEdns0(edns, false)
	}
	w := &recorder{remote: peer}
	r.ServeDNS(w, req)
	resp := new(dns.Msg)
	if err := resp.Unpack(w.wire); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return resp, len(w.wire)
}

// TestForeignZoneIsRefused: an authoritative-only server must not claim
// with AA that a name in someone else's zone does not exist.
func TestForeignZoneIsRefused(t *testing.T) {
	r := newTestResolver(t, 0)
	resp, _ := ask(t, r, udpPeer, "www.example.org", dns.TypeA, 0)
	if resp.Rcode != dns.RcodeRefused || resp.Authoritative {
		t.Fatalf("rcode=%s aa=%v, want REFUSED without AA", dns.RcodeToString[resp.Rcode], resp.Authoritative)
	}

	// A missing name inside a hosted zone is still an authoritative NXDOMAIN.
	resp, _ = ask(t, r, udpPeer, "nope.example.com", dns.TypeA, 0)
	if resp.Rcode != dns.RcodeNameError || !resp.Authoritative || len(resp.Ns) != 1 {
		t.Fatalf("in-zone miss: rcode=%s aa=%v ns=%d", dns.RcodeToString[resp.Rcode], resp.Authoritative, len(resp.Ns))
	}
}

// TestLargeAnswerOverPlainUDPIsTruncated: without EDNS a UDP answer may
// carry 512 bytes, and one that does not fit sets TC for a TCP retry.
func TestLargeAnswerOverPlainUDPIsTruncated(t *testing.T) {
	r := newTestResolver(t, 12)

	resp, n := ask(t, r, udpPeer, "example.com", dns.TypeTXT, 0)
	if n > dns.MinMsgSize || !resp.Truncated {
		t.Fatalf("plain UDP: %d bytes, TC=%v; want <= 512 with TC", n, resp.Truncated)
	}
	if resp.IsEdns0() != nil {
		t.Fatal("OPT in a reply to a query without one")
	}

	resp, n = ask(t, r, tcpPeer, "example.com", dns.TypeTXT, 0)
	if resp.Truncated || len(resp.Answer) != 12 {
		t.Fatalf("TCP: %d bytes, TC=%v, %d answers; want all 12", n, resp.Truncated, len(resp.Answer))
	}
}

// TestEDNSBufferIsHonoredAndCapped: an EDNS client gets up to its buffer,
// never more than maxUDPPayload, and an OPT record in the reply.
func TestEDNSBufferIsHonoredAndCapped(t *testing.T) {
	r := newTestResolver(t, 8) // about 1 KB: over 512, under 1232

	resp, n := ask(t, r, udpPeer, "example.com", dns.TypeTXT, 4096)
	if resp.Truncated || len(resp.Answer) != 8 || n <= dns.MinMsgSize {
		t.Fatalf("EDNS 4096: %d bytes, TC=%v, %d answers", n, resp.Truncated, len(resp.Answer))
	}
	opt := resp.IsEdns0()
	if opt == nil || opt.UDPSize() != maxUDPPayload {
		t.Fatalf("reply OPT = %v, want buffer %d", opt, maxUDPPayload)
	}

	big := newTestResolver(t, 30)
	resp, n = ask(t, big, udpPeer, "example.com", dns.TypeTXT, 4096)
	if n > maxUDPPayload || !resp.Truncated {
		t.Fatalf("EDNS 4096, large answer: %d bytes, TC=%v; want <= %d with TC", n, resp.Truncated, maxUDPPayload)
	}

	// A buffer below 512 is treated as 512 (RFC 6891 section 6.2.5).
	resp, n = ask(t, r, udpPeer, "example.com", dns.TypeTXT, 100)
	if n > dns.MinMsgSize || !resp.Truncated {
		t.Fatalf("EDNS 100: %d bytes, TC=%v", n, resp.Truncated)
	}
}

// TestUnknownEDNSVersionIsBadVers covers RFC 6891 section 6.1.3.
func TestUnknownEDNSVersionIsBadVers(t *testing.T) {
	r := newTestResolver(t, 0)
	req := new(dns.Msg)
	req.SetQuestion("example.com.", dns.TypeA)
	req.SetEdns0(1232, false)
	req.IsEdns0().SetVersion(1)
	w := &recorder{remote: udpPeer}
	r.ServeDNS(w, req)
	resp := new(dns.Msg)
	if err := resp.Unpack(w.wire); err != nil {
		t.Fatal(err)
	}
	if resp.Rcode != dns.RcodeBadVers || len(resp.Answer) != 0 {
		t.Fatalf("rcode=%d answers=%d, want BADVERS and no answer", resp.Rcode, len(resp.Answer))
	}
}

func nsTestResolver(t *testing.T, records ...config.Record) *Resolver {
	t.Helper()
	r := NewResolver()
	r.UpdateConfig(config.Config{
		IPs: config.IPConfig{
			Primary: "203.0.113.9",
			IPv4:    []config.IPEntry{{Address: "203.0.113.9", Public: true}},
			IPv6:    []config.IPEntry{{Address: "2001:db8::9", Public: true}},
		},
		Zones: map[string]config.Zone{"example.com": {
			Records: append([]config.Record{{Name: "example.com", Type: "A"}}, records...),
			SOA:     config.SOARecord{Primary: "ns1.example.com", Email: "hostmaster.example.com", Serial: 1},
		}},
	})
	return r
}

func nsTargets(rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		if ns, ok := rr.(*dns.NS); ok {
			out = append(out, ns.Ns)
		}
	}
	return out
}

// TestZoneAnswersForItsDefaultDelegation: a registrar delegates to
// ns1/ns2.<zone> with glue. A resolver that checks the child side must find
// the same NS set and addresses there, or it treats the server as lame.
func TestZoneAnswersForItsDefaultDelegation(t *testing.T) {
	r := nsTestResolver(t)

	resp, _ := ask(t, r, udpPeer, "example.com", dns.TypeNS, 1232)
	got := nsTargets(resp.Answer)
	if !resp.Authoritative || len(resp.Ns) != 0 || strings.Join(got, ",") != "ns1.example.com.,ns2.example.com." {
		t.Fatalf("apex NS: aa=%v answer=%v authority=%d", resp.Authoritative, got, len(resp.Ns))
	}
	glue := map[string]bool{}
	for _, rr := range resp.Extra {
		switch v := rr.(type) {
		case *dns.A:
			glue[v.Hdr.Name+" "+v.A.String()] = true
		case *dns.AAAA:
			glue[v.Hdr.Name+" "+v.AAAA.String()] = true
		}
	}
	for _, want := range []string{"ns1.example.com. 203.0.113.9", "ns2.example.com. 2001:db8::9"} {
		if !glue[want] {
			t.Fatalf("glue %q missing: %v", want, glue)
		}
	}

	for _, host := range []string{"ns1.example.com", "NS2.Example.Com"} {
		resp, _ = ask(t, r, udpPeer, host, dns.TypeA, 0)
		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "203.0.113.9" {
			t.Fatalf("%s A: rcode=%s answer=%v", host, dns.RcodeToString[resp.Rcode], resp.Answer)
		}
		resp, _ = ask(t, r, udpPeer, host, dns.TypeAAAA, 0)
		if len(resp.Answer) != 1 || resp.Answer[0].(*dns.AAAA).AAAA.String() != "2001:db8::9" {
			t.Fatalf("%s AAAA: %v", host, resp.Answer)
		}
	}
}

// TestOperatorNameserversWin: explicit apex NS records replace the default
// set entirely, and an existing address for ns1 is never overwritten.
func TestOperatorNameserversWin(t *testing.T) {
	r := nsTestResolver(t,
		config.Record{Name: "example.com", Type: "NS", Value: "dns1.provider.net"},
		config.Record{Name: "example.com", Type: "NS", Value: "dns2.provider.net"},
	)
	resp, _ := ask(t, r, udpPeer, "example.com", dns.TypeNS, 0)
	if got := strings.Join(nsTargets(resp.Answer), ","); got != "dns1.provider.net.,dns2.provider.net." || len(resp.Extra) != 0 {
		t.Fatalf("explicit NS: answer=%s extra=%d", got, len(resp.Extra))
	}
	resp, _ = ask(t, r, udpPeer, "ns1.example.com", dns.TypeA, 0)
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("ns1 with explicit NS: rcode=%s, want NXDOMAIN", dns.RcodeToString[resp.Rcode])
	}

	r = nsTestResolver(t, config.Record{Name: "ns1.example.com", Type: "A", Value: "198.51.100.53"})
	resp, _ = ask(t, r, udpPeer, "ns1.example.com", dns.TypeA, 0)
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "198.51.100.53" {
		t.Fatalf("operator ns1 A replaced: %v", resp.Answer)
	}
	resp, _ = ask(t, r, udpPeer, "ns1.example.com", dns.TypeAAAA, 0)
	if len(resp.Answer) != 0 {
		t.Fatalf("operator ns1 gained a synthetic AAAA: %v", resp.Answer)
	}
}

// TestChildDelegationIsAReferral: NS records below the apex delegate a child
// zone, so the answer is a referral without AA, never an authoritative one.
func TestChildDelegationIsAReferral(t *testing.T) {
	r := nsTestResolver(t, config.Record{Name: "lab.example.com", Type: "NS", Value: "ns.lab.example.com"})
	resp, _ := ask(t, r, udpPeer, "lab.example.com", dns.TypeNS, 0)
	if resp.Authoritative || len(resp.Answer) != 0 || strings.Join(nsTargets(resp.Ns), ",") != "ns.lab.example.com." {
		t.Fatalf("child NS: aa=%v answer=%d authority=%v", resp.Authoritative, len(resp.Answer), nsTargets(resp.Ns))
	}
}

// TestNoDataCarriesSOA: RFC 2308 says a NODATA answer without the SOA must
// not be cached, so every miss would come back to this server.
func TestNoDataCarriesSOA(t *testing.T) {
	r := nsTestResolver(t)
	resp, _ := ask(t, r, udpPeer, "example.com", dns.TypeMX, 0)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 || len(resp.Ns) != 1 {
		t.Fatalf("NODATA: rcode=%s answer=%d authority=%d", dns.RcodeToString[resp.Rcode], len(resp.Answer), len(resp.Ns))
	}
	if _, ok := resp.Ns[0].(*dns.SOA); !ok {
		t.Fatalf("NODATA authority is %T, want SOA", resp.Ns[0])
	}
}
