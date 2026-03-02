const {log, error} = Odac.core('Log', false).init('SSL')

const acme = require('acme-client')
const fs = require('fs')
const os = require('os')
const selfsigned = require('selfsigned')
const nodeCrypto = require('crypto')

class SSL {
  #checking = false
  #checked = {}
  #processing = new Map()
  #queued = new Set()

  async check() {
    if (this.#checking || this.#processing.size > 0 || this.#queued.size > 0 || !Odac.core('Config').config.domains) return
    this.#checking = true
    this.#self()
    for (const domain of Object.keys(Odac.core('Config').config.domains)) {
      const record = Odac.core('Config').config.domains[domain]
      if (record.cert === false) continue

      // Check Expiry
      if (!record.cert?.ssl || Date.now() + 1000 * 60 * 60 * 24 * 30 > record.cert.ssl.expiry) {
        await this.#ssl(domain)
        continue
      }

      // Check SAN Mismatch (Missing subdomains) - Run every 5 minutes
      if (!this.#checked[domain]) this.#checked[domain] = {}
      if ((this.#checked[domain].lastSanCheck || 0) + 1000 * 60 * 5 < Date.now()) {
        this.#checked[domain].lastSanCheck = Date.now()
        if (this.#checkSanMismatch(domain)) {
          log('Detected missing subdomains in SSL certificate for %s. Queuing renewal.', domain)
          await this.#ssl(domain)
        }
      }
    }
    this.#checking = false
  }

  // ... helper
  #checkSanMismatch(domain) {
    try {
      const record = Odac.core('Config').config.domains[domain]
      const certPath = record.cert?.ssl?.cert
      if (!certPath || !fs.existsSync(certPath)) return true // Missing cert, needs generation

      const certBuffer = fs.readFileSync(certPath)
      const x509 = new nodeCrypto.X509Certificate(certBuffer)

      // x509.subjectAltName returns string like "DNS:example.com, DNS:www.example.com"
      const sanString = x509.subjectAltName || ''
      const sans = sanString.split(',').map(s => s.trim().replace('DNS:', ''))

      const expected = [domain]
      if (record.subdomain) {
        record.subdomain.forEach(sub => expected.push(sub + '.' + domain))
      }

      // Check if every expected domain is in SANs
      const missing = expected.some(d => !sans.includes(d))
      if (missing) {
        log('SSL SAN Mismatch for %s. Expected: [%s], Found: [%s]', domain, expected.join(', '), sans.join(', '))
        return true
      }
    } catch (e) {
      error('Failed to parse certificate for SAN check for %s: %s', domain, e.message)
    }
    return false
  }

  renew(domain) {
    if (domain.match(/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/))
      return Odac.server('Api').result(false, __('SSL renewal is not available for IP addresses.'))

    // Direct lookup in domains object
    if (!Odac.core('Config').config.domains[domain]) {
      // Check if it's a subdomain being requested
      let found = false
      for (const [key, record] of Object.entries(Odac.core('Config').config.domains)) {
        if (record.subdomain && record.subdomain.some(sub => sub + '.' + key === domain)) {
          domain = key
          found = true
          break
        }
      }
      if (!found) return Odac.server('Api').result(false, __('Domain %s not found.', domain))
    }

    this.#ssl(domain)
    return Odac.server('Api').result(true, __('SSL certificate for domain %s renewed successfully.', domain))
  }

  /**
   * Attempts SSL certificate generation with HTTP-01 challenge first, then falls back to DNS-01.
   * HTTP-01 is faster and works without nameserver delegation, but requires the Go proxy
   * to serve tokens on port 80. DNS-01 is the universal fallback.
   * @param {import('acme-client').Client} client - ACME client instance
   * @param {Buffer} csr - Certificate Signing Request
   * @param {{cancelled: boolean}} context - Cancellation context
   * @returns {Promise<string|null>} PEM certificate or null
   */
  async #requestCertificate(client, csr, context) {
    // Phase 1: Try HTTP-01 (fast, no DNS delegation needed)
    try {
      log('Attempting SSL via HTTP-01 challenge...')
      return await this.#acmeAuto(client, csr, 'http-01', context)
    } catch (err) {
      if (context.cancelled) throw err // Propagate cancellation, don't fallback
      log('HTTP-01 challenge failed: %s. Falling back to DNS-01...', err.message)
    }

    // Phase 2: Fallback to DNS-01 (requires nameserver delegation)
    return await this.#acmeAuto(client, csr, 'dns-01', context)
  }

  /**
   * Executes ACME auto flow with a specific challenge type.
   * @param {import('acme-client').Client} client - ACME client instance
   * @param {Buffer} csr - Certificate Signing Request
   * @param {'dns-01'|'http-01'} challengeType - Challenge type to use
   * @param {{cancelled: boolean}} context - Cancellation context
   * @returns {Promise<string>} PEM certificate
   */
  async #acmeAuto(client, csr, challengeType, context) {
    return client.auto({
      csr,
      termsOfServiceAgreed: true,
      challengePriority: [challengeType],
      challengeCreateFn: async (authz, challenge, keyAuthorization) => {
        if (context.cancelled) return

        if (challenge.type === 'http-01') {
          log('Creating HTTP-01 challenge for %s (token: %s...)', authz.identifier.value, challenge.token.substring(0, 8))
          await Odac.server('Proxy').setACMEChallenge(challenge.token, keyAuthorization)
        } else if (challenge.type === 'dns-01') {
          log('Creating DNS-01 challenge for %s', authz.identifier.value)
          Odac.server('DNS').record({
            name: '_acme-challenge.' + authz.identifier.value,
            type: 'TXT',
            value: keyAuthorization,
            ttl: 100,
            unique: true
          })
        }
      },
      challengeRemoveFn: async (authz, challenge, keyAuthorization) => {
        if (challenge.type === 'http-01') {
          log('Removing HTTP-01 challenge for %s (token: %s...)', authz.identifier.value, challenge.token.substring(0, 8))
          try {
            await Odac.server('Proxy').deleteACMEChallenge(challenge.token)
          } catch {
            /* best-effort cleanup */
          }
        } else if (challenge.type === 'dns-01') {
          log('Removing DNS-01 challenge for %s', authz.identifier.value)
          Odac.server('DNS').delete({
            name: '_acme-challenge.' + authz.identifier.value,
            type: 'TXT',
            value: keyAuthorization
          })
        }
      }
    })
  }

