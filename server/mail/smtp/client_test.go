package smtp

import (
	"testing"

	"odac-mail/config"
)

// TestGetLocalAddress_PTRMatchedIPv6 verifies that IPv6 with matching PTR
// gets highest priority when target supports IPv6.
func TestGetLocalAddress_PTRMatchedIPv6(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "1.2.3.4", PTR: "mail.example.com", Public: true},
		},
		IPv6: []config.IPEntry{
			{Address: "2001:db8::1", PTR: "mail.example.com", Public: true},
		},
	}

	result := c.getLocalAddress("mail.example.com", true, ips)

	if result.Address != "2001:db8::1" {
		t.Errorf("expected IPv6 address, got %s", result.Address)
	}
	if result.EHLO != "mail.example.com" {
		t.Errorf("expected EHLO mail.example.com, got %s", result.EHLO)
	}
}

// TestGetLocalAddress_PTRMatchedIPv4WhenNoIPv6 verifies IPv4 PTR match
// is used when target doesn't support IPv6.
func TestGetLocalAddress_PTRMatchedIPv4WhenNoIPv6(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "1.2.3.4", PTR: "mail.example.com", Public: true},
		},
		IPv6: []config.IPEntry{
			{Address: "2001:db8::1", PTR: "mail.example.com", Public: true},
		},
	}

	result := c.getLocalAddress("mail.example.com", false, ips)

	if result.Address != "1.2.3.4" {
		t.Errorf("expected IPv4 address when target is IPv4-only, got %s", result.Address)
	}
}

// TestGetLocalAddress_RootDomainPTRMatch verifies that PTR matching works
// with root domain suffix (mail.example.com matches PTR *.example.com).
func TestGetLocalAddress_RootDomainPTRMatch(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv6: []config.IPEntry{
			{Address: "2001:db8::1", PTR: "server1.example.com", Public: true},
		},
	}

	// mail.example.com should match PTR server1.example.com via rootDomain
	result := c.getLocalAddress("mail.example.com", true, ips)

	if result.Address != "2001:db8::1" {
		t.Errorf("expected PTR-matched IPv6 via root domain, got %s", result.Address)
	}
	if result.EHLO != "server1.example.com" {
		t.Errorf("expected EHLO from PTR, got %s", result.EHLO)
	}
}

// TestGetLocalAddress_FallbackToPublicIPv6 verifies fallback to first public
// IPv6 when no PTR match exists.
func TestGetLocalAddress_FallbackToPublicIPv6(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "1.2.3.4", PTR: "", Public: true},
		},
		IPv6: []config.IPEntry{
			{Address: "2001:db8::99", PTR: "", Public: true},
		},
	}

	result := c.getLocalAddress("mail.other.com", true, ips)

	if result.Address != "2001:db8::99" {
		t.Errorf("expected fallback to public IPv6, got %s", result.Address)
	}
}

// TestGetLocalAddress_FallbackToPublicIPv4 verifies fallback to first public
// IPv4 when no IPv6 and no PTR match.
func TestGetLocalAddress_FallbackToPublicIPv4(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "5.6.7.8", PTR: "", Public: true},
		},
	}

	result := c.getLocalAddress("mail.other.com", false, ips)

	if result.Address != "5.6.7.8" {
		t.Errorf("expected fallback to public IPv4, got %s", result.Address)
	}
}

// TestGetLocalAddress_FallbackToPrimary verifies fallback to primary IP
// when no public IPs are available.
func TestGetLocalAddress_FallbackToPrimary(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		Primary: "10.0.0.1",
	}

	result := c.getLocalAddress("mail.example.com", true, ips)

	if result.Address != "10.0.0.1" {
		t.Errorf("expected fallback to primary IP, got %s", result.Address)
	}
}

// TestGetLocalAddress_SkipsPrivateIPs verifies that private IPs are not selected.
func TestGetLocalAddress_SkipsPrivateIPs(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "192.168.1.1", PTR: "mail.example.com", Public: false},
			{Address: "1.2.3.4", PTR: "", Public: true},
		},
	}

	result := c.getLocalAddress("mail.example.com", false, ips)

	// Should skip private IP even though PTR matches, and use public IP
	if result.Address != "1.2.3.4" {
		t.Errorf("expected public IP (skipping private), got %s", result.Address)
	}
}

// TestGetLocalAddress_EmptyIPs verifies graceful handling of empty IP config.
func TestGetLocalAddress_EmptyIPs(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{}

	result := c.getLocalAddress("mail.example.com", true, ips)

	if result.Address != "" {
		t.Errorf("expected empty address for empty IPs, got %s", result.Address)
	}
	if result.EHLO != "mail.example.com" {
		t.Errorf("expected domain as EHLO fallback, got %s", result.EHLO)
	}
}

