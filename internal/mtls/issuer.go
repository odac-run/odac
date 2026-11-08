package mtls

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"regexp"
	"time"
)

// Issuer modes of a record. With IssuerPerHost a certificate that opens one host is
// signed by an issuer of that host alone, so the handshake on that host names only
// it and a browser offers only that host's certificates. IssuerRecord is the
// default and is never stored: the dashboard diffs records by serialization.
const (
	IssuerPerHost = "per-host"
	IssuerRecord  = "record"
)

// maxChainIssuers bounds how many presented certificates are tried as issuers,
// since each one that matches the name costs two signature checks.
const maxChainIssuers = 8

// maxIssuerHost is the X.520 upper bound of a common name, which is where the
// host goes. A longer host is signed by the record's CA instead.
const maxIssuerHost = 64

var issuerHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// PerHostIssuable reports whether host can have an issuer of its own: a plain DNS
// name that fits a common name. A wildcard scope covers many hosts, so a
// certificate for one is signed by the record's CA like any multi-host one.
func PerHostIssuable(host string) bool {
	return len(host) <= maxIssuerHost && issuerHostRe.MatchString(host)
}

// IssuerName is the subject of the issuer a record's CA keeps for host. Every
// server holding the CA derives the same name with no coordination, and that is
// all a browser filters on; each server signs with a key of its own, which never
// travels. The organization is the CA's common name, or its id when it has none.
func IssuerName(host string, ca *x509.Certificate) pkix.Name {
	org := ca.Subject.CommonName
	if org == "" {
		org = "Client CA " + Fingerprint(ca.Raw)[:16]
	}
	return pkix.Name{CommonName: host, Organization: []string{org}}
}

// IssuerSubject is IssuerName in DER, the exact bytes written into the issuer and
// compared on every request. Comparing bytes rather than parsed names is what a
// browser's filter does too.
func IssuerSubject(host string, ca *x509.Certificate) []byte {
	der, err := asn1.Marshal(IssuerName(host, ca).ToRDNSequence())
	if err != nil {
		return nil
	}
	return der
}

// VerifyPerHost checks a presented chain the way a per-host record trusts it.
// x509.Verify cannot do this: with the CA in its pool the handshake would name the
// CA and every certificate would be offered again, and without it the issuer of
// another server, which has the same name and a different key, would not verify.
//
// The leaf is accepted when one of cas signed it directly, or when an issuer
// signed it that carries IssuerSubject(host) exactly, is a CA limited to signing
// leaves (pathlen 0), may sign client certificates, and was itself signed by that
// CA. An issuer of another host is refused however it chains. Issuers come from
// the client's chain and then from extra, this server's own. The CA is the trust
// anchor and its own path length is deliberately not applied: a managed CA is
// often made with pathlen 0, and per RFC 5280 an anchor's constraints are
// optional input. Every certificate on the path must be valid at now.
//
// It returns the CA that anchors the path and the end of the path's validity, or
// a nil CA with VerifyExpired or VerifyUntrusted.
func VerifyPerHost(chain, extra, cas []*x509.Certificate, host string, now time.Time) (*x509.Certificate, time.Time, string) {
	if len(chain) == 0 {
		return nil, time.Time{}, VerifyUntrusted
	}
	leaf := chain[0]
	if (leaf.BasicConstraintsValid && leaf.IsCA) || len(leaf.UnhandledCriticalExtensions) > 0 || !allowsClientAuth(leaf.ExtKeyUsage, false) {
		return nil, time.Time{}, VerifyUntrusted
	}
	presented := chain[1:]
	if len(presented) > maxChainIssuers {
		presented = presented[:maxChainIssuers]
	}
	candidates := make([]*x509.Certificate, 0, len(presented)+len(extra))
	candidates = append(candidates, presented...)
	candidates = append(candidates, extra...)

	for _, ca := range cas {
		if bytes.Equal(leaf.RawIssuer, ca.RawSubject) && leaf.CheckSignatureFrom(ca) == nil {
			return finishPath(now, leaf, ca)
		}
		want := IssuerSubject(host, ca)
		if want == nil || !bytes.Equal(leaf.RawIssuer, want) {
			continue
		}
		for _, issuer := range candidates {
			if !bytes.Equal(issuer.RawSubject, want) || !bytes.Equal(issuer.RawIssuer, ca.RawSubject) || !validHostIssuer(issuer) {
				continue
			}
			if issuer.CheckSignatureFrom(ca) != nil || leaf.CheckSignatureFrom(issuer) != nil {
				continue
			}
			return finishPath(now, leaf, issuer, ca)
		}
	}
	return nil, time.Time{}, VerifyUntrusted
}

func validHostIssuer(cert *x509.Certificate) bool {
	return cert.BasicConstraintsValid && cert.IsCA && cert.MaxPathLen == 0 && cert.MaxPathLenZero &&
		cert.KeyUsage&x509.KeyUsageCertSign != 0 && len(cert.UnhandledCriticalExtensions) == 0 &&
		allowsClientAuth(cert.ExtKeyUsage, true)
}

// allowsClientAuth reads an extended key usage list. A leaf must name client
// authentication; an issuer may also leave the list out, which restricts nothing.
func allowsClientAuth(usages []x509.ExtKeyUsage, emptyAllows bool) bool {
	if len(usages) == 0 {
		return emptyAllows
	}
	for _, usage := range usages {
		if usage == x509.ExtKeyUsageClientAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func finishPath(now time.Time, path ...*x509.Certificate) (*x509.Certificate, time.Time, string) {
	until := path[0].NotAfter
	for _, cert := range path {
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return nil, time.Time{}, VerifyExpired
		}
		if cert.NotAfter.Before(until) {
			until = cert.NotAfter
		}
	}
	return path[len(path)-1], until, VerifySuccess
}
