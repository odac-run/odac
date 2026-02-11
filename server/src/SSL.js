const {log, error} = Odac.core('Log', false).init('SSL')

const acme = require('acme-client')
const fs = require('fs')
const os = require('os')
const selfsigned = require('selfsigned')
const nodeCrypto = require('crypto')

class SSL {
  #checking = false
  #checked = {}
  #processing = new Set()
  #queued = new Set()

  async check() {
    if (this.#checking || !Odac.core('Config').config.domains) return
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
      log('SSL generation for %s is already in progress. Queuing next run.', domain)
      this.#queued.add(domain)
      return
    }

    // If queued, we want to run immediately, so we ignore the interval check
    // But if it's a standard check, we respect it.
    // We can check if it was queued to bypass, but implicit is fine:
    // If it was queued, it's called from finally block where we might want to reset interval?
    // Actually, simply: if in queue, we assume "force update".
    // But here we are entering the function. #queued was deleted before calling this recursively?
    // Let's handle logic inside finally block carefully.

    if (this.#checked[domain]?.interval > Date.now()) return

    this.#processing.add(domain)

    try {
      const accountPrivateKey = await acme.forge.createPrivateKey()

      // Create ACME client with proper error handling configuration
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

      const [key, csr] = await acme.forge.createCsr({
        commonName: domain,
        altNames: subdomains
      })

      log('Requesting SSL certificate for domain %s...', domain)

      const cert = await client.auto({
        csr,
        termsOfServiceAgreed: true,
        challengePriority: ['dns-01'],
        challengeCreateFn: async (authz, challenge, keyAuthorization) => {
          if (challenge.type === 'dns-01') {
            log('Creating DNS challenge for %s', authz.identifier.value)
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
          if (challenge.type === 'dns-01') {
            log('Removing DNS challenge for %s', authz.identifier.value)
            Odac.server('DNS').delete({
              name: '_acme-challenge.' + authz.identifier.value,
              type: 'TXT',
              value: keyAuthorization
            })
          }
        }
      })

      if (!cert) {
        error('SSL certificate generation failed for domain %s: No certificate returned', domain)
        return
      }

      // Save certificate files
      this.#saveCertificate(domain, key, cert)
    } catch (err) {
      this.#handleSSLError(domain, err)
    } finally {
      this.#processing.delete(domain)
      if (this.#queued.has(domain)) {
        this.#queued.delete(domain)
        // Clear collision/error interval to force retry because configuration changed (presumed, since it was queued)
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