  #self() {
    let ssl = Odac.core('Config').config.ssl ?? {}
    if (ssl && ssl.expiry > Date.now() && ssl.key && ssl.cert && fs.existsSync(ssl.key) && fs.existsSync(ssl.cert)) return
    log('Generating self-signed SSL certificate...')
    const attrs = [{name: 'commonName', value: 'Odac'}]
    const pems = selfsigned.generate(attrs, {days: 365, keySize: 2048})
    if (!fs.existsSync(os.homedir() + '/.odac/cert/ssl')) fs.mkdirSync(os.homedir() + '/.odac/cert/ssl', {recursive: true})
    let key_file = os.homedir() + '/.odac/cert/ssl/odac.key'
    let crt_file = os.homedir() + '/.odac/cert/ssl/odac.crt'
    fs.writeFileSync(key_file, pems.private)
    fs.writeFileSync(crt_file, pems.cert)
    ssl.key = key_file
    ssl.cert = crt_file
    ssl.expiry = Date.now() + 86400000
    Odac.core('Config').config.ssl = ssl
  }

  async #ssl(domain) {
    if (this.#processing.has(domain)) {
      this.#processing.get(domain).cancelled = true
      log('SSL generation for %s is outdated due to config change. Cancelling current run and queuing fresh generation.', domain)
      this.#queued.add(domain)
      return
    }

    if (this.#checked[domain]?.interval > Date.now()) return

    const context = {cancelled: false}
    this.#processing.set(domain, context)

    try {
      const accountPrivateKey = await acme.forge.createPrivateKey()

      const client = new acme.Client({
        directoryUrl: acme.directory.letsencrypt.production,
        accountKey: accountPrivateKey
      })

      const domainRecord = Odac.core('Config').config.domains[domain]
      let subdomains = [domain]
      if (domainRecord && domainRecord.subdomain) {
        for (const subdomain of domainRecord.subdomain) {
          subdomains.push(subdomain + '.' + domain)
        }
      }

      if (context.cancelled) {
        log('SSL generation for %s cancelled before CSR creation.', domain)
        return
      }

      const [key, csr] = await acme.forge.createCsr({
        commonName: domain,
        altNames: subdomains
      })

      log('Requesting SSL certificate for domain %s...', domain)

      if (context.cancelled) {
        log('SSL generation for %s cancelled before ACME request.', domain)
        return
      }

      const cert = await this.#requestCertificate(client, csr, context)

      if (context.cancelled) {
        log('SSL generation for %s cancelled after ACME response. Discarding stale certificate.', domain)
        return
      }

      if (!cert) {
        error('SSL certificate generation failed for domain %s: No certificate returned', domain)
        return
      }

      this.#saveCertificate(domain, key, cert)
    } catch (err) {
      if (!context.cancelled) {
        this.#handleSSLError(domain, err)
      } else {
        log('SSL generation for %s cancelled during execution. Suppressing error backoff.', domain)
      }
    } finally {
      this.#processing.delete(domain)
      if (this.#queued.has(domain)) {
        this.#queued.delete(domain)
        delete this.#checked[domain]
        log('Processing queued SSL generation for %s', domain)
        this.#ssl(domain)
      }
    }
  }

  #handleSSLError(domain, err) {
    if (!this.#checked[domain]) this.#checked[domain] = {error: 0}
    this.#checked[domain].error += 1

    const errorCount = this.#checked[domain].error
    let backoffMs = 1000 * 30 // Start with 30s
    if (errorCount === 2) {
      backoffMs = 1000 * 60 * 2 // 2m
    } else if (errorCount === 3) {
      backoffMs = 1000 * 60 * 10 // 10m
    } else if (errorCount >= 4) {
      backoffMs = 1000 * 60 * 30 // 30m
    }

    this.#checked[domain].interval = Date.now() + backoffMs

    // More specific error handling
    if (err.message && err.message.includes('validateStatus')) {
      error(
        'SSL certificate request failed for domain %s (Attempt %d). Next retry in %ds. Reason: HTTP validation error.',
        domain,
        errorCount,
        backoffMs / 1000
      )
    } else if (err.code === 'ENOTFOUND' || err.code === 'ECONNREFUSED') {
      error(
        'SSL request failed for domain %s (Attempt %d). Next retry in %ds. Network issue: %s',
        domain,
        errorCount,
        backoffMs / 1000,
        err.message
      )
    } else {
      error(
        'SSL request failed for domain %s (Attempt %d). Next retry in %ds. Error: %s',
        domain,
        errorCount,
        backoffMs / 1000,
        err.message
      )
    }
  }

  #saveCertificate(domain, key, cert) {
    try {
      delete this.#checked[domain]

      if (!fs.existsSync(os.homedir() + '/.odac/cert/ssl')) {
        fs.mkdirSync(os.homedir() + '/.odac/cert/ssl', {recursive: true})
      }

      fs.writeFileSync(os.homedir() + '/.odac/cert/ssl/' + domain + '.key', key)
      fs.writeFileSync(os.homedir() + '/.odac/cert/ssl/' + domain + '.crt', cert)

      let domains = Odac.core('Config').config.domains ?? {}
      let domainRecord = domains[domain]
      if (!domainRecord) return

      if (!domainRecord.cert) domainRecord.cert = {}
      domainRecord.cert.ssl = {
        key: os.homedir() + '/.odac/cert/ssl/' + domain + '.key',
        cert: os.homedir() + '/.odac/cert/ssl/' + domain + '.crt',
        expiry: Date.now() + 1000 * 60 * 60 * 24 * 30 * 3
      }

      domains[domain] = domainRecord
      Odac.core('Config').config.domains = domains

      try {
        if (Odac.server('Mail')) Odac.server('Mail').clearSSLCache(domain)
      } catch {
        // Ignore error
      }

      // Sync proxy config to reload SSL certificates
      try {
        if (Odac.server('Proxy')) Odac.server('Proxy').syncConfig()
      } catch (e) {
        error('Failed to sync proxy config after SSL update: %s', e.message)
      }

      log('SSL certificate successfully generated and saved for domain %s', domain)
    } catch (err) {
      error('Failed to save SSL certificate for domain %s: %s', domain, err.message)
    }
  }
}

module.exports = new SSL()
