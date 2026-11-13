package proxysig

import (
	"testing"

	"odac/internal/mtls"
)

// The vector is what the framework and the docs' examples are checked against,
// so a change here is a change to the wire contract.
func TestSignMatchesPublishedVector(t *testing.T) {
	c := Connection{
		Host: "example.com", Method: "GET", RemoteAddress: "203.0.113.7",
		SSL: true, Timestamp: 1700000000, URI: "/a?b=1",
	}
	const want = "t=1700000000,v1=d9a16b4e0be33e772aac465b062640fb9fa3163a9e7bf997c7b1323579b979c8"
	if got := c.Sign("k"); got != want {
		t.Fatalf("Sign = %s", got)
	}
}

func TestSignBindsEveryField(t *testing.T) {
	base := Connection{Host: "a.test", Method: "GET", RemoteAddress: "1.2.3.4", SSL: true, Timestamp: 1, URI: "/"}
	sig := base.Sign("k")
	for name, c := range map[string]Connection{
		"host":   {Host: "b.test", Method: "GET", RemoteAddress: "1.2.3.4", SSL: true, Timestamp: 1, URI: "/"},
		"method": {Host: "a.test", Method: "POST", RemoteAddress: "1.2.3.4", SSL: true, Timestamp: 1, URI: "/"},
		"remote": {Host: "a.test", Method: "GET", RemoteAddress: "1.2.3.5", SSL: true, Timestamp: 1, URI: "/"},
		"ssl":    {Host: "a.test", Method: "GET", RemoteAddress: "1.2.3.4", Timestamp: 1, URI: "/"},
		"time":   {Host: "a.test", Method: "GET", RemoteAddress: "1.2.3.4", SSL: true, Timestamp: 2, URI: "/"},
		"uri":    {Host: "a.test", Method: "GET", RemoteAddress: "1.2.3.4", SSL: true, Timestamp: 1, URI: "/x"},
	} {
		if c.Sign("k") == sig {
			t.Errorf("%s is not signed", name)
		}
	}
	if base.Sign("other") == sig {
		t.Error("key is not used")
	}
}

func TestSignNeverEqualsAClientCertSignature(t *testing.T) {
	a := mtls.Assertion{Method: "GET", Host: "a.test", URI: "/", Timestamp: 1}
	c := Connection{Method: "GET", Host: "a.test", URI: "/", Timestamp: 1}
	if a.Sign("k") == c.Sign("k") {
		t.Fatal("one key signs two contracts the same way")
	}
}
