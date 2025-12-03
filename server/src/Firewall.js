const {log} = Candy.core('Log', false).init('Firewall')

/**
 * Firewall class to handle IP blocking and rate limiting for all services (Web, DNS, Mail, etc).
 */
class Firewall {
  #requestCounts = new Map() // IP -> { count, timestamp }
  #config = {}
  #cleanupInterval = null

  constructor() {
    this.load()
    // Run cleanup every minute
    this.#cleanupInterval = setInterval(() => this.cleanup(), 60000)
    if (this.#cleanupInterval.unref) this.#cleanupInterval.unref()
  }

  /**
   * Load configuration from the global Config module.
   */
  load() {
    // Load configuration from Candy.core('Config').config
    const config = Candy.core('Config').config.firewall || {}

    this.#config = {
      enabled: config.enabled !== false,
      rateLimit: {
        enabled: config.rateLimit?.enabled !== false,
        windowMs: config.rateLimit?.windowMs ?? 60000, // 1 minute
        max: config.rateLimit?.max ?? 300 // limit each IP to 300 requests per windowMs
      },
      blacklist: new Set(config.blacklist || []),
      whitelist: new Set(config.whitelist || [])
    }
  }

  /**
   * Check if an IP should be allowed.
   * @param {string} ip - The IP address to check.
   * @returns {Object} An object containing {allowed: boolean, reason?: string}.
   */
  check(ip) {
    if (!this.#config.enabled) return {allowed: true}

    // Normalize IPv6-mapped IPv4 addresses
    if (ip && ip.startsWith('::ffff:')) {
      ip = ip.substring(7)
    }

    if (!ip) return {allowed: true}

    // 1. Check whitelist (bypass everything)
    if (this.#config.whitelist.has(ip)) return {allowed: true}

    // 2. Check blacklist
    if (this.#config.blacklist.has(ip)) {
      log(`Blocked request from blacklisted IP: ${ip}`)
      return {allowed: false, reason: 'blacklist'}
    }

    // 3. Rate limiting
    if (this.#config.rateLimit.enabled) {
      // Memory protection: if map gets too big, clear it to prevent memory leak
      if (this.#requestCounts.size > 20000) {
        this.#requestCounts.clear()
        log('Firewall request counts cleared due to memory limit')
      }

      const now = Date.now()
      let record = this.#requestCounts.get(ip)

      if (!record) {
        record = {count: 0, timestamp: now}
        this.#requestCounts.set(ip, record)
      }

      // Check window
      if (now - record.timestamp > this.#config.rateLimit.windowMs) {
        // Reset window
        record.count = 1
        record.timestamp = now
      } else {
        record.count++
      }

      if (record.count > this.#config.rateLimit.max) {
        if (record.count === this.#config.rateLimit.max + 1) {
          log(`Rate limit exceeded for IP: ${ip}`)
        }
        return {allowed: false, reason: 'rate_limit'}
      }
    }

    return {allowed: true}
  }

  /**
   * Cleanup stale rate limit records.
   */
  cleanup() {
    if (!this.#config.rateLimit?.windowMs) return

    const now = Date.now()
    const windowMs = this.#config.rateLimit.windowMs

    for (const [ip, record] of this.#requestCounts) {
      if (now - record.timestamp > windowMs) {
        this.#requestCounts.delete(ip)
      }
    }
  }

  /**
   * Add an IP to the blacklist.
   * @param {string} ip - The IP address to block.
   */
  addBlock(ip) {
    if (this.#config.whitelist.has(ip)) this.#config.whitelist.delete(ip)
    this.#config.blacklist.add(ip)
    this.#save()
  }

  /**
   * Remove an IP from the blacklist.
   * @param {string} ip - The IP address to unblock.
   */
  removeBlock(ip) {
    this.#config.blacklist.delete(ip)
    this.#save()
  }

  /**
   * Add an IP to the whitelist.
   * @param {string} ip - The IP address to whitelist.
   */
  addWhitelist(ip) {
    if (this.#config.blacklist.has(ip)) this.#config.blacklist.delete(ip)
    this.#config.whitelist.add(ip)
    this.#save()
  }

  /**
   * Remove an IP from the whitelist.
   * @param {string} ip - The IP address to remove from whitelist.
   */
  removeWhitelist(ip) {
    this.#config.whitelist.delete(ip)
    this.#save()
  }

  #save() {
    // Update the global config
    if (!Candy.core('Config').config.firewall) Candy.core('Config').config.firewall = {}

    Candy.core('Config').config.firewall.blacklist = Array.from(this.#config.blacklist)
    Candy.core('Config').config.firewall.whitelist = Array.from(this.#config.whitelist)
  }
}

module.exports = Firewall
