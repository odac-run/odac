const {log, error} = Odac.core('Log', false).init('Domain')

/**
 * Domain management service for ODAC.
 * Handles domain CRUD operations, validation, and DNS zone management.
 * Provides domain-to-app binding functionality for routing traffic.
 *
 * Config structure (config.domains):
 * [
 *   { domain: "example.com", appId: "myapp", created: 1234567890 },
 *   { domain: "api.example.com", appId: "myapp", created: 1234567890 }
 * ]
 */
class Domain {
  /**
   * Returns the domains configuration array.
   * Initializes if not present.
   * @returns {Array} domains array
   */
  #getDomains() {
    if (!Odac.core('Config').config.domains) {
      Odac.core('Config').config.domains = []
    }
    return Odac.core('Config').config.domains
  }

  /**
   * Validates and sanitizes a domain name.
   * @param {string} domain - Raw domain input
   * @returns {{valid: boolean, domain?: string, error?: string}}
   */
  #validate(domain) {
    if (!domain || typeof domain !== 'string') {
      return {valid: false, error: __('Domain is required.')}
    }

    // Sanitize domain input
    domain = domain.trim().toLowerCase()
    for (const prefix of ['http://', 'https://', 'ftp://', 'www.']) {
      if (domain.startsWith(prefix)) domain = domain.replace(prefix, '')
    }

    // Security: Validate domain format to prevent injection attacks
    if (
      domain.length < 3 ||
      (!domain.includes('.') && domain !== 'localhost') ||
      domain.includes('/') ||
      domain.includes('\\') ||
      domain.includes('..')
    ) {
      return {valid: false, error: __('Invalid domain format.')}
    }

    return {valid: true, domain}
  }

  /**
   * Adds a domain and binds it to an existing App.
   * Creates DNS records and associates the domain with the specified application.
   *
   * @param {string} domain - The domain name to add (e.g., "example.com")
   * @param {string|number} appId - The App ID or name to bind this domain to
   * @returns {Promise<{result: boolean, message: string}>} API result object
   */
  async add(domain, appId) {
    // Phase 1: Input Validation
    const validation = this.#validate(domain)
    if (!validation.valid) {
      return Odac.server('Api').result(false, validation.error)
    }
    domain = validation.domain

    if (!appId) {
      return Odac.server('Api').result(false, __('App ID is required.'))
    }

    // Phase 2: Check if domain already exists in domains config
    const domains = this.#getDomains()
    const existing = domains.find(d => d.domain === domain)
    if (existing) {
      return Odac.server('Api').result(false, __('Domain %s is already registered.', domain))
    }

    // Phase 3: Verify target App exists
    const apps = Odac.core('Config').config.apps || []
    const targetApp = apps.find(app => app.id === appId || app.name === appId)
    if (!targetApp) {
      return Odac.server('Api').result(false, __('App %s not found.', appId))
    }

    // Phase 4: Create DNS records for the domain (skip for localhost and IP addresses)
    let sslEnabled = false
    if (domain !== 'localhost' && !domain.match(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)) {
      try {
        // Build DNS records - A and AAAA without value, DNS will resolve dynamically via PTR matching
        const dnsRecords = [
          {name: domain, type: 'A'},
          {name: domain, type: 'AAAA'},
          {name: 'www.' + domain, type: 'CNAME', value: domain},
          {name: domain, type: 'MX', value: domain},
          {
            name: '_dmarc.' + domain,
            type: 'TXT',
            value: 'v=DMARC1; p=reject; rua=mailto:postmaster@' + domain
          }
        ]

        // Build SPF record - needs explicit IPs for external validation
        const publicIPv4 = Odac.server('DNS').ip
        const publicIPv6 = Odac.server('DNS').ips?.ipv6?.find(i => i.public)
        let spfValue = 'v=spf1 a mx'
        if (publicIPv4 && publicIPv4 !== '127.0.0.1') {
          spfValue += ' ip4:' + publicIPv4
        }
        if (publicIPv6) {
          spfValue += ' ip6:' + publicIPv6.address
        }
        spfValue += ' ~all'
        dnsRecords.push({name: domain, type: 'TXT', value: spfValue})

        Odac.server('DNS').record(...dnsRecords)
        log('Created DNS records for domain %s', domain)

        // Mark SSL as enabled for certificate provisioning
        sslEnabled = true
      } catch (e) {
        error('Failed to create DNS records for %s: %s', domain, e.message)
        return Odac.server('Api').result(false, __('Failed to create DNS records: %s', e.message))
      }
    }

    // Phase 5: Add domain record to domains config
    const domainRecord = {
      appId: targetApp.name,
      created: Date.now(),
      domain,
      subdomain: ['www']
    }

    // Initialize SSL cert tracking if SSL is enabled
    if (sslEnabled) {
      domainRecord.cert = {}
    }

    domains.push(domainRecord)

    // Phase 6: Persist configuration (triggers auto-save via proxy)
    Odac.core('Config').config.domains = domains

    // Phase 7: Trigger SSL certificate provisioning for non-localhost domains
    if (sslEnabled) {
      try {
        Odac.server('SSL').renew(domain)
        log('Initiated SSL certificate provisioning for %s', domain)
      } catch (e) {
        // Non-fatal: SSL will be retried later
        error('SSL provisioning failed for %s: %s', domain, e.message)
      }
    }

    log('Domain %s added to app %s', domain, targetApp.name)
    return Odac.server('Api').result(true, __('Domain %s added to app %s.', domain, targetApp.name))
  }
}

module.exports = new Domain()
