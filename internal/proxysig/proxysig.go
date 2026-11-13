// Package proxysig is how odac-proxy vouches to an app for the connection a
// request arrived on. Anything else that reaches the app's TCP port (a published
// port, a neighbour on a shared network) can send the same headers, so an app
// listening on TCP should believe them only through the signature.
package proxysig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Headers the proxy sets on every request it forwards. A client sending any of
// them has them removed first.
const (
	HeaderRemoteAddress = "X-Odac-Connection-RemoteAddress"
	HeaderSSL           = "X-Odac-Connection-Ssl"
	HeaderSignature     = "X-Odac-Connection-Signature"
)

// Headers lists every connection header, for stripping.
var Headers = []string{HeaderRemoteAddress, HeaderSSL, HeaderSignature}

// KeyEnv names the variable holding the key an app verifies HeaderSignature
// with. It is the value mtls.KeyEnv carries too, so one key checks both
// signatures; their signed strings start differently, so neither stands in for
// the other.
const KeyEnv = "ODAC_PROXY_KEY"

// Connection is what the proxy vouches for on one request. Method, host and URI
// are signed so a captured signature proves nothing on another route.
type Connection struct {
	Host          string
	Method        string
	RemoteAddress string
	SSL           bool
	Timestamp     int64
	URI           string
}

// Sign returns the HeaderSignature value. The signed string is part of the
// contract: app-side verifiers rebuild it exactly.
func (c Connection) Sign(key string) string {
	ssl := ""
	if c.SSL {
		ssl = "true"
	}
	ts := strconv.FormatInt(c.Timestamp, 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(strings.Join([]string{
		"odac-connection-v1", ts, c.Method, c.Host, c.URI, c.RemoteAddress, ssl,
	}, "\n")))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
