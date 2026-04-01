/**
 * Native ACME (RFC 8555) client for automated certificate issuance.
 * Eliminates external dependency on acme-client by implementing the protocol
 * directly using Node.js built-in crypto and https modules.
 * Uses EC P-256 for both account and domain keys — providing equivalent security
 * to RSA-3072 with significantly faster TLS handshakes and smaller payloads.
 */

const https = require('https')
const {createHash, createPublicKey, generateKeyPairSync, sign} = require('crypto')

// ─── Base64url (RFC 4648 §5) ────────────────────────────────────────────────

/** @param {Buffer|string} input @returns {string} URL-safe base64 without padding */
function b64url(input) {
  return (Buffer.isBuffer(input) ? input : Buffer.from(input)).toString('base64url')
}

// ─── ASN.1 DER Encoding (ITU-T X.690) ──────────────────────────────────────

/** Encodes DER length octets */
function derLen(len) {
  if (len < 0x80) return Buffer.from([len])
  const bytes = []
  let tmp = len
  while (tmp > 0) {
    bytes.unshift(tmp & 0xff)
    tmp >>= 8
  }
  return Buffer.from([0x80 | bytes.length, ...bytes])
}

/** Wraps data fragments in a single DER TLV (Tag-Length-Value) */
function der(tag, ...parts) {
  const data = Buffer.concat(parts.map(p => (Buffer.isBuffer(p) ? p : Buffer.from(p))))
  return Buffer.concat([Buffer.from([tag]), derLen(data.length), data])
}

/* Convenience wrappers – kept terse per project conventions */
const BITS = d => der(0x03, Buffer.concat([Buffer.from([0x00]), d]))
const CTX = (n, d) => der(0xa0 | n, d)
const INT = n => {
  const b = []
  let t = n
  while (t > 0) {
    b.unshift(t & 0xff)
    t >>= 8
  }
  if (!b.length || b[0] & 0x80) b.unshift(0)
  return der(0x02, Buffer.from(b))
}
const OCT = d => der(0x04, d)
const SEQ = (...p) => der(0x30, ...p)
const SET = (...p) => der(0x31, ...p)
const UTF8 = s => der(0x0c, Buffer.from(s, 'utf8'))

/** Encodes an OID dot-string (e.g. "2.5.4.3") into DER */
function OID(oid) {
  const p = oid.split('.').map(Number)
  const b = [40 * p[0] + p[1]]
  for (let i = 2; i < p.length; i++) {
    let v = p[i]
    if (v < 128) {
      b.push(v)
      continue
    }
    const e = []
    e.push(v & 0x7f)
    v >>= 7
    while (v > 0) {
      e.push((v & 0x7f) | 0x80)
      v >>= 7
    }
    b.push(...e.reverse())
  }
  return der(0x06, Buffer.from(b))
}

// ─── HTTPS Transport ────────────────────────────────────────────────────────

/** Minimal HTTPS request helper – returns {status, headers, body} */
function httpRequest(url, opts = {}) {
  return new Promise((resolve, reject) => {
    const req = https.request(url, {method: opts.method || 'GET', headers: opts.headers || {}}, res => {
      const chunks = []
      res.on('data', c => chunks.push(c))
      res.on('end', () => {
        const raw = Buffer.concat(chunks).toString('utf8')
        const ct = res.headers['content-type'] || ''
        let body = raw
        if (ct.includes('json') && raw) {
          try {
            body = JSON.parse(raw)
          } catch {
            /* keep raw */
          }
        }
        resolve({status: res.statusCode, headers: res.headers, body})
      })
    })
    req.on('error', reject)
    if (opts.body) req.write(opts.body)
    req.end()
  })
}

// ─── ACME Client ────────────────────────────────────────────────────────────

class Acme {
  /** Let's Encrypt production ACME directory URL */
  static LETS_ENCRYPT = 'https://acme-v02.api.letsencrypt.org/directory'

  #accountKey
  #accountJwk
  #accountUrl
  #directory
  #nonce
  #thumbprint

  /**
   * Factory: creates and initialises an ACME client with a fresh ephemeral account.
   * Performs directory discovery, nonce acquisition, and account registration in one shot.
   * @param {string} [directoryUrl] ACME directory URL (defaults to Let's Encrypt production)
   * @returns {Promise<Acme>}
   */
  static async create(directoryUrl = Acme.LETS_ENCRYPT) {
    const client = new Acme()
    await client.#init(directoryUrl)
    return client
  }

