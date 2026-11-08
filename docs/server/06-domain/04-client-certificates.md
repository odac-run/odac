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
| `odac domain mtls set -d <host> --mode <mode>` | Set a host's mode. `--ca <ids>` replaces the trusted CAs, `--ca-file <pem>` adds your own. On a main domain, `--issuer per-host` gives each host its own issuer (see [One Issuer per Host](#one-issuer-per-host)) |
| `odac domain mtls off -d <host>` | Turn it off. On a main domain this clears all of its hosts |
| `odac domain mtls issue -d <host> --name <name> --hosts <hosts>` | Issue a certificate. `--out`, `--days`, `--password`, `--csr <file>`, `--renews <fingerprint>` |
| `odac domain mtls scope --fingerprint <fp> --hosts <hosts>` | Change which hosts a certificate opens |
| `odac domain mtls revoke --fingerprint <fp>` | Revoke a certificate |
| `odac domain mtls list [-d <host>]` | List certificates |
| `odac domain mtls ca import --cert <pem> --key <pem>` | Import a CA with its key, to share it between servers |
| `odac domain mtls import --cert <pem> --hosts <hosts>` | Record a certificate another server signed with a shared CA. `--name` must match its subject |

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

- `required` is allowed right away, since ODAC cannot see who holds one. The domain list reports such CAs under `external` in the domain's `mtls` object (and in a host's override), next to the `ca` ids it trusts.
- They have no scope. To limit one to a host, give that host its own CA or intermediate.
- They are revoked by fingerprint.
- Public CAs are refused, because anyone holding a certificate from one would be let in.

### Sharing a CA Between Servers

