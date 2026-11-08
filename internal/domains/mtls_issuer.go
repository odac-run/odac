package domains

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"time"

	"odac/internal/mtls"
)

// A per-host issuer (mtls.IssuerPerHost) is an intermediate the record's CA signs
// for one host. Its name is derived from the host and the CA, so every server
// holding the CA arrives at the same one, while each signs it with a key it made
// itself and never sends anywhere. A browser filters client certificates by the
// issuer names a handshake gives, so naming only the host's issuer is what keeps
// one host's prompt free of every other host's certificates.

// perHostLocked reports whether host belongs to a record whose certificates for a
// single host are signed by a per-host issuer. Caller holds the config lock.
func perHostLocked(domains map[string]any, host string) bool {
	record, ok := locateHost(domains, host)
	return ok && recordConfig(domains, record)["issuer"] == mtls.IssuerPerHost
}

// issuerHostFor returns the host whose issuer signs a certificate for hosts, or ""
// when the record's CA signs it: more than one host, a wildcard, a name that does
// not fit a subject, or a record that is not per-host.
func (m *MTLS) issuerHostFor(hosts []string) string {
	if len(hosts) != 1 || !mtls.PerHostIssuable(hosts[0]) {
		return ""
	}
	perHost := false
	m.cfg.View(func() { perHost = perHostLocked(m.domainsLocked(), hosts[0]) })
	if !perHost {
		return ""
	}
	return hosts[0]
}

// issuerKeyPath names a key file by a hash of CA and host, so no part of a host
// name ever reaches a path.
func (m *MTLS) issuerKeyPath(caID, host string) string {
	sum := sha256.Sum256([]byte(caID + "\x00" + host))
	return filepath.Join(m.certDir, "issuers", hex.EncodeToString(sum[:16])+".key")
}

// ensureIssuerLocked returns this server's issuer for host under the CA, creating
// it on first use: at issue, and at import too, since a handshake can only name an
// issuer this server holds. Its key is written before its row, as a CA's is, so a
// crash leaves at most an unused file. One whose key is gone is made again: the
// certificates it signed still verify through the chain their clients send.
// Caller holds m.mu.
func (m *MTLS) ensureIssuerLocked(store *certStore, caID, host string, caCert *x509.Certificate, caKey any) (*x509.Certificate, crypto.Signer, string, error) {
	if !mtls.AllowsHostIssuer(caCert) {
		// Set refuses this; only a hand-edited config gets here.
		return nil, nil, "", errors.New("the CA has a path length of 0 and cannot sign an issuer")
	}
	stored, err := store.issuer(caID, host)
	if err != nil {
		return nil, nil, "", err
	}
	if stored != "" {
		cert, key, err := m.loadIssuer(caID, host, stored)
		if err == nil {
			return cert, key, stored, nil
		}
		m.log.Error("Client certificate issuer for %s is unusable, making a new one: %s", host, err.Error())
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, "", err
	}
	now := m.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		RawSubject:            mtls.IssuerSubject(host, caCert),
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              caCert.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", err
	}
	if err := writeSecret(m.issuerKeyPath(caID, host), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, nil, "", err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err := store.putIssuer(caID, host, certPEM, now.UnixMilli()); err != nil {
		return nil, nil, "", err
	}
	m.log.Log("Created the client certificate issuer of %s under CA %s", host, caID)
	return cert, key, certPEM, nil
}

func (m *MTLS) loadIssuer(caID, host, certPEM string) (*x509.Certificate, crypto.Signer, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, nil, errors.New("issuer certificate is unreadable")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(m.issuerKeyPath(caID, host))
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(raw)
	if keyBlock == nil {
		return nil, nil, errors.New("issuer key is unreadable")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(crypto.Signer)
	public, matches := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !matches || !public.Equal(cert.PublicKey) {
		return nil, nil, errors.New("issuer key does not match its certificate")
	}
	if !m.now().Before(cert.NotAfter) {
		return nil, nil, errors.New("issuer has expired")
	}
	return cert, key, nil
}

// chainFor is what an issue reply carries as the chain of a stored certificate:
// the issuer that signed it, when one did, then the CA.
func (m *MTLS) chainFor(cert map[string]any, caID string) string {
	issuer, _ := cert["issuerCert"].(string)
	return issuer + m.caPEM(caID)
}

// sweepIssuersLocked removes the issuers nothing needs any more: no live
// certificate was signed through them, or their host no longer belongs to a
// per-host record trusting their CA (a deleted domain, a record that left per-host
// mode). The snapshot already leaves an idle issuer out, so this only stops the
// files from piling up. Caller holds m.mu.
func (m *MTLS) sweepIssuersLocked(store *certStore) {
	idle, err := store.idleIssuers(float64(m.now().UnixMilli()))
	if err == nil {
		var all []mtls.HostIssuer
		all, err = store.issuers()
		m.cfg.View(func() {
			domains := m.domainsLocked()
			for _, issuer := range all {
				record, ok := locateHost(domains, issuer.Host)
				if !ok || !perHostLocked(domains, issuer.Host) || !contains(effectiveCAs(domains, record, issuer.Host), issuer.CA) {
					idle = append(idle, issuer)
				}
			}
		})
	}
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return
	}
	seen := map[string]bool{}
	for _, issuer := range idle {
		key := issuer.CA + "\x00" + issuer.Host
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := store.deleteIssuer(issuer.CA, issuer.Host); err != nil {
			m.log.Error("Client certificate store failed: %s", err.Error())
			return
		}
		if err := os.Remove(m.issuerKeyPath(issuer.CA, issuer.Host)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Error("Failed to remove the client certificate issuer key of %s: %s", issuer.Host, err.Error())
		}
		m.log.Log("Removed the client certificate issuer of %s", issuer.Host)
	}
}
