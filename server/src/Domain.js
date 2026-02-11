const {log, error} = Odac.core('Log', false).init('Domain')

/**
 * Domain management service for ODAC.
 * Handles domain CRUD operations, validation, and DNS zone management.
 * Provides domain-to-app binding functionality for routing traffic.
 *
 * Config structure (config.domains):
 * {
 *   "example.com": { appId: "myapp", created: 1234567890, subdomain: ['www'], cert: {...} },
 *   "api.example.com": { appId: "myapp", created: 1234567890, subdomain: [], cert: {...} }
 * }
 */
class Domain {
  /**
   * Returns the domains configuration object.
   * Initializes if not present.
   * @returns {Object} domains object
   */
  #getDomains() {
    if (!Odac.core('Config').config.domains) {
      Odac.core('Config').config.domains = {}
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
    if (domains[domain]) {
      return Odac.server('Api').result(false, __('Domain %s is already registered.', domain))
    }

    // Phase 3: Verify target App exists
    const apps = Odac.core('Config').config.apps || []
    const targetApp = apps.find(app => app.id === appId || app.name === appId)
    if (!targetApp) {
      return Odac.server('Api').result(false, __('App %s not found.', appId))
    }

    // Phase 3.5: Check if domain is a subdomain of an existing domain for the same app
    const sortedDomains = Object.keys(domains).sort((a, b) => b.length - a.length)

    for (const parentDomain of sortedDomains) {
      const record = domains[parentDomain]
      // Check if domain ends with .parentDomain and belongs to the same app
      if (record.appId === targetApp.name && domain.endsWith('.' + parentDomain) && domain !== parentDomain) {
        const sub = domain.slice(0, -(parentDomain.length + 1))

        // Add to subdomain list if not exists
        if (!record.subdomain) record.subdomain = []
        if (!record.subdomain.includes(sub)) {
          record.subdomain.push(sub)

          // Persist config
          Odac.core('Config').config.domains = domains

          // Create DNS record (CNAME to parent)
          try {
            Odac.server('DNS').record({name: domain, type: 'CNAME', value: parentDomain})
            log('Added subdomain %s to %s', sub, parentDomain)
          } catch (e) {
            error('Failed to create DNS for subdomain %s: %s', domain, e.message)
          }

          // Renew SSL for parent to include new subdomain
          try {
            Odac.server('SSL').renew(parentDomain)
          } catch (e) {
            error('Failed to trigger SSL renew for %s: %s', parentDomain, e.message)
          }

          // Sync proxy config (if subdomain added)
          try {
            Odac.server('Proxy').syncConfig()
          } catch (err) {
            error('Proxy sync failed: %s', err.message)
          }

          return Odac.server('Api').result(true, __('Added %s as a subdomain of %s.', domain, parentDomain))
        }

        return Odac.server('Api').result(true, __('Subdomain %s already exists on %s.', sub, parentDomain))
      }
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
      subdomain: ['www']
    }

    // Initialize SSL cert tracking if SSL is enabled
    if (sslEnabled) {
      domainRecord.cert = {}
    }

    domains[domain] = domainRecord

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

    // Sync proxy config to apply changes immediately
    try {
      Odac.server('Proxy').syncConfig()
    } catch (e) {
      error('Failed to sync proxy config: %s', e.message)
    }

    return Odac.server('Api').result(true, __('Domain %s added to app %s.', domain, targetApp.name))
  }

