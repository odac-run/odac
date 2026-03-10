const {log, error} = Odac.core('Log', false).init('DNS')

const axios = require('axios')
const childProcess = require('child_process')
const fs = require('fs')
const nodeDns = require('dns')
const os = require('os')
const path = require('path')
const {randomUUID} = require('crypto')

/**
 * DNS service that delegates authoritative DNS serving to a Go binary.
 * Mirrors the Proxy.js architecture: Node.js spawns a Go child process
 * and communicates via Unix socket (or TCP fallback) for config sync.
 *
 * Node.js retains: IP detection, PTR lookup, zone CRUD, SOA serial management.
 * Go binary handles: UDP/TCP DNS serving, rate limiting, query processing.
 */
class DNS {
  ips = {ipv4: [], ipv6: []} // Arrays of {address, ptr, public} objects
  ip = '127.0.0.1' // Primary IPv4 for backward compatibility

  #active = false
  #dnsApiPort = null
  #dnsProcess = null
  #dnsSocketPath = null

  // Private IPv4 ranges (RFC 1918, RFC 6598, Link-local)
  #privateIPv4Ranges = [
    {start: 0x0a000000, end: 0x0affffff}, // 10.0.0.0/8
    {start: 0x64400000, end: 0x647fffff}, // 100.64.0.0/10 (CGNAT)
    {start: 0x7f000000, end: 0x7fffffff}, // 127.0.0.0/8 (loopback)
    {start: 0xa9fe0000, end: 0xa9feffff}, // 169.254.0.0/16 (link-local)
    {start: 0xac100000, end: 0xac1fffff}, // 172.16.0.0/12
    {start: 0xc0a80000, end: 0xc0a8ffff} // 192.168.0.0/16
  ]

  /**
   * Checks if the DNS process is healthy and respawns if needed.
   * Called by the Watchdog service on periodic health checks.
   */
  check() {
    if (!this.#active) return
    this.spawnDNS()
  }

  /**
   * Removes DNS records matching the given criteria from zone config.
   * Triggers config sync to Go binary after modification.
   * @param {...Object} args - Objects with {name, type, value?} to delete
   */
  delete(...args) {
    if (!Odac.core('Config').config.dns) return

    let changedDomains = new Set()

    for (let obj of args) {
      let domain = obj.name
      if (!this.#isSafe(domain)) continue

      const config = Odac.core('Config').config
      while (domain.includes('.') && (!Object.prototype.hasOwnProperty.call(config.dns, domain) || !this.#isSafe(domain))) {
        domain = domain.split('.').slice(1).join('.')
      }

      if (!this.#isSafe(domain) || !Object.prototype.hasOwnProperty.call(config.dns, domain)) continue
      if (!obj.type) continue

      const type = obj.type.toUpperCase()
      const zone = config.dns[domain]

      const initialLength = zone.records.length
      zone.records = zone.records.filter(
        record => !(record.type === type && record.name === obj.name && (!obj.value || record.value === obj.value))
      )

      if (zone.records.length !== initialLength) {
        changedDomains.add(domain)
      }
    }

    for (const domain of changedDomains) {
      this.#updateSOASerial(domain)
    }

    if (changedDomains.size > 0) {
      if (Odac.core('Config').force) Odac.core('Config').force()
      this.syncConfig()
    }
  }

  /**
   * Adds or updates DNS records in zone config.
   * Creates new zones automatically with SOA and default CAA.
   * Triggers config sync to Go binary after modification.
   * @param {...Object} args - Objects with {name, type, value, priority?, ttl?, unique?}
   */
  record(...args) {
    if (!Odac.core('Config').config.dns) Odac.core('Config').config.dns = {}

    let changedDomains = new Set()

    for (let obj of args) {
      let domain = obj.name

      let found = false
      let zoneDomain = domain
      const dnsConfig = Odac.core('Config').config.dns

      let temp = domain
      while (temp.includes('.')) {
        if (this.#isSafe(temp) && Object.prototype.hasOwnProperty.call(dnsConfig, temp)) {
          zoneDomain = temp
          found = true
          break
        }
        temp = temp.split('.').slice(1).join('.')
      }

      if (!found) {
        zoneDomain = domain
      }

      if (!this.#isSafe(zoneDomain)) continue

      // Initialize zone if missing
      if (!Object.prototype.hasOwnProperty.call(dnsConfig, zoneDomain)) {
        const dateStr = new Date()
          .toISOString()
          .replace(/[^0-9]/g, '')
          .slice(0, 8)
        dnsConfig[zoneDomain] = {
          soa: {
            email: `hostmaster.${zoneDomain}`,
            expire: 604800,
            minimum: 3600,
            primary: `ns1.${zoneDomain}`,
            refresh: 3600,
            retry: 600,
            serial: parseInt(dateStr + '01'),
            ttl: 3600
          },
          records: []
        }
        dnsConfig[zoneDomain].records.push({
          id: randomUUID(),
          name: zoneDomain,
          ttl: 3600,
          type: 'CAA',
          value: '0 issue letsencrypt.org'
        })
        dnsConfig[zoneDomain].records.push({
          id: randomUUID(),
          name: zoneDomain,
          ttl: 3600,
          type: 'CAA',
          value: '0 issuewild letsencrypt.org'
        })
      }

      const zone = dnsConfig[zoneDomain]
      if (!obj.type) continue

      let type = obj.type.toUpperCase()
      const validTypes = ['A', 'AAAA', 'CAA', 'CNAME', 'MX', 'NS', 'TXT']
      if (!validTypes.includes(type)) continue

      if (obj.unique !== false) {
        zone.records = zone.records.filter(r => !(r.type === type && r.name === obj.name))
      }

      zone.records.push({
        id: randomUUID(),
        name: obj.name,
        priority: obj.priority,
        ttl: obj.ttl || 3600,
        type: type,
        value: obj.value
      })

      changedDomains.add(zoneDomain)
    }

    for (const domain of changedDomains) {
      this.#updateSOASerial(domain)
    }

    if (changedDomains.size > 0) {
      if (Odac.core('Config').force) Odac.core('Config').force()
      this.syncConfig()
    }
  }

  // Test helper — resets internal state for unit tests
  reset() {
    this.#dnsApiPort = null
    this.#dnsProcess = null
    this.#dnsSocketPath = null
  }

  /**
   * Spawns or adopts the Go DNS binary process.
   * Follows the exact same pattern as Proxy.js#spawnProxy().
   */
  spawnDNS() {
    if (this.#dnsProcess) return

    const isWindows = os.platform() === 'win32'
    const binaryName = isWindows ? 'odac-dns.exe' : 'odac-dns'
    const binPath = path.resolve(__dirname, '../../bin', binaryName)
    const logDir = path.join(os.homedir(), '.odac', 'logs')
    const runDir = path.join(os.homedir(), '.odac', 'run')

    if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, {recursive: true})
    if (!fs.existsSync(runDir)) fs.mkdirSync(runDir, {recursive: true})

    const instanceId = process.env.ODAC_INSTANCE_ID || 'default'
    const logFile = path.join(logDir, 'dns.log')
    const pidFile = path.join(runDir, `dns-${instanceId}.pid`)

    if (!isWindows) {
      this.#dnsSocketPath = path.join(runDir, `dns-${instanceId}.sock`)
    }

    // 1. Try to adopt existing process (skip in Update Mode)
    const isUpdateMode = process.env.ODAC_UPDATE_MODE === 'true'

    if (!isUpdateMode) {
      try {
        const pid = parseInt(fs.readFileSync(pidFile, 'utf8'))
        process.kill(pid, 0)

        // Validate socket exists (Unix) to prevent PID reuse attacks
        if (!isWindows) {
          if (!fs.existsSync(this.#dnsSocketPath)) {
            log(`PID ${pid} exists but socket file is missing. PID reuse detected or DNS crashed. Ignoring orphan...`)
            try {
              fs.unlinkSync(pidFile)
            } catch {
              /* ignore */
            }
            throw new Error('Socket missing')
          }
        }

        // Verify process name on Linux (PID reuse attack mitigation)
        try {
          const procPath = `/proc/${pid}/cmdline`
          if (fs.existsSync(procPath)) {
            const cmdline = fs.readFileSync(procPath, 'utf8')
            if (!cmdline.includes('odac-dns')) {
              log(`PID ${pid} is active but command line does not match DNS binary. PID reuse detected!`)
              try {
                fs.unlinkSync(pidFile)
              } catch {
                /* ignore */
              }
              throw new Error('PID reuse detected')
            }
          }
        } catch (e) {
          if (e.message === 'PID reuse detected') throw e
        }

        log(`Found orphaned Go DNS (PID: ${pid}). Reconnecting...`)

        this.#dnsProcess = {
          pid,
          kill: () => {
            try {
              process.kill(pid)
            } catch {
              /* ignore */
            }
          }
        }

        setTimeout(() => this.syncConfig(), 1000)
        return
      } catch (err) {
        if (err.code !== 'ENOENT') {
          log('Orphaned DNS PID file issue. Cleaning up.')
          try {
            fs.unlinkSync(pidFile)
          } catch {
            /* ignore */
          }
        }
      }
    } else {
      log('Update mode detected. Forcing new DNS instance spawn...')
    }

    if (!fs.existsSync(binPath)) {
      error(`Go DNS binary not found at ${binPath}. Please run 'go build -o bin/${binaryName} ./server/dns'`)
      return
    }

    // 2. Start new DNS process
    let env = {...process.env}

    if (!isWindows) {
      env.ODAC_DNS_SOCKET_PATH = this.#dnsSocketPath
      log(`Starting Go DNS (Socket: ${this.#dnsSocketPath})...`)
    } else {
      log('Starting Go DNS (TCP Mode)...')
    }

    try {
      const logFd = fs.openSync(logFile, 'a')

      this.#dnsProcess = childProcess.spawn(binPath, [], {
        detached: true,
        env: env,
        stdio: ['ignore', logFd, logFd]
      })

      this.#dnsProcess.unref()

      if (this.#dnsProcess.pid) {
        try {
          const flags = isUpdateMode ? 'w' : 'wx'
          fs.writeFileSync(pidFile, this.#dnsProcess.pid.toString(), {flag: flags})
          log(`Go DNS started with PID ${this.#dnsProcess.pid}`)
        } catch (err) {
          if (err.code === 'EEXIST') {
            error(`Race condition detected: PID file ${pidFile} already exists. Stopping redundant DNS instance.`)
            this.#dnsProcess.kill()
            this.#dnsProcess = null
            return
          }
          throw err
        }
      }

      this.#dnsProcess.on('exit', code => {
        error(`Go DNS exited with code ${code}`)
        this.#dnsProcess = null
        try {
          fs.unlinkSync(pidFile)
        } catch {
          /* ignore */
        }
      })

      setTimeout(() => this.syncConfig(), 1000)

      // Cleanup previous instance files
      const prevId = process.env.ODAC_PREVIOUS_INSTANCE_ID
      if (prevId) {
        setTimeout(() => {
          log(`Cleaning up files from previous DNS instance: ${prevId}`)
          const prevPidFile = path.join(runDir, `dns-${prevId}.pid`)
          const prevSockFile = path.join(runDir, `dns-${prevId}.sock`)

          try {
            if (fs.existsSync(prevPidFile)) fs.unlinkSync(prevPidFile)
            if (fs.existsSync(prevSockFile)) fs.unlinkSync(prevSockFile)
            log(`DNS cleanup successful for instance ${prevId}`)
          } catch (e) {
            log(`Warning: Failed to cleanup previous DNS instance files: ${e.message}`)
          }
        }, 60000)
      }
    } catch (err) {
      error(`Failed to spawn Go DNS: ${err.message}`)
    }
  }

  /**
   * Starts the DNS service: detects IPs, spawns Go binary, syncs config.
   */
  async start() {
    if (this.#active) return
    this.#active = true

    await this.#getExternalIP()
    this.spawnDNS()
  }

  /**
   * Stops the DNS service: kills the Go binary and cleans up.
   */
  stop() {
    this.#active = false
    if (this.#dnsProcess) {
      this.#dnsProcess.kill()
      this.#dnsProcess = null
      this.#dnsApiPort = null
      if (this.#dnsSocketPath && fs.existsSync(this.#dnsSocketPath)) {
        try {
          fs.unlinkSync(this.#dnsSocketPath)
        } catch {
          /* ignore */
        }
      }
    }
  }

  /**
   * Syncs the full DNS zone configuration to the Go binary.
   * Sends zones + IP data so the Go resolver can serve authoritative answers.
   * @param {number} retryCount - Internal retry counter
   */
  async syncConfig(retryCount = 0) {
    log('DNS: syncConfig called (Retry: %d)', retryCount)

    if (!this.#dnsProcess) return
    if (!this.#dnsSocketPath && !this.#dnsApiPort) return

    if (this.#dnsSocketPath && !fs.existsSync(this.#dnsSocketPath)) {
      return
    }

    const dnsConfig = Odac.core('Config').config.dns || {}

    const payload = {
      ips: {
        ipv4: this.ips.ipv4,
        ipv6: this.ips.ipv6,
        primary: this.ip
      },
      zones: dnsConfig
    }

    log('DNS: Syncing %d zones to Go binary', Object.keys(dnsConfig).length)

    try {
      if (this.#dnsSocketPath) {
        await axios.post('http://localhost/config', payload, {
          socketPath: this.#dnsSocketPath,
          validateStatus: () => true
        })
      } else {
        await axios.post(`http://127.0.0.1:${this.#dnsApiPort}/config`, payload)
      }
    } catch (e) {
      if (retryCount < 3 && (e.code === 'ECONNREFUSED' || e.code === 'ENOENT' || e.code === 'ECONNRESET')) {
        log(`DNS config sync failed (${e.code}). Retrying in 1s...`)
        await new Promise(r => setTimeout(r, 1000))
        return this.syncConfig(retryCount + 1)
      }
      error(`Failed to sync DNS config to Go binary: ${e.message}`)
    }
  }

  // ─── Private: IP Detection ─────────────────────────────────────────

  #collectLocalIPs() {
    try {
      const networkInterfaces = os.networkInterfaces()
      for (const interfaceName in networkInterfaces) {
        const interfaces = networkInterfaces[interfaceName]
        for (const iface of interfaces) {
          if (iface.internal) continue

          if (iface.family === 'IPv4') {
            if (!this.ips.ipv4.find(i => i.address === iface.address)) {
              const isPublic = !this.#isPrivateIPv4(iface.address)
              this.ips.ipv4.push({address: iface.address, ptr: null, public: isPublic})
              log(`Local IPv4 detected on ${interfaceName}: ${iface.address} [${isPublic ? 'public' : 'private'}]`)
            }
          } else if (iface.family === 'IPv6') {
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

  async #getExternalIP() {
    this.#collectLocalIPs()

    const ipv4Services = [
      'https://curlmyip.org/',
      'https://ipv4.icanhazip.com/',
      'https://api.ipify.org/',
      'https://checkip.amazonaws.com/',
      'https://ipinfo.io/ip'
    ]

    const ipv6Services = ['https://ipv6.icanhazip.com/', 'https://api64.ipify.org/', 'https://v6.ident.me/']

    for (const service of ipv4Services) {
      try {
        log(`Attempting to get external IPv4 from ${service}`)
        const response = await axios.get(service, {
          headers: {'User-Agent': 'Odac-DNS/1.0'},
          timeout: 5000
        })

        const ip = response.data.trim()
        if (ip && /^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(ip)) {
          log('External IPv4 detected:', ip)
          if (!this.ips.ipv4.find(i => i.address === ip)) {
            this.ips.ipv4.unshift({address: ip, ptr: null, public: true})
          }
          this.ip = ip
          break
        } else {
          log(`Invalid IPv4 format from ${service}:`, ip)
        }
      } catch (err) {
        log(`Failed to get IPv4 from ${service}:`, err.message)
        continue
      }
    }

    for (const service of ipv6Services) {
      try {
        log(`Attempting to get external IPv6 from ${service}`)
        const response = await axios.get(service, {
          family: 6,
          headers: {'User-Agent': 'Odac-DNS/1.0'},
          timeout: 5000
        })

        const ip = response.data.trim()
        if (ip && /^[0-9a-fA-F:]+$/.test(ip) && ip.includes(':')) {
          log('External IPv6 detected:', ip)
          if (!this.ips.ipv6.find(i => i.address === ip)) {
            this.ips.ipv6.unshift({address: ip, ptr: null, public: true})
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

    if (this.ips.ipv4.length === 0) {
      log('Could not determine external IPv4, using default 127.0.0.1')
      error('DNS', 'All IPv4 detection methods failed, DNS A records will use 127.0.0.1')
    } else {
      const publicIPv4 = this.ips.ipv4.find(i => i.public)
      this.ip = publicIPv4 ? publicIPv4.address : this.ips.ipv4[0].address
    }

    await this.#lookupPTRRecords()

    log(
      `Detected IPs - IPv4: [${this.ips.ipv4.map(i => `${i.address}${i.public ? ' [public]' : ' [private]'}${i.ptr ? ` (${i.ptr})` : ''}`).join(', ')}]`
    )
    log(
      `Detected IPs - IPv6: [${this.ips.ipv6.map(i => `${i.address}${i.public ? ' [public]' : ' [private]'}${i.ptr ? ` (${i.ptr})` : ''}`).join(', ')}]`
    )
  }

  #isPrivateIPv4(ip) {
    const parts = ip.split('.').map(Number)
    if (parts.length !== 4) return false
    const ipNum = ((parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]) >>> 0

    for (const range of this.#privateIPv4Ranges) {
      if (ipNum >= range.start && ipNum <= range.end) {
        return true
      }
    }
    return false
  }

  #isPrivateIPv6(ip) {
    const normalized = ip.toLowerCase()
    if (normalized.startsWith('fe80:')) return true
    if (normalized.startsWith('fc') || normalized.startsWith('fd')) return true
    if (normalized === '::1') return true
    return false
  }

  #isSafe(key) {
    const prohibited = ['__proto__', 'constructor', 'prototype']
    return typeof key === 'string' && !prohibited.includes(key.toLowerCase())
  }

  async #lookupPTRRecords() {
    const lookupPromises = []

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

    await Promise.race([Promise.allSettled(lookupPromises), new Promise(resolve => setTimeout(resolve, 5000))])
  }

  async #reverseLookup(ip) {
    try {
      const hostnames = await nodeDns.promises.reverse(ip)
      return hostnames && hostnames.length > 0 ? hostnames[0] : null
    } catch {
      return null
    }
  }

  #updateSOASerial(domain) {
    const zone = Odac.core('Config').config.dns[domain]
    if (!zone || !zone.soa) return

    const dateStr = new Date()
      .toISOString()
      .replace(/[^0-9]/g, '')
      .slice(0, 8)

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
