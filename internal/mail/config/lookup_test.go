package config

import "testing"

func TestMatchDomain(t *testing.T) {
	cfg := Config{Domains: map[string]Domain{
		"example.com": {MXEnabled: true},
		"other.org":   {},
	}}

	hit := []string{
		"example.com",
		"mail.example.com",
		"a.b.example.com",
		"EXAMPLE.COM",
		"example.com.", // absolute form
		"  example.com ",
	}
	for _, h := range hit {
		if _, ok := cfg.MatchDomain(h); !ok {
			t.Errorf("expected %q to match a configured domain", h)
		}
	}

	miss := []string{"", "com", "notexample.com", "example.com.evil.net", "example.co"}
	for _, h := range miss {
		if _, ok := cfg.MatchDomain(h); ok {
			t.Errorf("expected %q not to match a configured domain", h)
		}
	}
}

func TestMatchDomain_ReturnsEntry(t *testing.T) {
	cfg := Config{Domains: map[string]Domain{"example.com": {MXEnabled: true}}}
	d, ok := cfg.MatchDomain("mail.example.com")
	if !ok || !d.MXEnabled {
		t.Fatalf("walk-up must return the parent entry, got %+v ok=%v", d, ok)
	}
}
