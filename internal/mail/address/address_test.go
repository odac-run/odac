package address

import "testing"

func TestValid(t *testing.T) {
	valid := []string{
		"user@example.com",
		"a@b.co",
		"test+tag@domain.org",
		"User@Example.COM",
		"user@example.com.", // absolute FQDN, legal in an envelope path
	}
	invalid := []string{
		"", "@", "user@", "@domain", "user@d", "nope",
		"a@@b.com",          // two separators
		"a@b@c.com",         // quoted local parts are out of scope
		"user@.example.com", // empty leading label
		"user name@ex.com",  // space
		"user\r\n@ex.com",   // control characters forge log records
		"user\x7f@ex.com",
	}

	for _, e := range valid {
		if !Valid(e) {
			t.Errorf("expected %q to be valid", e)
		}
	}
	for _, e := range invalid {
		if Valid(e) {
			t.Errorf("expected %q to be invalid", e)
		}
	}
}

func TestValid_LengthCeiling(t *testing.T) {
	local := make([]byte, 250)
	for i := range local {
		local[i] = 'a'
	}
	if Valid(string(local) + "@ex.com") {
		t.Error("expected an address past the 254 byte ceiling to be invalid")
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"  Ali@Example.COM ": "ali@example.com",
		"ali@example.com":    "ali@example.com",
		"":                   "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalAndDomain(t *testing.T) {
	cases := []struct {
		addr, local, domain string
	}{
		{"PostMaster@Example.COM", "postmaster", "example.com"},
		{"a.b+c@sub.example.com", "a.b+c", "sub.example.com"},
		{"nope", "", ""},
	}
	for _, c := range cases {
		if got := Local(c.addr); got != c.local {
			t.Errorf("Local(%q) = %q, want %q", c.addr, got, c.local)
		}
		if got := Domain(c.addr); got != c.domain {
			t.Errorf("Domain(%q) = %q, want %q", c.addr, got, c.domain)
		}
	}
}
