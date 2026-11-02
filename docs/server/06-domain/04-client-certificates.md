# Client Certificates (mTLS)

Client certificates let a domain check who is connecting before anything reaches your app, or let your app use a certificate as a login. ODAC runs the CA, issues and revokes certificates, and enforces them in the proxy.

### Modes

Each host runs in one of three modes:

| Mode | Without a valid certificate | Use it for |
|---|---|---|
| `required` | The connection is refused | Admin panels and internal tools |
| `optional` | The request reaches your app, marked as unverified | Certificate login, or restricting parts of a public site |
| `off` | Nothing is asked for | The default |

### Quick Start

Issue a certificate first. ODAC refuses to turn on `required` while nobody could get in.

```bash
odac domain mtls issue -d example.com --name alice --hosts auth.example.com --out alice.p12
odac domain mtls set -d auth.example.com --mode required
```

Import `alice.p12` into the browser or operating system, and it opens `auth.example.com` only.

If something goes wrong, this turns the whole domain back off. It works through the local API, not the proxy, so a lockout cannot stop it:

```bash
odac domain mtls off -d example.com
```

### Commands

| Command | Purpose |
|---|---|
| `odac domain mtls set -d <host> --mode <mode>` | Set a host's mode. `--ca <ids>` replaces the trusted CAs, `--ca-file <pem>` adds your own |
| `odac domain mtls off -d <host>` | Turn it off. On a main domain this clears all of its hosts |
| `odac domain mtls issue -d <host> --name <name> --hosts <hosts>` | Issue a certificate. `--out`, `--days`, `--password`, `--csr <file>`, `--renews <fingerprint>` |
| `odac domain mtls scope --fingerprint <fp> --hosts <hosts>` | Change which hosts a certificate opens |
| `odac domain mtls revoke --fingerprint <fp>` | Revoke a certificate |
| `odac domain mtls list [-d <host>]` | List certificates |

Setting a mode on a main domain covers all of its hosts. Setting it on a subdomain overrides that one host, so `example.com` can stay `optional` while `auth.example.com` is `required`.

### Limiting a Certificate to One Host

Every certificate lists the hosts it opens, and an empty list opens nothing. This holds even when an app has several domains or a wildcard: a certificate for `auth.example.com` does not open `admin.example.com`.

- A host matches exactly.
- `*.example.com` covers one level: `a.example.com`, but not `a.b.example.com` or `example.com`.
- Scope is stored on the server, so changing it needs no new certificate and applies on the next request.

The proxy can only tell hosts apart. If your app serves `/admin` on every host, a certificate for `auth.example.com` can open `auth.example.com/admin`, so separate those areas by host in your app.

### Where the Private Key Lives

With `--out file.p12`, the CLI creates the key on the machine it runs on and writes it only into that password-protected file. To keep the key off the server entirely, create it elsewhere and send a CSR:

```bash
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout alice.key -out alice.csr -subj "/CN=alice"
odac domain mtls issue -d example.com --name alice --hosts auth.example.com --csr alice.csr --out alice.crt
```

ODAC uses only the public key from a CSR. It decides the subject, usage and lifetime itself.

### Renewing a Certificate

```bash
odac domain mtls issue -d example.com --renews <fingerprint> --out ali-2027.p12
```

A renewal keeps the name and hosts of the certificate it replaces, unless you pass `--hosts`. The old certificate keeps working until it expires or you revoke it, so you can switch over without a gap.

With `--csr` you can keep the same private key. Sending the same request again returns the certificate that request produced, as long as it has not expired. To renew again, pass the fingerprint of the newest certificate.

Revoking a certificate only revokes that certificate. If its key is shared with a renewal, the reply lists the ones that still work. Revoke those too if the key was exposed.

A key stays with one certificate name, and an expired certificate is never returned as a success: use `--renews` to get a new one.

### Using Your Own CA

```bash
odac domain mtls set -d example.com --mode required --ca-file corp-ca.pem
```

ODAC has no record of certificates another CA issues, so:

- They have no scope. To limit one to a host, give that host its own CA or intermediate.
- They are revoked by fingerprint.
- Public CAs are refused, because anyone holding a certificate from one would be let in.

### Verifying in Your App

On a host with a mode set, your app receives these headers:

| Header | Value |
|---|---|
| `X-Odac-Client-Verify` | `success`, `none`, `expired`, `revoked`, `untrusted` or `out_of_scope` |
| `X-Odac-Client-Subject` | The subject, URL-encoded (only on `success`) |
| `X-Odac-Client-Issuer` | SHA-256 of the trusted CA certificate (only on `success`) |
| `X-Odac-Client-Fingerprint` | SHA-256 of the certificate (only on `success`) |
| `X-Odac-Client-Serial` | Serial number in hex (only on `success`) |
| `X-Odac-Client-Not-After` | Expiry in Unix milliseconds (only on `success`) |
| `X-Odac-Client-Signature` | `t=<unix seconds>,v1=<hex>` |

**Verify the signature before trusting any of them.** Other containers on the same network and any published port can reach your app without passing through the proxy, and can send whatever headers they like.

The ODAC framework does this for you through `Odac.Request.clientCert`. Elsewhere, compute HMAC-SHA256 with the key in the `ODAC_CLIENT_CERT_KEY` environment variable over these fields, joined by a newline, with an absent header as an empty string:

```
v1
<t>
<method>
<host header>
<request URI>
<verify>
<subject>
<issuer>
<fingerprint>
<serial>
<not-after>
```

Compare in constant time and reject a `t` more than 60 seconds away from your clock. Treat anything except a verified `success` as anonymous.

To identify a user, match on issuer and subject together. These stay the same when a certificate is renewed with `--renews`, while the fingerprint changes.

`ODAC_CLIENT_CERT_KEY` is set when a container starts, so restart the app after giving it its first mode. Until then it has no key and every request counts as unverified.

### Browser Behavior

- **Browsers ask when connecting, not per page.** Your app can refuse `/admin` to a visitor without a certificate, but it cannot make the certificate prompt appear when they reach it.
- **Canceling is remembered.** If someone dismisses the prompt, the browser usually keeps that choice for a while, so a "Sign in with certificate" button cannot bring it back on the same host.
- **Log out cannot clear the choice.** The browser keeps sending the certificate, so your app has to remember that the user signed out.

For a login flow, use a separate host. Make `auth.example.com` `required`, verify the certificate there, set a session cookie for `.example.com`, and send the user back to `example.com`.

### Limits

- **Published ports are not protected.** They reach the app directly. ODAC warns when you turn on a mode for such an app.
- **Other domains on the same app are not protected** unless you set a mode on them too. ODAC warns about this as well.
- **Hosts must be registered.** Once any host of an app has a mode, a subdomain that is neither listed nor covered by a wildcard is refused on all of that app's domains.
- **Tunnels are refused.** A tunnel reaches the app directly, so an app cannot have both.
- **HTTP/3 is off** on hosts with a mode, including visitors with no certificate on `optional` hosts. They use HTTP/2 instead.
- **Requests with a certificate skip the proxy cache**, since the answer may depend on who is asking. Anonymous requests on `optional` hosts are cached as usual.

### Security Notes

- CA private keys stay in `cert/mtls/` on the server and never reach the proxy or the Cloud.
- Issued certificates and revocations are in `cert/mtls/certs.db`. Include all of `cert/mtls/` in backups: restoring an older copy brings revoked certificates back.
- Revoking a certificate also closes its open connections, including WebSockets.
- Deleting a domain removes its hosts from every certificate, and a CA that no domain uses any more is deleted with its key. Adding the domain again does not bring old certificates back.
