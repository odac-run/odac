package domains

import (
	"strings"
	"sync"
	"testing"
)

func TestValidateAcceptsHostnames(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":                    "example.com",
		"  Example.COM ":                 "example.com",
		"https://example.com":            "example.com",
		"a-b.c-d.example.co.uk":          "a-b.c-d.example.co.uk",
		"*.example.com":                  "*.example.com",
		"localhost":                      "localhost",
		"192.168.1.10":                   "192.168.1.10",
		"münchen.de":                     "xn--mnchen-3ya.de",
		"*.münchen.de":                   "*.xn--mnchen-3ya.de",
		"xn--mnchen-3ya.de":              "xn--mnchen-3ya.de",
		"1password.com":                  "1password.com",
		strings.Repeat("a", 63) + ".com": strings.Repeat("a", 63) + ".com",
	} {
		got, errMsg := validate(in)
		if errMsg != "" || got != want {
			t.Errorf("validate(%q) = %q, %q; want %q", in, got, errMsg, want)
		}
	}
}

// TestValidateRefusesWhatIsNotAHostname covers the characters the Node-era
// check let through into log lines, DNS names, TXT content and file names.
func TestValidateRefusesWhatIsNotAHostname(t *testing.T) {
	for _, in := range []string{
		"exa mple.com",
		"example.com\nlog-injection",
		"exam\x00ple.com",
		`"example".com`,
		"exa'mple.com",
		"exam;ple.com",
		"_dmarc.example.com",
		"-example.com",
		"example-.com",
		"example.com.",
		".example.com",
		"ex*mple.com",
		"a.*.example.com",
		"*.com",
		"999.1.1.1",
		"1.2.3",
		"::1",
		"2001:db8::1",
		"example.com:8080",
		strings.Repeat("a", 64) + ".com",
		strings.Repeat("abcdefghi.", 26) + "com",
	} {
		if got, errMsg := validate(in); errMsg == "" {
			t.Errorf("validate(%q) accepted as %q", in, got)
		}
	}
}

// TestConcurrentAddsRegisterOnce pins the check-then-insert gap: every add
// of one name passed the View check, then the later insert overwrote the
// earlier one and each fan-out duplicated the multi-value records.
func TestConcurrentAddsRegisterOnce(t *testing.T) {
	fx := newFixture(t)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if fx.d.Add("example.com", "myapp").Status {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d adds succeeded, want 1", won)
	}
	txt := 0
	for _, rec := range fx.dns.recorded() {
		if rec["type"] == "TXT" {
			txt++
		}
	}
	if txt != 2 {
		t.Fatalf("TXT records = %d, want 2 (one SPF, one DMARC)", txt)
	}
}

// TestDeleteRemovesALegacyName keeps records added under the old check
// removable after the grammar tightened.
func TestDeleteRemovesALegacyName(t *testing.T) {
	fx := newFixture(t)
	fx.setDomains(map[string]any{
		"my_site.example.com": map[string]any{"appId": "myapp"},
		"example.org":         map[string]any{"appId": "myapp", "subdomain": []any{"www", "old_sub"}},
	})
	if r := fx.d.Delete("my_site.example.com", true); !r.Status {
		t.Fatalf("legacy domain not deleted: %v", r.Message)
	}
	if fx.domain("my_site.example.com") != nil {
		t.Fatal("legacy domain still registered")
	}
	if r := fx.d.Delete("old_sub.example.org", true); !r.Status {
		t.Fatalf("legacy subdomain not deleted: %v", r.Message)
	}
	if subs, _ := fx.domain("example.org")["subdomain"].([]any); len(subs) != 1 {
		t.Fatalf("subdomains = %v, want [www]", subs)
	}
	if r := fx.d.Delete("other_site.example.com", true); r.Status {
		t.Fatal("an unknown invalid name was accepted")
	}
}
