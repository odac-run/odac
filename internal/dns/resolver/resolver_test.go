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
