const {log, error} = Odac.core('Log', false).init('SSL')

const acme = require('acme-client')
const fs = require('fs')
const os = require('os')
const selfsigned = require('selfsigned')

class SSL {
  #checking = false
  #checked = {}

  async check() {
    if (this.#checking || !Odac.core('Config').config.websites) return
    this.#checking = true
    this.#self()
    for (const domain of Object.keys(Odac.core('Config').config.websites)) {
      if (Odac.core('Config').config.websites[domain].cert === false) continue
      if (
        !Odac.core('Config').config.websites[domain].cert?.ssl ||
        Date.now() + 1000 * 60 * 60 * 24 * 30 > Odac.core('Config').config.websites[domain].cert.ssl.expiry
      )
        await this.#ssl(domain)
    }
    this.#checking = false
  }

  renew(domain) {
    if (domain.match(/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/))
      return Odac.server('Api').result(false, __('SSL renewal is not available for IP addresses.'))
    if (!Odac.core('Config').config.websites[domain]) {
      for (const key of Object.keys(Odac.core('Config').config.websites)) {
        for (const subdomain of Odac.core('Config').config.websites[key].subdomain)
          if (subdomain + '.' + key == domain) {
            domain = key
            break
          }
      }
      if (!Odac.core('Config').config.websites[domain]) return Odac.server('Api').result(false, __('Domain %s not found.', domain))
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
    if (this.#checked[domain]?.interval > Date.now()) return

    try {
      const accountPrivateKey = await acme.forge.createPrivateKey()

      // Create ACME client with proper error handling configuration
      const client = new acme.Client({
        directoryUrl: acme.directory.letsencrypt.production,
        accountKey: accountPrivateKey
      })

      let subdomains = [domain]
      for (const subdomain of Odac.core('Config').config.websites[domain].subdomain ?? []) {
        subdomains.push(subdomain + '.' + domain)
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
    }
  }

  #handleSSLError(domain, err) {
    if (!this.#checked[domain]) this.#checked[domain] = {error: 0}
    if (this.#checked[domain].error < 5) {
      this.#checked[domain].error = this.#checked[domain].error + 1
    }
    this.#checked[domain].interval = this.#checked[domain].error * 1000 * 60 * 5 + Date.now()

    // More specific error handling
    if (err.message && err.message.includes('validateStatus')) {
      error(
        'SSL certificate request failed due to HTTP validation error for domain %s. This may be due to network issues or ACME server problems.',
        domain
      )
    } else if (err.code === 'ENOTFOUND' || err.code === 'ECONNREFUSED') {
      error('SSL certificate request failed due to network connectivity issues for domain %s: %s', domain, err.message)
    } else {
      error('SSL certificate request failed for domain %s: %s', domain, err.message)
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

      let websites = Odac.core('Config').config.websites ?? {}
      let website = websites[domain]
      if (!website) return

      if (!website.cert) website.cert = {}
      website.cert.ssl = {
        key: os.homedir() + '/.odac/cert/ssl/' + domain + '.key',
        cert: os.homedir() + '/.odac/cert/ssl/' + domain + '.crt',
        expiry: Date.now() + 1000 * 60 * 60 * 24 * 30 * 3
      }

      websites[domain] = website
      Odac.core('Config').config.websites = websites

      Odac.server('Web').clearSSLCache(domain)
      Odac.server('Mail').clearSSLCache(domain)

      log('SSL certificate successfully generated and saved for domain %s', domain)
    } catch (err) {
      error('Failed to save SSL certificate for domain %s: %s', domain, err.message)
    }
  }
}

module.exports = new SSL()