// TestGetLocalAddress_IPv6PriorityOverIPv4PTR verifies that IPv6 PTR match
// takes priority over IPv4 PTR match when target supports IPv6.
func TestGetLocalAddress_IPv6PriorityOverIPv4PTR(t *testing.T) {
	c := &Client{}
	ips := config.IPConfig{
		IPv4: []config.IPEntry{
			{Address: "1.2.3.4", PTR: "mail.example.com", Public: true},
		},
		IPv6: []config.IPEntry{
			{Address: "2001:db8::1", PTR: "mail.example.com", Public: true},
		},
	}

	result := c.getLocalAddress("mail.example.com", true, ips)

	// IPv6 should win over IPv4 even though both have matching PTR
	if result.Address != "2001:db8::1" {
		t.Errorf("expected IPv6 priority over IPv4, got %s", result.Address)
	}
}

func TestPtrMatchesDomain(t *testing.T) {
	tests := []struct {
		ptr, domain, root string
		want              bool
	}{
		{"mail.example.com", "mail.example.com", "example.com", true},
		{"server1.example.com", "mail.example.com", "example.com", true},
		{"mail.other.com", "mail.example.com", "example.com", false},
		{"example.com", "sub.example.com", "example.com", true},
	}

	for _, tt := range tests {
		got := ptrMatchesDomain(tt.ptr, tt.domain, tt.root)
		if got != tt.want {
			t.Errorf("ptrMatchesDomain(%q, %q, %q) = %v, want %v",
				tt.ptr, tt.domain, tt.root, got, tt.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	if s := sanitize("hello\r\nworld"); s != "helloworld" {
		t.Errorf("expected CR/LF stripped, got %q", s)
	}

	long := ""
	for i := 0; i < 2000; i++ {
		long += "a"
	}
	if len(sanitize(long)) != 1000 {
		t.Errorf("expected truncation to 1000 chars")
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"192.168.1.1:12345", "192.168.1.1"},
		{"[::1]:12345", "::1"},
		{"10.0.0.1", "10.0.0.1"},
	}

	for _, tt := range tests {
		got := extractIP(tt.input)
		if got != tt.want {
			t.Errorf("extractIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsValidEmail(t *testing.T) {
	valid := []string{"user@example.com", "a@b.co", "test+tag@domain.org"}
	invalid := []string{"", "@", "user@", "@domain", "user@d", "nope"}

	for _, e := range valid {
		if !isValidEmail(e) {
			t.Errorf("expected %q to be valid", e)
		}
	}
	for _, e := range invalid {
		if isValidEmail(e) {
			t.Errorf("expected %q to be invalid", e)
		}
	}
}

func TestEncodeDataBody_DotStuffing(t *testing.T) {
	in := []byte("Subject: x\r\n\r\n<style>\r\n.foo { color: red; }\r\n.\r\nbar\r\n</style>\r\n")
	got := string(encodeDataBody(in))
	want := "Subject: x\r\n\r\n<style>\r\n..foo { color: red; }\r\n..\r\nbar\r\n</style>\r\n"
	if got != want {
		t.Errorf("dot-stuffing failed\n got: %q\nwant: %q", got, want)
	}
}

func TestEncodeDataBody_BareLFNormalized(t *testing.T) {
	in := []byte("Subject: x\n\nhello\n.world\n")
	got := string(encodeDataBody(in))
	want := "Subject: x\r\n\r\nhello\r\n..world\r\n"
	if got != want {
		t.Errorf("LF→CRLF + dot-stuffing failed\n got: %q\nwant: %q", got, want)
	}
}

func TestEncodeDataBody_TrailingCRLFGuaranteed(t *testing.T) {
	got := string(encodeDataBody([]byte("no-trailing-newline")))
	if got != "no-trailing-newline\r\n" {
		t.Errorf("trailing CRLF not added, got %q", got)
	}
	got = string(encodeDataBody([]byte("ends-with-crlf\r\n")))
	if got != "ends-with-crlf\r\n" {
		t.Errorf("should not duplicate trailing CRLF, got %q", got)
	}
}

func TestEncodeDataBody_DotMidLineUnchanged(t *testing.T) {
	in := []byte("This is a sentence. With a period.\r\nAnother line.\r\n")
	got := string(encodeDataBody(in))
	if got != string(in) {
		t.Errorf("mid-line dots should not be stuffed\n got: %q\nwant: %q", got, string(in))
	}
}
