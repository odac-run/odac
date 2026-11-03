package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"odac/internal/apiproto"
)

// mtlsCommand is `odac domain mtls`. It talks to the server over the local API
// like every other command, so it works whether or not the server has a hub.
func mtlsCommand() *command {
	return &command{
		sub: []entry{
			{"set", &command{
				description: "Set a host's client certificate mode: --mode required, optional or off",
				args:        []string{"-d", "--domain", "-m", "--mode", "--ca", "--ca-file"},
				action:      (*app).mtlsSet,
			}},
			{"off", &command{
				description: "Stop asking for client certificates on a host",
				args:        []string{"-d", "--domain"},
				action: func(a *app, args []string) int {
					return a.callMessage("domain.mtls.set", []any{mtlsDomainArg(a, args), "off"})
				},
			}},
			{"issue", &command{
				description: "Issue a client certificate that opens only --hosts. Writes a password-protected .p12, or a .crt with --csr",
				args:        []string{"-d", "--domain", "--name", "--hosts", "--csr", "--out", "--days", "--password", "--renews"},
				action:      (*app).mtlsIssue,
			}},
			{"scope", &command{
				description: "Change which hosts a client certificate opens, effective on the next request",
				args:        []string{"--fingerprint", "--hosts"},
				action: func(a *app, args []string) int {
					fingerprint := parseArg(args, "--fingerprint")
					if fingerprint == "" {
						fingerprint = a.question(__("Enter the certificate fingerprint: "))
					}
					hosts := splitHosts(parseArg(args, "--hosts"))
					return a.call("domain.mtls.cert.scope", []any{fingerprint, hosts}, false)
				},
			}},
			{"revoke", &command{
				description: "Revoke a client certificate, cutting its open connections",
				args:        []string{"--fingerprint"},
				action: func(a *app, args []string) int {
					fingerprint := parseArg(args, "--fingerprint")
					if fingerprint == "" {
						fingerprint = a.question(__("Enter the certificate fingerprint: "))
					}
					return a.call("domain.mtls.cert.revoke", []any{fingerprint}, false)
				},
			}},
			{"list", &command{
				description: "List client certificates",
				args:        []string{"-d", "--domain"},
				action: func(a *app, args []string) int {
					data := []any{}
					if domain := parseArg(args, "-d", "--domain"); domain != "" {
						data = []any{domain}
					}
					return a.call("domain.mtls.cert.list", data, true)
				},
			}},
		},
	}
}

func mtlsDomainArg(a *app, args []string) string {
	domain := parseArg(args, "-d", "--domain")
	if domain == "" {
		domain = a.question(__("Enter the domain name: "))
	}
	return domain
}

func splitHosts(raw string) []any {
	out := []any{}
	for _, host := range strings.Split(raw, ",") {
		if host = strings.TrimSpace(host); host != "" {
			out = append(out, host)
		}
	}
	return out
}

func (a *app) mtlsSet(args []string) int {
	domain := mtlsDomainArg(a, args)
	mode := parseArg(args, "-m", "--mode")
	if mode == "" {
		mode = a.question(__("Enter the mode (required, optional, off): "))
	}
	opts := map[string]any{}
	if ca := parseArg(args, "--ca"); ca != "" {
		opts["ca"] = splitHosts(ca)
	}
	if file := parseArg(args, "--ca-file"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintln(a.errOut, __("Failed to read %s: %s", file, err.Error()))
			return 1
		}
		opts["caPem"] = string(raw)
	}
	return a.callMessage("domain.mtls.set", []any{domain, mode, opts})
}