  /**
   * Generates an EC P-256 key pair for domain certificates.
   * EC P-256 provides equivalent security to RSA-3072 with ~10x faster signatures.
   * @returns {{privateKey: crypto.KeyObject, pem: string}} Key object and PKCS#8 PEM
   */
  static generateKeyPair() {
    const {privateKey} = generateKeyPairSync('ec', {namedCurve: 'P-256'})
    return {
      pem: privateKey.export({type: 'pkcs8', format: 'pem'}),
      privateKey
    }
  }

  /**
   * Builds a DER-encoded PKCS#10 Certificate Signing Request with SAN extension.
   * @param {string[]} domains Domain names — first entry becomes the CommonName
   * @param {crypto.KeyObject} privateKey EC P-256 private key corresponding to the domain cert
   * @returns {Buffer} DER-encoded CSR ready for ACME finalization
   */
  static createCsr(domains, privateKey) {
    const publicKey = createPublicKey(privateKey)
    const spki = publicKey.export({type: 'spki', format: 'der'})

    // Subject: CN = primary domain
    const subject = SEQ(SET(SEQ(OID('2.5.4.3'), UTF8(domains[0]))))

    // SubjectAltName extension (all domains including CN)
    const sanEntries = domains.map(d => der(0x82, Buffer.from(d, 'ascii')))
    const extensions = SEQ(SEQ(OID('2.5.29.17'), OCT(SEQ(...sanEntries))))

    // extensionRequest attribute ([0] IMPLICIT per PKCS#10)
    const attributes = CTX(0, SEQ(OID('1.2.840.113549.1.9.14'), SET(extensions)))

    // CertificationRequestInfo
    const info = SEQ(INT(0), subject, spki, attributes)

    // Sign with ECDSA-SHA256 (DER-encoded signature for X.509)
    const signature = sign('SHA256', info, privateKey)

    // Full CSR: info + algorithm + signature
    return SEQ(info, SEQ(OID('1.2.840.10045.4.3.2')), BITS(signature))
  }

  /**
   * Executes the full ACME order lifecycle: order creation, authorization,
   * challenge validation, finalization, and certificate download.
   * @param {Object} opts
   * @param {Buffer} opts.csr DER-encoded CSR
   * @param {string[]} opts.domains Domain names matching the CSR identifiers
   * @param {string} opts.challengeType 'http-01' or 'dns-01'
   * @param {Function} opts.challengeCreateFn (authz, challenge, keyAuthorization) => Promise
   * @param {Function} opts.challengeRemoveFn (authz, challenge, keyAuthorization) => Promise
   * @returns {Promise<string>} PEM-encoded certificate chain
   */
  async order({csr, domains, challengeType, challengeCreateFn, challengeRemoveFn}) {
    // 1. Create order
    const identifiers = domains.map(d => ({type: 'dns', value: d}))
    const orderRes = await this.#signedRequest(this.#directory.newOrder, {identifiers})

    if (orderRes.status >= 400) {
      throw new Error('ACME order creation failed: ' + (orderRes.body?.detail || JSON.stringify(orderRes.body)))
    }

    const order = orderRes.body
    const orderUrl = orderRes.headers.location

    // 2. Process each authorization
    for (const authzUrl of order.authorizations) {
      const authzRes = await this.#signedRequest(authzUrl, '')
      const authz = authzRes.body

      if (authz.status === 'valid') continue

      const challenge = authz.challenges.find(c => c.type === challengeType)
      if (!challenge) {
        throw new Error('Challenge type ' + challengeType + ' not available for ' + authz.identifier.value)
      }

      // Compute key authorization per RFC 8555 §8.1
      const keyAuth = challenge.token + '.' + this.#thumbprint
      const authValue = challengeType === 'dns-01' ? b64url(createHash('sha256').update(keyAuth).digest()) : keyAuth

      await challengeCreateFn(authz, challenge, authValue)
      await this.#signedRequest(challenge.url, {})
      await this.#poll(authzUrl, ['valid'], ['deactivated', 'expired', 'invalid', 'revoked'])

      try {
        await challengeRemoveFn(authz, challenge, authValue)
      } catch {
        /* best-effort cleanup */
      }
    }

    // 3. Finalize order with CSR
    const finalizeRes = await this.#signedRequest(order.finalize, {csr: b64url(csr)})
    let finalOrder = finalizeRes.body

    if (finalOrder.status !== 'valid') {
      finalOrder = await this.#poll(orderUrl, ['valid'], ['invalid'])
    }

    // 4. Download certificate
    const certRes = await this.#signedRequest(finalOrder.certificate, '')
    return certRes.body
  }

