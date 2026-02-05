const {log, error} = Odac.core('Log', false).init('DNS')

const axios = require('axios')
const dns = require('native-dns')
const {execSync} = require('child_process')
const fs = require('fs')
const nodeDns = require('dns')
const os = require('os')
const {randomUUID} = require('crypto')

class DNS {
  ips = {ipv4: [], ipv6: []} // Arrays of {address, ptr} objects
  ip = '127.0.0.1' // Primary IPv4 for backward compatibility
  #loaded = false
  #ptrCache = new Map() // PTR -> {address, type} for O(1) lookup
  #tcp

  #udp
  #requestCount = new Map() // Rate limiting
  #rateLimit = 2500 // requests per minute per IP
  #rateLimitWindow = 60000 // 1 minute

  // Private IPv4 ranges (RFC 1918, RFC 6598, Link-local)
  #privateIPv4Ranges = [
    {start: 0x0a000000, end: 0x0affffff}, // 10.0.0.0/8
    {start: 0xac100000, end: 0xac1fffff}, // 172.16.0.0/12
    {start: 0xc0a80000, end: 0xc0a8ffff}, // 192.168.0.0/16
    {start: 0xa9fe0000, end: 0xa9feffff}, // 169.254.0.0/16 (link-local)
    {start: 0x64400000, end: 0x647fffff}, // 100.64.0.0/10 (CGNAT)
    {start: 0x7f000000, end: 0x7fffffff} // 127.0.0.0/8 (loopback)
  ]

  #execHost(cmd, options = {}) {
    // Check if running in Docker
    const isDocker = fs.existsSync('/.dockerenv')

    if (isDocker) {
      // Strip sudo strings, assuming we are root in container and joining as root on host
      const cleanCmd = cmd.replace(/sudo\s+/g, '')
      // Use nsenter to execute on host (PID 1)
      // Requires pid: host, privileged: true in docker-compose
      const nsenterCmd = `nsenter -t 1 -m -u -n -i sh -c "${cleanCmd.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`
      return execSync(nsenterCmd, {...options, encoding: 'utf8'})
    }

    return execSync(cmd, options)
  }

  delete(...args) {
    if (!Odac.core('Config').config.dns) return

    let changedDomains = new Set()

    for (let obj of args) {
      let domain = obj.name
      // Find the root domain
      while (!Odac.core('Config').config.dns[domain] && domain.includes('.')) {
        domain = domain.split('.').slice(1).join('.')
      }

      if (!Odac.core('Config').config.dns[domain]) continue
      if (!obj.type) continue

      const type = obj.type.toUpperCase()
      const zone = Odac.core('Config').config.dns[domain]

      const initialLength = zone.records.length
      zone.records = zone.records.filter(
        record => !(record.type === type && record.name === obj.name && (!obj.value || record.value === obj.value))
      )

      if (zone.records.length !== initialLength) {
        changedDomains.add(domain)
      }
    }

    // Update SOA serial for changed zones
    for (const domain of changedDomains) {
      this.#updateSOASerial(domain)
    }

    if (changedDomains.size > 0) {
      if (Odac.core('Config').force) Odac.core('Config').force()
    }
  }

  init() {
    this.#udp = dns.createServer()
    this.#tcp = dns.createTCPServer()
    // Patch native-dns TCP server to handle connection errors
    if (this.#tcp._socket) {
      this.#tcp._socket.on('connection', socket => {
        socket.on('error', err => {
          if (err.code !== 'ECONNRESET') error('DNS TCP Socket Error:', err.message)
        })
      })
    }

    // MIGRATION: Move DNS records from websites to dns config
    this.#migrateDNS()
  }

  #migrateDNS() {
    const config = Odac.core('Config').config
    if (!config.websites) return

    // Initialize dns config if missing
    if (!config.dns) config.dns = {}

    let migrationNeeded = false
    const dnsConfig = config.dns

    // Check websites for legacy DNS records
    for (const domain in config.websites) {
      const site = config.websites[domain]
      if (site.DNS && !dnsConfig[domain]) {
        migrationNeeded = true
        log(`Migrating DNS records for ${domain}...`)

        // Create new zone structure
        const zone = {
          soa: {},
          records: []
        }

        // Migrate SOA
        if (site.DNS.SOA && site.DNS.SOA[0]) {
          const oldSoa = site.DNS.SOA[0]
          const parts = oldSoa.value.split(' ')
          zone.soa = {
            primary: parts[0] || `ns1.${domain}`,
            email: parts[1] || `hostmaster.${domain}`,
            serial:
              parseInt(parts[2]) ||
              parseInt(
                new Date()
                  .toISOString()
                  .replace(/[^0-9]/g, '')
                  .slice(0, 8) + '01'
              ),
            refresh: parseInt(parts[3]) || 3600,
            retry: parseInt(parts[4]) || 600,
            expire: parseInt(parts[5]) || 604800,
            minimum: parseInt(parts[6]) || 3600,
            ttl: oldSoa.ttl || 3600
          }
        } else {
          // Create default SOA if missing
          const dateStr = new Date()
            .toISOString()
            .replace(/[^0-9]/g, '')
            .slice(0, 8)
          zone.soa = {
            primary: `ns1.${domain}`,
            email: `hostmaster.${domain}`,
            serial: parseInt(dateStr + '01'),
            refresh: 3600,
            retry: 600,
            expire: 604800,
            minimum: 3600,
            ttl: 3600
          }
        }

        // Migrate other records
        const recordTypes = ['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS', 'CAA']
        for (const type of recordTypes) {
          if (site.DNS[type] && Array.isArray(site.DNS[type])) {
            for (const rec of site.DNS[type]) {
              zone.records.push({
                id: randomUUID(),
                type: type,
                name: rec.name,
                value: rec.value,
                priority: rec.priority, // Only for MX
                ttl: rec.ttl || 3600
              })
            }
          }
        }

        dnsConfig[domain] = zone
        // Remove legacy DNS data
        delete site.DNS
      }
    }

    if (migrationNeeded) {
      if (Odac.core('Config').force) Odac.core('Config').force()
      log('DNS migration completed successfully.')
    }
  }

  start() {
    if (this.#loaded) return
    this.#getExternalIP()
    this.#publish()
  }

  stop() {
    try {
      if (this.#udp) {
        this.#udp.close()
      }
      if (this.#tcp) {
        this.#tcp.close()
      }
      this.#loaded = false
    } catch (e) {
      error('Error stopping DNS services: %s', e.message)
    }
  }

  async #getExternalIP() {
    // Collect all IPs from local interfaces first
    this.#collectLocalIPs()

    // IPv4 detection services
    const ipv4Services = [
      'https://curlmyip.org/',
      'https://ipv4.icanhazip.com/',
      'https://api.ipify.org/',
      'https://checkip.amazonaws.com/',
      'https://ipinfo.io/ip'
    ]

    // IPv6 detection services
    const ipv6Services = ['https://ipv6.icanhazip.com/', 'https://api64.ipify.org/', 'https://v6.ident.me/']

    // Detect external IPv4
    for (const service of ipv4Services) {
      try {
        log(`Attempting to get external IPv4 from ${service}`)
        const response = await axios.get(service, {
          timeout: 5000,
          headers: {'User-Agent': 'Odac-DNS/1.0'}
        })

        const ip = response.data.trim()
        if (ip && /^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(ip)) {
          log('External IPv4 detected:', ip)
          // Add to IPv4 list if not already present - external IPs are always public
          if (!this.ips.ipv4.find(i => i.address === ip)) {
            this.ips.ipv4.unshift({address: ip, ptr: null, public: true}) // External IP first
          }
          this.ip = ip // Set primary IP
          break
        } else {
          log(`Invalid IPv4 format from ${service}:`, ip)
        }
      } catch (err) {
        log(`Failed to get IPv4 from ${service}:`, err.message)
        continue
      }
    }

    // Detect external IPv6
    for (const service of ipv6Services) {
      try {
        log(`Attempting to get external IPv6 from ${service}`)
        const response = await axios.get(service, {
          timeout: 5000,
          headers: {'User-Agent': 'Odac-DNS/1.0'},
          family: 6 // Force IPv6
        })

        const ip = response.data.trim()
        // Basic IPv6 validation
        if (ip && /^[0-9a-fA-F:]+$/.test(ip) && ip.includes(':')) {
          log('External IPv6 detected:', ip)
          if (!this.ips.ipv6.find(i => i.address === ip)) {
            this.ips.ipv6.unshift({address: ip, ptr: null, public: true}) // External IP first
          }
          break
        } else {
          log(`Invalid IPv6 format from ${service}:`, ip)
        }
      } catch (err) {
        log(`Failed to get IPv6 from ${service}:`, err.message)
        continue
      }
    }

    // Use first local IPv4 as fallback if no external IP detected
    if (this.ips.ipv4.length === 0) {
      log('Could not determine external IPv4, using default 127.0.0.1')
      error('DNS', 'All IPv4 detection methods failed, DNS A records will use 127.0.0.1')
    } else {
      // Use first public IPv4 as primary, fallback to first available
      const publicIPv4 = this.ips.ipv4.find(i => i.public)
      this.ip = publicIPv4 ? publicIPv4.address : this.ips.ipv4[0].address
    }

    // Perform PTR lookups for all detected IPs
    await this.#lookupPTRRecords()

    log(
      `Detected IPs - IPv4: [${this.ips.ipv4.map(i => `${i.address}${i.public ? ' [public]' : ' [private]'}${i.ptr ? ` (${i.ptr})` : ''}`).join(', ')}]`
    )
    log(
      `Detected IPs - IPv6: [${this.ips.ipv6.map(i => `${i.address}${i.public ? ' [public]' : ' [private]'}${i.ptr ? ` (${i.ptr})` : ''}`).join(', ')}]`
    )
  }

  /**
   * Performs PTR (reverse DNS) lookups for all detected IP addresses
   * Updates ips.ipv4 and ips.ipv6 arrays with ptr field
   */
  async #lookupPTRRecords() {
    const lookupPromises = []

    // Lookup IPv4 PTR records
    for (let i = 0; i < this.ips.ipv4.length; i++) {
      const ipObj = this.ips.ipv4[i]
      lookupPromises.push(
        this.#reverseLookup(ipObj.address)
          .then(ptr => {
            this.ips.ipv4[i].ptr = ptr
            if (ptr) log(`PTR record for ${ipObj.address}: ${ptr}`)
          })
          .catch(() => {
            this.ips.ipv4[i].ptr = null
          })
      )
    }

    // Lookup IPv6 PTR records
    for (let i = 0; i < this.ips.ipv6.length; i++) {
      const ipObj = this.ips.ipv6[i]
      lookupPromises.push(
        this.#reverseLookup(ipObj.address)
          .then(ptr => {
            this.ips.ipv6[i].ptr = ptr
            if (ptr) log(`PTR record for ${ipObj.address}: ${ptr}`)
          })
          .catch(() => {
            this.ips.ipv6[i].ptr = null
          })
      )
    }

    // Wait for all lookups with timeout
    await Promise.race([
      Promise.allSettled(lookupPromises),
      new Promise(resolve => setTimeout(resolve, 5000)) // 5 second timeout for all PTR lookups
    ])

    // Build PTR cache for O(1) lookup during DNS queries
    this.#buildPTRCache()
  }

  /**
   * Builds the PTR cache Map from collected IP addresses
   * Called once after PTR lookups complete
   */
  #buildPTRCache() {
    this.#ptrCache.clear()

    // Cache IPv4 PTR records
    for (const ipObj of this.ips.ipv4) {
      if (ipObj.ptr) {
        this.#ptrCache.set(ipObj.ptr, {address: ipObj.address, type: 'ipv4'})
      }
    }

    // Cache IPv6 PTR records
    for (const ipObj of this.ips.ipv6) {
      if (ipObj.ptr) {
        this.#ptrCache.set(ipObj.ptr, {address: ipObj.address, type: 'ipv6'})
      }
    }

    log(`PTR cache built with ${this.#ptrCache.size} entries`)
  }

  /**
   * Performs a reverse DNS lookup for an IP address
   * @param {string} ip - IPv4 or IPv6 address
   * @returns {Promise<string|null>} - Hostname or null if not found
   */
  async #reverseLookup(ip) {
    try {
      const hostnames = await nodeDns.promises.reverse(ip)
      return hostnames && hostnames.length > 0 ? hostnames[0] : null
    } catch {
      // ENOTFOUND, ENODATA, or other DNS errors mean no PTR record
      return null
    }
  }

  /**
   * Checks if an IPv4 address is in a private range
   * @param {string} ip - IPv4 address
   * @returns {boolean} - true if private, false if public
   */
  #isPrivateIPv4(ip) {
    const parts = ip.split('.').map(Number)
    if (parts.length !== 4) return false

    // Convert to 32-bit integer for range comparison
    const ipNum = (parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]

    for (const range of this.#privateIPv4Ranges) {
      if (ipNum >= range.start && ipNum <= range.end) {
        return true
      }
    }
    return false
  }

  /**
   * Checks if an IPv6 address is private/link-local
   * @param {string} ip - IPv6 address
   * @returns {boolean} - true if private, false if public
   */
  #isPrivateIPv6(ip) {
    const normalized = ip.toLowerCase()
    // Link-local (fe80::)
    if (normalized.startsWith('fe80:')) return true
    // Unique local (fc00::/7 - fc00:: to fdff::)
    if (normalized.startsWith('fc') || normalized.startsWith('fd')) return true
    // Loopback (::1)
    if (normalized === '::1') return true
    return false
  }

  /**
   * Collects all non-internal IP addresses from local network interfaces
   * Populates this.ips.ipv4 and this.ips.ipv6 arrays
   */
  #collectLocalIPs() {
    try {
      const networkInterfaces = require('os').networkInterfaces()
      for (const interfaceName in networkInterfaces) {
        const interfaces = networkInterfaces[interfaceName]
        for (const iface of interfaces) {
          // Skip loopback and internal addresses
          if (iface.internal) continue

          if (iface.family === 'IPv4') {
            if (!this.ips.ipv4.find(i => i.address === iface.address)) {
              const isPublic = !this.#isPrivateIPv4(iface.address)
              this.ips.ipv4.push({address: iface.address, ptr: null, public: isPublic})
              log(`Local IPv4 detected on ${interfaceName}: ${iface.address} [${isPublic ? 'public' : 'private'}]`)
            }
          } else if (iface.family === 'IPv6') {
            // Skip link-local addresses (fe80::) - already handled in isPrivateIPv6
            if (iface.address.startsWith('fe80:')) continue
            if (!this.ips.ipv6.find(i => i.address === iface.address)) {
              const isPublic = !this.#isPrivateIPv6(iface.address)
              this.ips.ipv6.push({address: iface.address, ptr: null, public: isPublic})
              log(`Local IPv6 detected on ${interfaceName}: ${iface.address} [${isPublic ? 'public' : 'private'}]`)
            }
          }
        }
      }
    } catch (err) {
      error('Failed to collect local network IPs:', err.message)
    }
  }

  #publish() {
    if (this.#loaded) return
    // Ensure we have dns config
    if (!Odac.core('Config').config.dns) Odac.core('Config').config.dns = {}

    this.#loaded = true

    // Set up request handlers
    this.#udp.on('request', (request, response) => {
      try {
        this.#request(request, response)
      } catch (err) {
        error('DNS UDP request handler error:', err.message)
      }
    })
    this.#tcp.on('request', (request, response) => {
      try {
        this.#request(request, response)
      } catch (err) {
        error('DNS TCP request handler error:', err.message)
      }
    })

    // Log system information before starting
    this.#logSystemInfo()

    this.#startDNSServers()
  }

  #logSystemInfo() {
    try {
      log('DNS Server initialization - System information:')
      log('Platform:', os.platform())
      log('Architecture:', os.arch())

      // Check what's using port 53
      try {
        const port53Info = this.#execHost(
          '(lsof -i :53 2>/dev/null || (netstat -tulpn 2>/dev/null | grep :53) || (ss -tulpn 2>/dev/null | grep :53)) || echo "Port 53 appears to be free"',
          {
            encoding: 'utf8',
            timeout: 5000
          }
        )
        log('Port 53 status:', port53Info.trim())
      } catch (err) {
        log('Could not check port 53 status:', err.message)
      }

      // Check systemd-resolved status on Linux
      if (os.platform() === 'linux') {
        try {
          const resolvedStatus = this.#execHost('systemctl is-active systemd-resolved 2>/dev/null || echo "not-active"', {
            encoding: 'utf8',
            timeout: 3000
          }).trim()
          log('systemd-resolved status:', resolvedStatus)

          if (resolvedStatus === 'active') {
            try {
              const resolvedConfig = this.#execHost(
                'systemd-resolve --status 2>/dev/null | head -20 || resolvectl status 2>/dev/null | head -20 || echo "Could not get resolver status"',
                {
                  encoding: 'utf8',
                  timeout: 3000
                }
              )
              log('Current DNS resolver configuration:', resolvedConfig.trim())
            } catch {
              log('Could not get DNS resolver configuration')
            }
          }
        } catch (err) {
          log('Could not check systemd-resolved status:', err.message)
        }
      }
    } catch (err) {
      log('Error logging system info:', err.message)
    }
  }

  async #startDNSServers() {
    // First, proactively check if port 53 is available
    const portAvailable = await this.#checkPortAvailability(53)
    if (!portAvailable) {
      log('Port 53 is already in use, attempting to resolve conflict...')
      const resolved = await this.#handleSystemdResolveConflict()
      if (resolved) {
        // Wait a bit and retry
        setTimeout(() => this.#attemptDNSStart(53), 3000)
      } else {
        log('Could not resolve port 53 conflict, using alternative port...')
        this.#useAlternativePort()
      }
      return
    }

    // Port seems available, try to start
    this.#attemptDNSStart(53)
  }

  #attemptDNSStart(port) {
    try {
      // Set up error handlers before starting
      this.#udp.on('error', async err => {
        error('DNS UDP Server Error:', err.message)
        if (err.code === 'EADDRINUSE' || err.code === 'EACCES') {
          log(`Port ${port} conflict detected via error event, attempting resolution...`)
          if (port === 53) {
            const resolved = await this.#handleSystemdResolveConflict()
            if (!resolved) {
              this.#useAlternativePort()
            }
          }
        }
      })

      this.#tcp.on('error', async err => {
        error('DNS TCP Server Error:', err.message)
        if (err.code === 'EADDRINUSE' || err.code === 'EACCES') {
          log(`Port ${port} conflict detected via error event, attempting resolution...`)
          if (port === 53) {
            const resolved = await this.#handleSystemdResolveConflict()
            if (!resolved) {
              this.#useAlternativePort()
            }
          }
        }
      })

      // Try to start servers
      this.#udp.serve(port)
      this.#tcp.serve(port)
      log(`DNS servers started on port ${port}`)

      // Update system DNS configuration for internet access
      if (port === 53) {
        this.#setupSystemDNSForInternet()
      }
    } catch (err) {
      error('Failed to start DNS servers:', err.message)
      if (err.code === 'EADDRINUSE' || err.code === 'EACCES') {
        log(`Port ${port} is in use (caught exception), attempting resolution...`)
        if (port === 53) {
          this.#handleSystemdResolveConflict().then(resolved => {
            if (resolved) {
              setTimeout(() => this.#attemptDNSStart(53), 3000)
            } else {
              this.#useAlternativePort()
            }
          })
        } else {
          this.#useAlternativePort()
        }
      }
    }
  }

  async #checkPortAvailability(port) {
    try {
      // Check if anything is listening on the port
      const portCheck = this.#execHost(
        `lsof -i :${port} 2>/dev/null || netstat -tulpn 2>/dev/null | grep :${port} || ss -tulpn 2>/dev/null | grep :${port} || true`,
        {
          encoding: 'utf8',
          timeout: 5000
        }
      )

      if (portCheck.trim()) {
        log(`Port ${port} is in use by:`, portCheck.trim())
        return false
      }

      return true
    } catch (err) {
      log('Error checking port availability:', err.message)
      return false
    }
  }

  async #handleSystemdResolveConflict() {
    try {
      // Check if we're on Linux
      if (os.platform() !== 'linux') {
        log('Not on Linux, skipping systemd-resolve conflict resolution')
        return false
      }

      // More comprehensive check for what's using port 53
      let portInfo = ''
      try {
        portInfo = this.#execHost(
          'lsof -i :53 2>/dev/null || netstat -tulpn 2>/dev/null | grep :53 || ss -tulpn 2>/dev/null | grep :53 || true',
          {
            encoding: 'utf8',
            timeout: 5000
          }
        )
      } catch (err) {
        log('Could not check port 53 usage:', err.message)
        return false
      }

      if (!portInfo || (!portInfo.includes('systemd-resolve') && !portInfo.includes('resolved'))) {
        log('systemd-resolve not detected on port 53, conflict may be with another service')
        return false
      }

      log('Detected systemd-resolve using port 53, attempting resolution...')

      // Try the direct approach first - disable DNS stub listener
      const stubDisabled = await this.#disableSystemdResolveStub()
      if (stubDisabled) {
        return true
      }

      // If that fails, try alternative approach
      return this.#tryAlternativeApproach()
    } catch (err) {
      error('Error handling systemd-resolve conflict:', err.message)
      return false
    }
  }

  async #disableSystemdResolveStub() {
    try {
      log('Attempting to disable systemd-resolved DNS stub listener...')

      // Check if systemd-resolved is active
      const isActive = this.#execHost('systemctl is-active systemd-resolved 2>/dev/null || echo inactive', {
        encoding: 'utf8',
        timeout: 5000
      }).trim()

      if (isActive !== 'active') {
        log('systemd-resolved is not active')
        return false
      }

      // Create or update resolved.conf to disable DNS stub
      const resolvedConfDir = '/etc/systemd/resolved.conf.d'
      const resolvedConfFile = `${resolvedConfDir}/odac-dns.conf`

      try {
        // Ensure directory exists
        if (!fs.existsSync(resolvedConfDir)) {
          this.#execHost(`sudo mkdir -p ${resolvedConfDir}`, {timeout: 10000})
        }

        // Create configuration to disable DNS stub listener and use public DNS
        const resolvedConfig = `[Resolve]
DNSStubListener=no
DNS=1.1.1.1 1.0.0.1 8.8.8.8 8.8.4.4
FallbackDNS=1.1.1.1 1.0.0.1
`

        this.#execHost(`echo '${resolvedConfig}' | sudo tee ${resolvedConfFile}`, {timeout: 10000})
        log('Created systemd-resolved configuration to disable DNS stub listener')

        // Restart systemd-resolved
        this.#execHost('sudo systemctl restart systemd-resolved', {timeout: 15000})
        log('Restarted systemd-resolved service')

        // Wait for service to restart and port to be freed
        return new Promise(resolve => {
          setTimeout(() => {
            try {
              // Check if port 53 is now free
              const portCheck = this.#execHost('lsof -i :53 2>/dev/null || true', {
                encoding: 'utf8',
                timeout: 3000
              })

              if (!portCheck.includes('systemd-resolve') && !portCheck.includes('resolved')) {
                log('Port 53 is now available')
                resolve(true)
              } else {
                log('Port 53 still in use, trying alternative approach')
                resolve(this.#tryAlternativeApproach())
              }
            } catch (err) {
              log('Error checking port availability:', err.message)
              resolve(this.#tryAlternativeApproach())
            }
          }, 3000)
        })
      } catch (sudoErr) {
        log('Could not configure systemd-resolved (no sudo access):', sudoErr.message)
        return false
      }
    } catch (err) {
      log('Error disabling systemd-resolved stub:', err.message)
      return false
    }
  }

  #tryAlternativeApproach() {
    try {
      log('Trying alternative approach: temporarily stopping systemd-resolved...')

      // Check if we can stop systemd-resolved
      try {
        this.#execHost('sudo systemctl stop systemd-resolved', {timeout: 10000})
        log('Temporarily stopped systemd-resolved')

        // Set up cleanup handlers to restart systemd-resolved when process exits
        this.#setupCleanupHandlers()

        return true
      } catch (stopErr) {
        log('Could not stop systemd-resolved:', stopErr.message)

        // Last resort: try to use a different port for our DNS server
        return this.#useAlternativePort()
      }
    } catch (err) {
      log('Alternative approach failed:', err.message)
      return false
    }
  }

  #setupCleanupHandlers() {
    const restartSystemdResolved = () => {
      try {
        this.#execHost('sudo systemctl start systemd-resolved', {timeout: 10000})
        log('Restarted systemd-resolved on cleanup')
      } catch (err) {
        error('Failed to restart systemd-resolved on cleanup:', err.message)
      }
    }

    // Handle various exit scenarios
    process.on('exit', restartSystemdResolved)
    process.on('SIGINT', () => {
      restartSystemdResolved()
      process.exit(0)
    })
    process.on('SIGTERM', () => {
      restartSystemdResolved()
      process.exit(0)
    })
    process.on('uncaughtException', err => {
      error('Uncaught exception:', err.message)
      restartSystemdResolved()
      process.exit(1)
    })
    process.on('unhandledRejection', (reason, promise) => {
      error('Unhandled rejection at:', promise, 'reason:', reason)
      restartSystemdResolved()
      process.exit(1)
    })

    log('Set up cleanup handlers to restart systemd-resolved on exit')
  }

  async #useAlternativePort() {
    try {
      log('Attempting to use alternative port for DNS server...')

      // Try ports 5353, 1053, 8053 as alternatives
      const alternativePorts = [5353, 1053, 8053]

      for (const port of alternativePorts) {
        const available = await this.#checkPortAvailability(port)
        if (available) {
          try {
            // Create new server instances for alternative port
            const udpAlt = dns.createServer()
            const tcpAlt = dns.createTCPServer()

            // Copy event handlers
            udpAlt.on('request', (request, response) => {
              try {
                this.#request(request, response)
              } catch (err) {
                error('DNS UDP request handler error:', err.message)
              }
            })

            tcpAlt.on('request', (request, response) => {
              try {
                this.#request(request, response)
              } catch (err) {
                error('DNS TCP request handler error:', err.message)
              }
            })

            udpAlt.on('error', err => error('DNS UDP Server Error (alt port):', err.stack))
            tcpAlt.on('error', err => error('DNS TCP Server Error (alt port):', err.stack))

            // Start on alternative port
            udpAlt.serve(port)
            tcpAlt.serve(port)

            // Replace original servers
            this.#udp = udpAlt
            this.#tcp = tcpAlt

            log(`DNS servers started on alternative port ${port}`)

            // Update system to use our alternative port
            this.#updateSystemDNSConfig(port)
            return true
          } catch (portErr) {
            log(`Failed to start on port ${port}:`, portErr.message)
            continue
          }
        } else {
          log(`Port ${port} is also in use, trying next...`)
          continue
        }
      }

      error('All alternative ports are in use')
      return false
    } catch (err) {
      error('Failed to use alternative port:', err.message)
      return false
    }
  }

  #setupSystemDNSForInternet() {
    try {
      // Configure system to use public DNS for internet access
      const resolvConf = `# Odac DNS Configuration
# Odac handles local domains on port 53
# Public DNS servers handle all internet domains

nameserver 1.1.1.1
nameserver 1.0.0.1
nameserver 8.8.8.8
nameserver 8.8.4.4

# Cloudflare DNS (1.1.1.1) - Fast and privacy-focused
# Google DNS (8.8.8.8) - Reliable fallback
# Original configuration backed up to /etc/resolv.conf.odac.backup
`

      // Backup original resolv.conf
      this.#execHost('sudo cp /etc/resolv.conf /etc/resolv.conf.odac.backup 2>/dev/null || true', {timeout: 5000})

      // Update resolv.conf with public DNS servers
      this.#execHost(`echo '${resolvConf}' | sudo tee /etc/resolv.conf`, {timeout: 5000})
      log('Configured system to use public DNS servers for internet access')
      log('Cloudflare DNS (1.1.1.1) and Google DNS (8.8.8.8) will handle non-Odac domains')

      // Set up restoration on exit
      process.on('exit', () => {
        try {
          this.#execHost('sudo mv /etc/resolv.conf.odac.backup /etc/resolv.conf 2>/dev/null || true', {timeout: 5000})
        } catch {
          // Silent fail on exit
        }
      })
    } catch (err) {
      log('Warning: Could not configure system DNS for internet access:', err.message)
    }
  }

  #updateSystemDNSConfig(port) {
    try {
      // Use reliable public DNS servers for internet access
      // Odac DNS only handles local domains, everything else goes to public DNS
      const resolvConf = `# Odac DNS Configuration
# Local domains handled by Odac DNS on port ${port}
# All other domains handled by reliable public DNS servers

nameserver 1.1.1.1
nameserver 1.0.0.1
nameserver 8.8.8.8
nameserver 8.8.4.4

# Cloudflare DNS (1.1.1.1) - Fast and privacy-focused
# Google DNS (8.8.8.8) - Reliable fallback
# Original configuration backed up to /etc/resolv.conf.odac.backup
`

      // Backup original resolv.conf
      this.#execHost('sudo cp /etc/resolv.conf /etc/resolv.conf.odac.backup 2>/dev/null || true', {timeout: 5000})

      // Update resolv.conf with public DNS servers
      this.#execHost(`echo '${resolvConf}' | sudo tee /etc/resolv.conf`, {timeout: 5000})
      log('Updated /etc/resolv.conf to use reliable public DNS servers (1.1.1.1, 8.8.8.8)')
      log('Odac domains will be handled locally, all other domains via public DNS')

      // Set up restoration on exit
      process.on('exit', () => {
        try {
          this.#execHost('sudo mv /etc/resolv.conf.odac.backup /etc/resolv.conf 2>/dev/null || true', {timeout: 5000})
        } catch {
          // Silent fail on exit
        }
      })
    } catch (err) {
      log('Warning: Could not update system DNS configuration:', err.message)
    }
  }

  #request(request, response) {
    try {
      // Basic rate limiting (skip for localhost)
      const clientIP = request.address?.address || 'unknown'
      const now = Date.now()

      // Skip rate limiting for localhost/loopback addresses
      if (clientIP !== '127.0.0.1' && clientIP !== '::1' && clientIP !== 'localhost') {
        if (!this.#requestCount.has(clientIP)) {
          this.#requestCount.set(clientIP, {count: 1, firstRequest: now})
        } else {
          const clientData = this.#requestCount.get(clientIP)
          if (now - clientData.firstRequest > this.#rateLimitWindow) {
            // Reset window
            this.#requestCount.set(clientIP, {count: 1, firstRequest: now})
          } else {
            clientData.count++
            if (clientData.count > this.#rateLimit) {
              log(`Rate limit exceeded for ${clientIP}`)
              return response.send()
            }
          }
        }
      }

      // Validate request structure
      if (!request || !response || !response.question || !response.question[0]) {
        log(`Invalid DNS request structure from ${clientIP}`)
        return response.send()
      }

      const questionName = response.question[0].name.toLowerCase()
      const questionType = response.question[0].type
      response.question[0].name = questionName

      // Resolve Zone
      let domain = questionName
      // Try exact match first, then walk up the tree
      while (!Odac.core('Config').config.dns[domain] && domain.includes('.')) {
        domain = domain.split('.').slice(1).join('.')
      }

      const zone = Odac.core('Config').config.dns[domain]

      if (!zone) {
        // For unknown domains, send search on public DNS? No, we are authoritative only for our zones.
        // Or if recursion is enabled (it's not), we would forward.
        // Send NXDOMAIN
        response.header.rcode = dns.consts.NAME_TO_RCODE.NXDOMAIN
        return response.send()
      }

      const records = zone.records || []
      const soa = zone.soa

      // SECURITY: Handle ANY queries strictly
      // Refuse ANY queries to prevent amplification attacks (RFC 8482 approach or just minimal response)
      if (questionType === dns.consts.NAME_TO_QTYPE.ANY) {
        // Return only HINFO or just SOA to minimize response size
        if (soa) {
          this.#processSOARecordObj(soa, domain, response)
        }
        return response.send()
      }

      // Only process records relevant to the question type for better performance
      switch (questionType) {
        case dns.consts.NAME_TO_QTYPE.A:
          this.#processARecords(
            records.filter(r => r?.type === 'A'),
            questionName,
            response
          )
          break
        case dns.consts.NAME_TO_QTYPE.AAAA:
          this.#processAAAARecords(
            records.filter(r => r?.type === 'AAAA'),
            questionName,
            response
          )
          break
        case dns.consts.NAME_TO_QTYPE.CNAME:
          this.#processCNAMERecords(
            records.filter(r => r?.type === 'CNAME'),
            questionName,
            response
          )
          break
        case dns.consts.NAME_TO_QTYPE.MX:
          this.#processMXRecords(
            records.filter(r => r?.type === 'MX'),
            questionName,
            response
          )
          break
        case dns.consts.NAME_TO_QTYPE.TXT:
          this.#processTXTRecords(
            records.filter(r => r?.type === 'TXT'),
            questionName,
            response
          )
          break
        case dns.consts.NAME_TO_QTYPE.NS:
          this.#processNSRecords(
            records.filter(r => r?.type === 'NS'),
            questionName,
            response,
            domain
          )
          break
        case dns.consts.NAME_TO_QTYPE.SOA:
          if (soa) {
            this.#processSOARecordObj(soa, domain, response)
          }
          break
        case dns.consts.NAME_TO_QTYPE.CAA: {
          const caaRecords = records.filter(r => r?.type === 'CAA')
          this.#processCAARecords(caaRecords, questionName, response)
          // If no CAA records found, add default Let's Encrypt CAA records
          if (response.answer.length === 0 && caaRecords.length === 0) {
            this.#addDefaultCAARecords(questionName, response)
          }
          break
        }
        default:
          // For unknown types, do nothing (NODATA)
          // Do NOT dump all records
          break
      }

      response.send()
    } catch (err) {
      error('DNS request processing error:', err.message)
      // Log client info for debugging
      const clientIP = request?.address?.address || 'unknown'
      log(`Error processing DNS request from ${clientIP}`)

      // Try to send an empty response if possible
      try {
        if (response && typeof response.send === 'function') {
          response.send()
        }
      } catch (sendErr) {
        error('Failed to send DNS error response:', sendErr.message)
      }
    }
  }

  // Helper to process SOA from object instead of record array
  #processSOARecordObj(soa, domain, response) {
    try {
      response.header.aa = 1
      response.answer.push(
        dns.SOA({
          name: domain,
          primary: soa.primary,
          admin: soa.email,
          serial: soa.serial,
          refresh: soa.refresh,
          retry: soa.retry,
          expiration: soa.expire,
          minimum: soa.minimum || 3600,
          ttl: soa.ttl || 3600
        })
      )
    } catch (err) {
      error('Error processing SOA object:', err.message)
    }
  }

  #processARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.answer.push(
          dns.A({
            name: record.name,
            address: record.value ?? this.#resolveIPByPTR(questionName, 'ipv4'),
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing A records:', err.message)
    }
  }

  /**
   * Resolves an IP address by matching PTR record to the given domain
   * Priority: 1. Exact match, 2. Subdomain match, 3. Root domain match
   * @param {string} domain - Domain to match against PTR records
   * @param {string} type - 'ipv4' or 'ipv6'
   * @returns {string} - Matching IP address or default IP
   */
  #resolveIPByPTR(domain, type = 'ipv4') {
    const ipList = type === 'ipv6' ? this.ips.ipv6 : this.ips.ipv4
    const queryRoot = this.#getRootDomain(domain)

    // Default: first PUBLIC IPv4 or first PUBLIC IPv6
    const publicIP = ipList.find(i => i.public)
    const defaultIP = type === 'ipv6' ? (publicIP ? publicIP.address : null) : publicIP ? publicIP.address : this.ip

    // O(1) exact match lookup from cache
    const cached = this.#ptrCache.get(domain)
    if (cached && (type === 'ipv4' ? cached.type === 'ipv4' : cached.type === 'ipv6')) {
      const cachedIPObj = ipList.find(i => i.address === cached.address)
      if (cachedIPObj && cachedIPObj.public) return cached.address
    }

    // O(n) scan for subdomain or root domain matching
    for (const ipObj of ipList) {
      if (!ipObj.ptr || !ipObj.public) continue

      // 1. Subdomain matching: ptr "mail.example.com" matches query "example.com" or vice versa
      if (ipObj.ptr.endsWith(`.${domain}`) || domain.endsWith(`.${ipObj.ptr}`)) {
        return ipObj.address
      }

      // 2. Root domain matching: ptr root "example.com" matches query root "example.com"
      if (queryRoot && this.#getRootDomain(ipObj.ptr) === queryRoot) {
        return ipObj.address
      }
    }

    return defaultIP
  }

  /**
   * Extracts root domain (e.g., example.com) from a hostname
   * @param {string} hostname
   * @returns {string|null}
   */
  #getRootDomain(hostname) {
    if (!hostname) return null
    const parts = hostname.toLowerCase().split('.')
    if (parts.length < 2) return null

    // Simple root domain logic: last two parts
    // Note: In production, this would need a Public Suffix List for co.uk etc.
    // but for our internal matching, comparing the last two parts is usually sufficient.
    return parts.slice(-2).join('.')
  }

  #processAAAARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        const address = record.value ?? this.#resolveIPByPTR(questionName, 'ipv6')
        if (!address) continue // Skip if no IPv6 address available
        response.answer.push(
          dns.AAAA({
            name: record.name,
            address: address,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing AAAA records:', err.message)
    }
  }

  #processCNAMERecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.answer.push(
          dns.CNAME({
            name: record.name,
            data: record.value ?? questionName,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing CNAME records:', err.message)
    }
  }

  #processMXRecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.answer.push(
          dns.MX({
            name: record.name,
            exchange: record.value ?? questionName,
            priority: record.priority ?? 10,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing MX records:', err.message)
    }
  }

  #processNSRecords(records, questionName, response, domain) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.header.aa = 1
        response.authority.push(
          dns.NS({
            name: record.name,
            data: record.value ?? domain,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing NS records:', err.message)
    }
  }

  #processTXTRecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (!record || record.name !== questionName) continue
        response.answer.push(
          dns.TXT({
            name: record.name,
            data: [record.value],
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing TXT records:', err.message)
    }
  }

  #processCAARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (!record || record.name !== questionName) continue

        // CAA record format: flags tag value
        // Example: "0 issue letsencrypt.org"
        const caaParts = record.value.split(' ')
        if (caaParts.length < 3) continue

        const flags = parseInt(caaParts[0]) || 0
        const tag = caaParts[1]
        const value = caaParts.slice(2).join(' ')

        response.answer.push(
          this.#createCAARecord({
            name: record.name,
            flags: flags,
            tag: tag,
            value: value,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing CAA records:', err.message)
    }
  }

  #addDefaultCAARecords(questionName, response) {
    try {
      // Add default CAA records allowing Let's Encrypt
      response.answer.push(
        this.#createCAARecord({
          name: questionName,
          flags: 0,
          tag: 'issue',
          value: 'letsencrypt.org',
          ttl: 3600
        })
      )
      response.answer.push(
        this.#createCAARecord({
          name: questionName,
          flags: 0,
          tag: 'issuewild',
          value: 'letsencrypt.org',
          ttl: 3600
        })
      )
      log("Added default CAA records for Let's Encrypt to response for:", questionName)
    } catch (err) {
      error('Error adding default CAA records:', err.message)
    }
  }

  #createCAARecord(opts) {
    const flags = opts.flags || 0
    const tag = opts.tag
    const value = opts.value

    const flagsBuf = Buffer.alloc(1)
    flagsBuf.writeUInt8(flags, 0)

    const tagBuf = Buffer.from(tag)
    const tagLenBuf = Buffer.alloc(1)
    tagLenBuf.writeUInt8(tagBuf.length, 0)

    const valueBuf = Buffer.from(value)

    const data = Buffer.concat([flagsBuf, tagLenBuf, tagBuf, valueBuf])

    return {
      name: opts.name,
      type: dns.consts.NAME_TO_QTYPE.CAA || 257,
      class: 1, // IN
      ttl: opts.ttl || 3600,
      data: data
    }
  }

  record(...args) {
    if (!Odac.core('Config').config.dns) Odac.core('Config').config.dns = {}

    let changedDomains = new Set()

    for (let obj of args) {
      let domain = obj.name
      // Walk up to find the root domain zone
      // Note: If domain doesn't exist yet, we stick to the provided name unless it's a subdomain we want to attach to parent
      // But creating a new record usually implies creating context.
      // Logic: If exact domain exists in DNS, use it. If not, try parents. If none, assume creating new zone.

      let zoneDomain = domain
      let found = false

      const dnsConfig = Odac.core('Config').config.dns

      // Try to find existing zone
      let temp = domain
      while (temp.includes('.')) {
        if (dnsConfig[temp]) {
          zoneDomain = temp
          found = true
          break
        }
        temp = temp.split('.').slice(1).join('.')
      }

      // If we didn't find a parent zone, and this is seemingly a new domain (e.g. from Web.create), we initialize it
      // Standard logic: The domain passed in the first record is the zone root usually
      if (!found) {
        zoneDomain = domain
      }

      // Initialize zone if missing
      if (!dnsConfig[zoneDomain]) {
        const dateStr = new Date()
          .toISOString()
          .replace(/[^0-9]/g, '')
          .slice(0, 8)
        dnsConfig[zoneDomain] = {
          soa: {
            primary: `ns1.${zoneDomain}`,
            email: `hostmaster.${zoneDomain}`,
            serial: parseInt(dateStr + '01'),
            refresh: 3600,
            retry: 600,
            expire: 604800,
            minimum: 3600,
            ttl: 3600
          },
          records: []
        }
        // Add default Let's Encrypt CAA
        dnsConfig[zoneDomain].records.push({
          id: randomUUID(),
          type: 'CAA',
          name: zoneDomain,
          value: '0 issue letsencrypt.org',
          ttl: 3600
        })
        dnsConfig[zoneDomain].records.push({
          id: randomUUID(),
          type: 'CAA',
          name: zoneDomain,
          value: '0 issuewild letsencrypt.org',
          ttl: 3600
        })
      }

      const zone = dnsConfig[zoneDomain]
      if (!obj.type) continue

      let type = obj.type.toUpperCase()
      const validTypes = ['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS', 'CAA']
      if (!validTypes.includes(type)) continue

      // Filter functionality: If we are setting a record that must be unique (like CNAME for a specific subdomain?), remove old one?
      // The original code had logic: if (obj.unique !== false) remove existing

      // If updating a record, remove conflict
      if (obj.unique !== false) {
        zone.records = zone.records.filter(r => !(r.type === type && r.name === obj.name))
      }

      zone.records.push({
        id: randomUUID(),
        type: type,
        name: obj.name,
        value: obj.value,
        priority: obj.priority,
        ttl: obj.ttl || 3600
      })

      changedDomains.add(zoneDomain)
    }

    // Update SOA serials
    for (const domain of changedDomains) {
      this.#updateSOASerial(domain)
    }

    if (changedDomains.size > 0) {
      if (Odac.core('Config').force) Odac.core('Config').force()
    }
  }

  #updateSOASerial(domain) {
    const zone = Odac.core('Config').config.dns[domain]
    if (!zone || !zone.soa) return

    const dateStr = new Date()
      .toISOString()
      .replace(/[^0-9]/g, '')
      .slice(0, 8)

    // Logic: If serial starts with today's date, increment. Else set to Today + 01
    // SOA format: YYYYMMDDNN

    let currentSerial = zone.soa.serial
    let currentSerialStr = currentSerial.toString()
    let currentDatePrefix = currentSerialStr.slice(0, 8)

    if (currentDatePrefix === dateStr) {
      zone.soa.serial++
    } else {
      zone.soa.serial = parseInt(dateStr + '01')
    }
  }
}

module.exports = new DNS()
