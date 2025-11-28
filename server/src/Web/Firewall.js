const {log, error} = Candy.core('Log', false).init('Firewall')

class Firewall {
  #blacklist = new Set()
  #whitelist = new Set()
  #requestCounts = new Map() // IP -> { count, timestamp }
  #config = {}
  #cleanupInterval = null

  constructor() {
    this.load()
    // Run cleanup every minute
    this.#cleanupInterval = setInterval(() => this.cleanup(), 60000)
    if (this.#cleanupInterval.unref) this.#cleanupInterval.unref()
  }

  load() {
    // Load configuration from Candy.core('Config').config
    const config = Candy.core('Config').config.firewall || {}

    this.#config = {
      enabled: config.enabled !== false,
      rateLimit: {
        enabled: config.rateLimit?.enabled !== false,
        windowMs: config.rateLimit?.windowMs || 60000, // 1 minute
        max: config.rateLimit?.max || 300 // limit each IP to 300 requests per windowMs
      },
      blacklist: new Set(config.blacklist || []),
      whitelist: new Set(config.whitelist || [])
    }

    this.#blacklist = this.#config.blacklist
    this.#whitelist = this.#config.whitelist
  }

  check(req) {
    if (!this.#config.enabled) return true

    let ip = req.socket.remoteAddress || req.headers['x-forwarded-for']
    if (ip && ip.startsWith('::ffff:')) {
      ip = ip.substring(7)
    }

    if (!ip) return true

    // 1. Check whitelist (bypass everything)
    if (this.#whitelist.has(ip)) return true

    // 2. Check blacklist
    if (this.#blacklist.has(ip)) {
      log(`Blocked request from blacklisted IP: ${ip}`)
      return false
    }

    // 3. Rate limiting
    if (this.#config.rateLimit.enabled) {
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
        return false
      }
    }

    return true
  }

  // Cleanup method to run periodically
  cleanup() {
    const now = Date.now()
    const windowMs = this.#config.rateLimit.windowMs

    for (const [ip, record] of this.#requestCounts) {
      if (now - record.timestamp > windowMs) {
        this.#requestCounts.delete(ip)
      }
    }
  }

  addBlock(ip) {
      if (this.#whitelist.has(ip)) this.#whitelist.delete(ip)
      this.#blacklist.add(ip)
      this.#save()
  }

  removeBlock(ip) {
      this.#blacklist.delete(ip)
      this.#save()
  }

  addWhitelist(ip) {
      if (this.#blacklist.has(ip)) this.#blacklist.delete(ip)
      this.#whitelist.add(ip)
      this.#save()
  }

  removeWhitelist(ip) {
      this.#whitelist.delete(ip)
      this.#save()
  }

  #save() {
      // Update the global config
      if (!Candy.core('Config').config.firewall) Candy.core('Config').config.firewall = {}

      Candy.core('Config').config.firewall.blacklist = Array.from(this.#blacklist)
      Candy.core('Config').config.firewall.whitelist = Array.from(this.#whitelist)
      // Config module handles saving automatically when properties change if using Proxy,
      // but here we are modifying the object structure.
      // Assuming Config module watches for changes or we need to trigger save.
      // Looking at Config.js, it uses Proxy to detect changes.
  }
}

module.exports = Firewall