  // ─── Private Methods ────────────────────────────────────────────────────

  /** Bootstraps account key, directory, nonce, and ACME account */
  async #init(directoryUrl) {
    const {privateKey, publicKey} = generateKeyPairSync('ec', {namedCurve: 'P-256'})
    this.#accountKey = privateKey
    this.#accountJwk = publicKey.export({format: 'jwk'})

    // JWK Thumbprint (RFC 7638) – lexicographically sorted EC members
    const thumbJson = JSON.stringify({
      crv: this.#accountJwk.crv,
      kty: this.#accountJwk.kty,
      x: this.#accountJwk.x,
      y: this.#accountJwk.y
    })
    this.#thumbprint = b64url(createHash('sha256').update(thumbJson).digest())

    // Directory discovery
    const dirRes = await httpRequest(directoryUrl)
    this.#directory = dirRes.body

    // Initial nonce
    await this.#fetchNonce()

    // Account registration (or retrieval if already exists)
    const acctRes = await this.#signedRequest(this.#directory.newAccount, {onlyReturnExisting: false, termsOfServiceAgreed: true}, true)
    this.#accountUrl = acctRes.headers.location
  }

  /** Fetches a fresh anti-replay nonce from the ACME server */
  async #fetchNonce() {
    const res = await httpRequest(this.#directory.newNonce, {method: 'HEAD'})
    this.#nonce = res.headers['replay-nonce']
  }

  /**
   * Sends a JWS-signed request to an ACME endpoint (RFC 8555 §6.2).
   * @param {string} url Target ACME endpoint
   * @param {Object|string} payload Object for POST, empty string '' for POST-as-GET
   * @param {boolean} [useJwk=false] Use JWK header instead of kid (for newAccount)
   */
  async #signedRequest(url, payload, useJwk = false) {
    if (!this.#nonce) await this.#fetchNonce()

    const header = {alg: 'ES256', nonce: this.#nonce, url}

    if (useJwk) {
      header.jwk = {
        crv: this.#accountJwk.crv,
        kty: this.#accountJwk.kty,
        x: this.#accountJwk.x,
        y: this.#accountJwk.y
      }
    } else {
      header.kid = this.#accountUrl
    }

    const protectedB64 = b64url(JSON.stringify(header))
    const payloadB64 = payload === '' ? '' : b64url(JSON.stringify(payload))

    // ES256 signature in IEEE P1363 format (raw r||s) per JWS spec
    const signature = sign('SHA256', Buffer.from(protectedB64 + '.' + payloadB64), {
      key: this.#accountKey,
      dsaEncoding: 'ieee-p1363'
    })

    const res = await httpRequest(url, {
      body: JSON.stringify({
        payload: payloadB64,
        protected: protectedB64,
        signature: b64url(signature)
      }),
      headers: {'Content-Type': 'application/jose+json'},
      method: 'POST'
    })

    if (res.headers['replay-nonce']) this.#nonce = res.headers['replay-nonce']
    return res
  }

  /** Polls an ACME resource until it reaches one of the target statuses */
  async #poll(url, successStatuses, failStatuses, maxAttempts = 30) {
    for (let i = 0; i < maxAttempts; i++) {
      await new Promise(r => setTimeout(r, i < 5 ? 1000 : 3000))
      const res = await this.#signedRequest(url, '')
      if (successStatuses.includes(res.body?.status)) return res.body
      if (failStatuses.includes(res.body?.status)) {
        throw new Error('ACME validation failed: ' + (res.body?.detail || JSON.stringify(res.body)))
      }
    }
    throw new Error('ACME polling timeout after ' + maxAttempts + ' attempts')
  }
}

module.exports = Acme
