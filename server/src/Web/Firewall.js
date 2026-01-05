const {log} = Odac.core('Log', false).init('Firewall')

/**
 * Firewall class to handle IP blocking and rate limiting.
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
    // Load configuration from Odac.core('Config').config
    const config = Odac.core('Config').config.firewall || {}

    this.#config = {
      enabled: config.enabled !== false,
      rateLimit: {
        enabled: config.rateLimit?.enabled !== false,
        windowMs: config.rateLimit?.windowMs ?? 60000, // 1 minute
        max: config.rateLimit?.max ?? 300 // limit each IP to 300 requests per windowMs
      },
      maxWsPerIp: config.maxWsPerIp ?? 50, // Default 50 concurrent WS per IP
      blacklist: new Set(config.blacklist || []),
      whitelist: new Set(config.whitelist || [])
    }
  }

  /**
   * Check if a request should be allowed.
   * @param {Object} req - The HTTP request object.
   * @returns {Object} An object containing {allowed: boolean, reason?: string}.
   */
  check(req) {
    if (!this.#config.enabled) return {allowed: true}

    // Extract IP address safely, handling x-forwarded-for which can be a comma-separated list
    let ip = req.socket?.remoteAddress || req.headers['x-forwarded-for']?.split(',')[0]?.trim()

    // Normalize IPv6-mapped IPv4 addresses
    if (ip && ip.startsWith('::ffff:')) {
      ip = ip.substring(7)
    }

    // Note: Native IPv6 addresses are not fully normalized (e.g. :: vs 0:0...).
    // Blacklist/Whitelist entries for IPv6 should match the format provided by the socket (usually compressed).

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
    if (!Odac.core('Config').config.firewall) Odac.core('Config').config.firewall = {}

    Odac.core('Config').config.firewall.blacklist = Array.from(this.#config.blacklist)
    Odac.core('Config').config.firewall.whitelist = Array.from(this.#config.whitelist)
    // Config module handles saving automatically when properties change if using Proxy,
    // but here we are modifying the object structure.
    // Assuming Config module watches for changes or we need to trigger save.
    // Looking at Config.js, it uses Proxy to detect changes.
  }
}

module.exports = Firewall