// mtlsIssue signs a certificate. With --csr the private key was made elsewhere
// and never touches this machine, the same guarantee the dashboard gives. Without
// it the key is made here, in this process, and only ever written into the .p12.
func (a *app) mtlsIssue(args []string) int {
	domain := mtlsDomainArg(a, args)
	name := parseArg(args, "--name")
	hosts := splitHosts(parseArg(args, "--hosts"))
	renews := parseArg(args, "--renews")
	if renews == "" {
		if name == "" {
			name = a.question(__("Enter a name for the certificate: "))
		}
		if len(hosts) == 0 {
			hosts = splitHosts(a.question(__("Enter the hosts it opens, comma separated: ")))
		}
	}

	opts := map[string]any{"name": name, "hosts": hosts}
	if renews != "" {
		opts["renews"] = renews
	}
	if raw := parseArg(args, "--days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Fprintln(a.errOut, __("Lifetime must be between 1 and 3650 days."))
			return 1
		}
		opts["days"] = days
	}

	out := parseArg(args, "--out")
	var key *ecdsa.PrivateKey
	if csrFile := parseArg(args, "--csr"); csrFile != "" {
		raw, err := os.ReadFile(csrFile)
		if err != nil {
			fmt.Fprintln(a.errOut, __("Failed to read %s: %s", csrFile, err.Error()))
			return 1
		}
		opts["csr"] = string(raw)
		if out == "" {
			out = strings.TrimSuffix(filepath.Base(csrFile), filepath.Ext(csrFile)) + ".crt"
		}
	} else {
		if out == "" {
			out = certFileName(name) + ".p12"
		}
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			fmt.Fprintln(a.errOut, err)
			return 1
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
		if err != nil {
			fmt.Fprintln(a.errOut, err)
			return 1
		}
		opts["csr"] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	}

	// All refused before signing, so a mistake here does not leave an active
	// certificate behind whose key was never saved.
	if _, err := os.Stat(out); err == nil {
		fmt.Fprintln(a.errOut, __("%s already exists.", out))
		return 1
	}
	password := parseArg(args, "--password")
	if key != nil && password == "" {
		password = a.secret(__("Enter a password for the .p12 file: "))
		if password == "" {
			fmt.Fprintln(a.errOut, __("A password is required, since the file holds the private key."))
			return 1
		}
	}

	if !a.check() {
		fmt.Fprintln(a.errOut, "Odac server is not running.")
		return 1
	}
	auth, _ := a.cfg.Map("api")["auth"].(string)
	resp, err := a.client.Call(apiproto.Request{Auth: auth, Action: "domain.mtls.cert.issue", Data: []any{domain, opts}}, nil)
	if err != nil {
		fmt.Fprintln(a.errOut, "Socket error:", err)
		return 1
	}
	if !resp.Result {
		(&renderer{out: a.out, errOut: a.errOut}).final(resp)
		return 1
	}

	var issued struct {
		Cert        string `json:"cert"`
		Chain       string `json:"chain"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(resp.Data, &issued); err != nil {
		fmt.Fprintln(a.errOut, err)
		return 1
	}

	var contents []byte
	if key == nil {
		contents = []byte(issued.Cert + issued.Chain)
	} else {
		contents, err = encodeP12(key, issued.Cert, issued.Chain, password)
		if err != nil {
			fmt.Fprintln(a.errOut, err)
			return 1
		}
	}
	if err := os.WriteFile(out, contents, 0o600); err != nil {
		fmt.Fprintln(a.errOut, __("Failed to write %s: %s", out, err.Error()))
		return 1
	}

	fmt.Fprintln(a.out, resp.Message)
	fmt.Fprintln(a.out, __("Saved to %s", out))
	fmt.Fprintln(a.out, __("Fingerprint: %s", issued.Fingerprint))
	if warning := ephemeralWarning(out); warning != "" {
		fmt.Fprintln(a.errOut, warning)
	}
	return 0
}

// secret reads a line without echoing it when stdin is a terminal.
func (a *app) secret(prompt string) string {
	f, ok := a.in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return a.question(prompt)
	}
	fmt.Fprint(a.out, prompt)
	line, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(a.out)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(line))
}

func certFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "client"
	}
	return b.String()
}

func encodeP12(key *ecdsa.PrivateKey, certPEM, chainPEM, password string) ([]byte, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("server returned no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	var chain []*x509.Certificate
	if caBlock, _ := pem.Decode([]byte(chainPEM)); caBlock != nil {
		ca, err := x509.ParseCertificate(caBlock.Bytes)
		if err != nil {
			return nil, err
		}
		chain = append(chain, ca)
	}
	return pkcs12.Modern.Encode(key, cert, chain, password)
}

// ephemeralWarning catches the file landing in a container's writable layer,
// where the next update discards it. ODAC usually runs in Docker, and
// `docker exec odac odac ...` starts in a directory that is not a bind mount.
func ephemeralWarning(path string) string {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	if onOverlay(string(raw), abs) {
		return __("Warning: %s is inside the container's writable layer and will be lost on update. Copy it out now, or use --out with a mounted path.", abs)
	}
	return ""
}

// onOverlay reports whether the longest mount holding path is the overlay root.
func onOverlay(mountinfo, path string) bool {
	best, overlay := -1, false
	for _, line := range strings.Split(mountinfo, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(left)
		kind := strings.Fields(right)
		if len(fields) < 5 || len(kind) == 0 {
			continue
		}
		point := fields[4]
		if path != point && !strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/") {
			continue
		}
		if len(point) > best {
			best, overlay = len(point), kind[0] == "overlay"
		}
	}
	return overlay
}