  /**
   * Deletes a domain and removes its DNS records.
   *
   * @param {string} domain - The domain name to delete
   * @returns {Promise<{result: boolean, message: string}>} API result object
   */
  async delete(domain) {
    // Phase 1: Input Validation
    const validation = this.#validate(domain)
    if (!validation.valid) {
      return Odac.server('Api').result(false, validation.error)
    }
    domain = validation.domain

    // Phase 2: Find domain in config
    const domains = this.#getDomains()

    // Case A: Domain is a main domain
    if (domains[domain]) {
      const domainRecord = domains[domain]

      // Phase 3: Delete DNS records (skip for localhost and IP addresses)
      if (domain !== 'localhost' && !domain.match(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)) {
        try {
          // Delete all DNS records associated with this domain
          const recordsToDelete = [
            {name: domain, type: 'A'},
            {name: domain, type: 'AAAA'},
            {name: 'www.' + domain, type: 'CNAME'},
            {name: domain, type: 'MX'},
            {name: domain, type: 'TXT'},
            {name: '_dmarc.' + domain, type: 'TXT'}
          ]

          for (const record of recordsToDelete) {
            try {
              Odac.server('DNS').delete(record)
            } catch {
              // Ignore individual record deletion failures
            }
          }
          log('Deleted DNS records for domain %s', domain)
        } catch (e) {
          error('Failed to delete DNS records for %s: %s', domain, e.message)
          // Continue with domain removal even if DNS cleanup fails
        }
      }

      // Phase 4: Remove domain from config
      delete domains[domain]
      Odac.core('Config').config.domains = domains

      log('Domain %s deleted (was assigned to app %s)', domain, domainRecord.appId)

      // Sync proxy config
      try {
        Odac.server('Proxy').syncConfig()
      } catch (e) {
        error('Failed to sync proxy config: %s', e.message)
      }

      return Odac.server('Api').result(true, __('Domain %s deleted successfully.', domain))
    }

    // Case B: Check if domain is a subdomain of an existing domain
    const sortedDomains = Object.keys(domains).sort((a, b) => b.length - a.length)

    for (const parentDomain of sortedDomains) {
      if (domain.endsWith('.' + parentDomain) && domain !== parentDomain) {
        const sub = domain.slice(0, -(parentDomain.length + 1))
        const record = domains[parentDomain]

        if (record.subdomain && record.subdomain.includes(sub)) {
          // 1. Remove from subdomain list
          record.subdomain = record.subdomain.filter(s => s !== sub)

          // 2. Persist config
          Odac.core('Config').config.domains = domains

          // 3. Delete DNS record (CNAME)
          try {
            Odac.server('DNS').delete({name: domain, type: 'CNAME'})
            log('Deleted CNAME record for subdomain %s', domain)
          } catch (e) {
            error('Failed to delete DNS for subdomain %s: %s', domain, e.message)
          }

          // 4. Trigger SSL renew for parent to update cert
          try {
            Odac.server('SSL').renew(parentDomain)
          } catch (e) {
            error('Failed to trigger SSL renew for %s: %s', parentDomain, e.message)
          }

          // Sync proxy config (if subdomain removed)
          try {
            Odac.server('Proxy').syncConfig()
          } catch (err) {
            error('Proxy sync failed: %s', err.message)
          }

          return Odac.server('Api').result(true, __('Subdomain %s removed from %s.', sub, parentDomain))
        }
      }
    }

    return Odac.server('Api').result(false, __('Domain %s not found.', domain))
  }

  /**
   * Lists all registered domains.
   *
   * @param {string} [appId] - Optional: filter by App ID or name
   * @returns {Promise<{result: boolean, message: string}>} API result object
   */
  async list(appId) {
    // Sanitize appId input to handle string "undefined" or "null" or non-string inputs (like functions accidentally passed)
    if (typeof appId !== 'string') {
      appId = undefined
    } else {
      const normalized = appId.trim().toLowerCase()
      if (normalized === 'undefined' || normalized === 'null') {
        appId = undefined
      }
    }

    const domains = this.#getDomains()
    const domainKeys = Object.keys(domains)

    if (domainKeys.length === 0) {
      return Odac.server('Api').result(false, __('No domains found.'))
    }

    const filteredRecords = []

    // Transform to array for filtering and display
    for (const [domain, record] of Object.entries(domains)) {
      if (!appId || record.appId === appId) {
        filteredRecords.push({
          domain,
          ...record
        })
      }
    }

    if (filteredRecords.length === 0) {
      return Odac.server('Api').result(false, appId ? __('No domains found for app %s.', appId) : __('No domains found.'))
    }

    const formattedRecords = filteredRecords.map(d => ({
      domain: d.domain,
      subdomain: d.subdomain,
      app: d.appId,
      created: d.created
    }))

    // Return raw data for CLI/Hub to format
    return Odac.server('Api').result(true, formattedRecords)
  }
}

module.exports = new Domain()