Several servers can sign and accept the same certificates when they share one CA. Create it once, on a machine you trust:

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca.key -out ca.crt -days 3650 -subj "/CN=Edge Client CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:1" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"
```

Then, on every server:

```bash
odac domain mtls ca import --cert ca.crt --key ca.key
odac domain mtls set -d example.com --mode optional --ca <id>
```

The import prints the CA's id. It is derived from the certificate, so it is the same on every server, and importing the same CA again changes nothing. With ODAC in Docker, run it with `docker exec` and pass paths the container can read.

- The key goes only through the local API, never through ODAC Cloud or an app, and is stored like ODAC's own CA keys (`cert/mtls/`, readable by its owner only). Delete the copies you imported from, and keep the original somewhere safe.
- Bind the CA to a domain right after importing it. Like any CA no domain trusts, it is deleted with its key the next time a domain is removed. Importing it again brings it back.
- A CA already trusted here as your own (`--ca-file`) cannot be imported as managed, because its existing certificates have no records and would all be refused.

Each server keeps its own records, so a certificate signed on one server works on another only once it has a record there too. Give it one with the certificate alone, never its key:

```bash
odac domain mtls import --cert alice.crt --hosts auth.example.com
```

The certificate must be signed by a managed CA that every listed host trusts, and the hosts follow the same rules as `issue`. Importing it again changes nothing, and a certificate revoked on this server cannot be imported, so revoke it on every server that has it. Scope and revocation are kept per server, too.

### One Issuer per Host

A browser decides which certificates to offer by the CA names the server gives when it asks. With one CA for a whole domain, a visitor who holds certificates for ten of its hosts is offered all ten on each of them. On a domain that serves many hosts this way, such as a wildcard with one certificate per subdomain, turn on per-host issuers:

```bash
odac domain mtls set -d example.com --mode optional --issuer per-host
```

From then on, a certificate that opens a single host is signed by that host's own issuer, an intermediate the domain's CA signs with the name `CN=<host>, O=<CA name>`. The handshake on that host names only its issuer, so the browser offers only that host's certificates. A certificate for several hosts, or for a wildcard, is still signed by the CA, and a host one of those opens names the CA as well.

- **The chain grows by one.** An issue reply's `chain`, and the `.p12` the CLI writes, carry the issuer and then the CA. A client must send the issuer along, which browsers and operating systems do once the `.p12` is imported.
- **Every server makes its own issuer.** The issuer is created on first use, and its key is made on that server and never leaves it. Servers sharing a CA all give their issuer the same name, which is all a browser filters on, and each accepts the others' issuers because the shared CA signed them. So [import](#sharing-a-ca-between-servers) a certificate together with its chain (the reply's `cert` followed by its `chain`): this server cannot check another server's issuer without it, and refuses the import.
- **A single-host certificate stays on its host.** Its issuer's name is its host's, so `scope` cannot move it to another host. Issue a new one instead.
- **Existing certificates keep working.** Ones signed by the CA before the switch are still accepted, and their hosts name the CA too. Going back with `--issuer record` is refused while any certificate signed by a per-host issuer is still valid, since nothing would accept it after the switch.
- **It needs a CA ODAC holds the key for, with room for one intermediate.** A domain or host override that trusts your own CA (`--ca-file`) cannot use per-host issuers, and neither can a CA whose `basicConstraints` say `pathlen:0`: it may only sign leaves, so every chain through an issuer under it would be invalid. CAs ODAC creates have `pathlen:1`; create a shared one the same way (see [Sharing a CA Between Servers](#sharing-a-ca-between-servers)), or leave the path length out.
- Hosts longer than 64 characters, the limit of a certificate name, are signed by the CA.
- An issuer is removed once no valid certificate was signed through it, or its domain is deleted.

`domain.list` shows the setting as `"issuer": "per-host"` in the domain's `mtls` object, and nothing when it is the default.

### Issuing From an App

An app can issue, list and revoke certificates for its own domains through the [local API](../03-app/08-api-access.md#client-certificates-for-the-apps-own-domains), once you grant it:

```bash
odac app api my-app --allow domain.mtls.cert.issue,domain.mtls.cert.revoke,domain.mtls.cert.list
odac app restart my-app
```

Turn the mode on yourself first: an app cannot set a mode, create a CA or change a certificate's hosts, and it never reaches another app's domains.

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

- **Only hosts with certificates ask.** On an `optional` host, ODAC sends no certificate request while no valid certificate it knows of opens that host, so visitors are not prompted for nothing and HTTP/3 stays on. A `required` host, and a host that trusts your own CA (whose certificates ODAC cannot count), always ask.
- **A new certificate needs a new connection.** A connection opened while a host was not asking, and a TLS session resumed from one, carry no certificate request. After the first certificate for a host is issued, a visitor may have to reconnect (close the tab or wait about two minutes) before the browser offers it.
- **Browsers ask when connecting, not per page.** Your app can refuse `/admin` to a visitor without a certificate, but it cannot make the certificate prompt appear when they reach it.
- **Canceling is remembered.** If someone dismisses the prompt, the browser usually keeps that choice for a while, so a "Sign in with certificate" button cannot bring it back on the same host.
- **Log out cannot clear the choice.** The browser keeps sending the certificate, so your app has to remember that the user signed out.

For a login flow, use a separate host. Make `auth.example.com` `required`, verify the certificate there, set a session cookie for `.example.com`, and send the user back to `example.com`.

### Limits

- **Published ports are not protected.** They reach the app directly. ODAC warns when you turn on a mode for such an app.
- **Other domains on the same app are not protected** unless you set a mode on them too. ODAC warns about this as well.
- **Hosts must be registered.** Once any host of an app has a mode, a subdomain that is neither listed nor covered by a wildcard is refused on all of that app's domains.
- **Tunnels are refused.** A tunnel reaches the app directly, so an app cannot have both.
- **HTTP/3 is off** on hosts that ask for a certificate, including visitors with no certificate on `optional` hosts. They use HTTP/2 instead. An `optional` host no certificate opens keeps HTTP/3. Browsers remember HTTP/3 for a host, so once its first certificate is issued their next attempt over HTTP/3 fails and they fall back to HTTP/2.
- **Requests with a certificate skip the proxy cache**, since the answer may depend on who is asking. Anonymous requests on `optional` hosts are cached as usual.

### Security Notes

- CA private keys, and the keys of per-host issuers (`cert/mtls/issuers/`), stay on the server and never reach the proxy or the Cloud.
- Issued certificates and revocations are in `cert/mtls/certs.db`. Include all of `cert/mtls/` in backups: restoring an older copy brings revoked certificates back.
- Revoking a certificate also closes its open connections, including WebSockets.
- Deleting a domain removes its hosts from every certificate, and a CA that no domain uses any more is deleted with its key. Adding the domain again does not bring old certificates back.
