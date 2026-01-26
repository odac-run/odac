const {log, error} = Odac.core('Log', false).init('DNS')

const axios = require('axios')
const dns = require('native-dns')
const {execSync} = require('child_process')
const fs = require('fs')
const os = require('os')

class DNS {
  ip = '127.0.0.1'
  #loaded = false
  #tcp
  #types = ['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS', 'SOA', 'CAA']
  #udp
  #requestCount = new Map() // Rate limiting
  #rateLimit = 2500 // requests per minute per IP
  #rateLimitWindow = 60000 // 1 minute

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
    for (let obj of args) {
      let domain = obj.name
      while (!Odac.core('Config').config.websites[domain] && domain.includes('.')) domain = domain.split('.').slice(1).join('.')
      if (!Odac.core('Config').config.websites[domain]) continue
      if (!obj.type) continue
      let type = obj.type.toUpperCase()
      if (!this.#types.includes(type)) continue
      if (!Odac.core('Config').config.websites[domain].DNS || !Odac.core('Config').config.websites[domain].DNS[type]) continue
      Odac.core('Config').config.websites[domain].DNS[type] = Odac.core('Config').config.websites[domain].DNS[type].filter(
        record => !(record.name === obj.name && (!obj.value || record.value === obj.value))
      )
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
  }

  start() {
    if (this.#loaded) return
    this.#getExternalIP()
    this.#publish()
  }

  async stop() {
    const closePromises = []
    try {
      if (this.#udp) {
        closePromises.push(
          new Promise(resolve => {
            this.#udp.close(resolve)
          })
        )
      }
      if (this.#tcp) {
        closePromises.push(
          new Promise(resolve => {
            this.#tcp.close(resolve)
          })
        )
      }
      await Promise.all(closePromises)
      this.#loaded = false
    } catch (e) {
      error('Error stopping DNS services: %s', e.message)
    }
  }

  async #getExternalIP() {
    // Multiple IP detection services as fallbacks
    const ipServices = [
      'https://curlmyip.org/',
      'https://ipv4.icanhazip.com/',
      'https://api.ipify.org/',
      'https://checkip.amazonaws.com/',
      'https://ipinfo.io/ip'
    ]

    for (const service of ipServices) {
      try {
        log(`Attempting to get external IP from ${service}`)
        const response = await axios.get(service, {
          timeout: 5000,
          headers: {
            'User-Agent': 'Odac-DNS/1.0'
          }
        })

        const ip = response.data.trim()
        if (ip && /^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(ip)) {
          log('External IP detected:', ip)
          this.ip = ip
          return
        } else {
          log(`Invalid IP format from ${service}:`, ip)
        }
      } catch (err) {
        log(`Failed to get IP from ${service}:`, err.message)
        continue
      }
    }

    // If all services fail, try to get local network IP
    try {
      const networkInterfaces = require('os').networkInterfaces()
      for (const interfaceName in networkInterfaces) {
        const interfaces = networkInterfaces[interfaceName]
        for (const iface of interfaces) {
          // Skip loopback and non-IPv4 addresses
          if (!iface.internal && iface.family === 'IPv4') {
            log('Using local network IP as fallback:', iface.address)
            this.ip = iface.address
            return
          }
        }
      }
    } catch (err) {
      log('Failed to get local network IP:', err.message)
    }

    log('Could not determine external IP, using default 127.0.0.1')
    error('DNS', 'All IP detection methods failed, DNS A records will use 127.0.0.1')
  }

  #publish() {
    if (this.#loaded || !Object.keys(Odac.core('Config').config.websites ?? {}).length) return
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
          'lsof -i :53 2>/dev/null || netstat -tulpn 2>/dev/null | grep :53 || ss -tulpn 2>/dev/null | grep :53 || echo "Port 53 appears to be free"',
          {
            encoding: 'utf8',
            timeout: 5000
          }
        )
        log('Port 53 status:', port53Info.trim() || 'No processes found on port 53')
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

      let domain = questionName
      while (!Odac.core('Config').config.websites[domain] && domain.includes('.')) {
        domain = domain.split('.').slice(1).join('.')
      }

      if (!Odac.core('Config').config.websites[domain] || !Odac.core('Config').config.websites[domain].DNS) {
        // For unknown domains, send proper NXDOMAIN response instead of empty response
        response.header.rcode = dns.consts.NAME_TO_RCODE.NXDOMAIN
        return response.send()
      }

      const dnsRecords = Odac.core('Config').config.websites[domain].DNS

      // Only process records relevant to the question type for better performance
      switch (questionType) {
        case dns.consts.NAME_TO_QTYPE.A:
          this.#processARecords(dnsRecords.A, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.AAAA:
          this.#processAAAARecords(dnsRecords.AAAA, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.CNAME:
          this.#processCNAMERecords(dnsRecords.CNAME, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.MX:
          this.#processMXRecords(dnsRecords.MX, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.TXT:
          this.#processTXTRecords(dnsRecords.TXT, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.NS:
          this.#processNSRecords(dnsRecords.NS, questionName, response, domain)
          break
        case dns.consts.NAME_TO_QTYPE.SOA:
          this.#processSOARecords(dnsRecords.SOA, questionName, response)
          break
        case dns.consts.NAME_TO_QTYPE.CAA:
          this.#processCAARecords(dnsRecords.CAA, questionName, response)
          // If no CAA records found, add default Let's Encrypt CAA records
          if (!response.answer.length && dnsRecords.CAA?.length === 0) {
            this.#addDefaultCAARecords(questionName, response)
          }
          break
        default:
          // For ANY queries or unknown types, process all relevant records
          this.#processARecords(dnsRecords.A, questionName, response)
          this.#processAAAARecords(dnsRecords.AAAA, questionName, response)
          this.#processCNAMERecords(dnsRecords.CNAME, questionName, response)
          this.#processMXRecords(dnsRecords.MX, questionName, response)
          this.#processTXTRecords(dnsRecords.TXT, questionName, response)
          this.#processNSRecords(dnsRecords.NS, questionName, response, domain)
          this.#processSOARecords(dnsRecords.SOA, questionName, response)
          this.#processCAARecords(dnsRecords.CAA, questionName, response)
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

  #processARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.answer.push(
          dns.A({
            name: record.name,
            address: record.value ?? this.ip,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing A records:', err.message)
    }
  }

  #processAAAARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (record.name !== questionName) continue
        response.answer.push(
          dns.AAAA({
            name: record.name,
            address: record.value,
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

  #processSOARecords(records, questionName, response) {
    try {
      for (const record of records ?? []) {
        if (!record || !record.value) continue
        const soaParts = record.value.split(' ')
        if (soaParts.length < 7) continue
        response.header.aa = 1
        response.authority.push(
          dns.SOA({
            name: record.name,
            primary: soaParts[0],
            admin: soaParts[1],
            serial: parseInt(soaParts[2]) || 1,
            refresh: parseInt(soaParts[3]) || 3600,
            retry: parseInt(soaParts[4]) || 600,
            expiration: parseInt(soaParts[5]) || 604800,
            minimum: parseInt(soaParts[6]) || 3600,
            ttl: record.ttl ?? 3600
          })
        )
      }
    } catch (err) {
      error('Error processing SOA records:', err.message)
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
    let domains = []
    for (let obj of args) {
      let domain = obj.name
      while (!Odac.core('Config').config.websites[domain] && domain.includes('.')) domain = domain.split('.').slice(1).join('.')
      if (!Odac.core('Config').config.websites[domain]) continue
      if (!obj.type) continue
      let type = obj.type.toUpperCase()
      delete obj.type
      if (!this.#types.includes(type)) continue
      if (!Odac.core('Config').config.websites[domain].DNS) Odac.core('Config').config.websites[domain].DNS = {}
      if (!Odac.core('Config').config.websites[domain].DNS[type]) Odac.core('Config').config.websites[domain].DNS[type] = []
      if (obj.unique !== false) {
        Odac.core('Config').config.websites[domain].DNS[type] = Odac.core('Config').config.websites[domain].DNS[type].filter(
          record => record.name !== obj.name
        )
      }
      Odac.core('Config').config.websites[domain].DNS[type].push(obj)
      domains.push(domain)
    }
    let date = new Date()
      .toISOString()
      .replace(/[^0-9]/g, '')
      .slice(0, 10)
    for (let domain of domains) {
      // Add SOA record
      Odac.core('Config').config.websites[domain].DNS.SOA = [
        {
          name: domain,
          value: 'ns1.' + domain + ' hostmaster.' + domain + ' ' + date + ' 3600 600 604800 3600'
        }
      ]

      // Add default CAA records for Let's Encrypt SSL certificates
      if (!Odac.core('Config').config.websites[domain].DNS.CAA) {
        Odac.core('Config').config.websites[domain].DNS.CAA = [
          {
            name: domain,
            value: '0 issue letsencrypt.org',
            ttl: 3600
          },
          {
            name: domain,
            value: '0 issuewild letsencrypt.org',
            ttl: 3600
          }
        ]
        log("Added default CAA records for Let's Encrypt to domain:", domain)
      }
    }
  }
}

module.exports = new DNS()
