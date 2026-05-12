const {log, error} = Odac.core('Log', false).init('Mail')

const {generateKeyPair} = require('crypto')
const childProcess = require('child_process')
const fs = require('fs')
const fsp = require('fs/promises')
const os = require('os')
const path = require('path')
const {promisify} = require('util')

/**
 * Mail service manager that spawns and communicates with the Go mail binary.
 * Mirrors the Proxy.js and DNS.js architecture: Node.js retains config management,
 * DKIM key generation, and domain CRUD; Go binary handles SMTP/IMAP serving,
 * SQLite storage, authentication, and outbound delivery.
 */
class Mail {
  #active = false
  #checking = false
  #cleanupTimer = null
  #mailApiPort = null
  #mailProcess = null
  #mailSocketPath = null
  #syncTimer = null

  /**
   * Checks the health of the Go mail binary and respawns if needed.
   * Also performs DKIM key generation for domains missing DKIM config.
   * Called by the Watchdog service on periodic health checks.
   */
  async check() {
    if (this.#checking) return
    if (!this.#active) return
    this.#checking = true

    // Respawn Go binary if it crashed
    this.spawnMail()

    // DKIM key generation (stays in Node.js — needs DNS record creation)
    try {
      const dnsConfig = Odac.core('Config').config.dns ?? {}
      for (const domain of Object.keys(Odac.core('Config').config.domains ?? {})) {
        const zone = dnsConfig[domain]
        if (!zone?.records?.some(r => r.type === 'MX')) continue
        if (Odac.core('Config').config.domains[domain].cert !== false && !Odac.core('Config').config.domains[domain].cert?.dkim)
          await this.#dkim(domain)
      }
    } catch (e) {
      error('DKIM check failed: %s', e.message)
    }

    this.#checking = false
  }

  /**
   * Clears the TLS context cache on the Go mail binary for a domain.
   * Called by SSL.js after certificate renewal.
   * @param {string} [domain] - Domain to clear, or all if omitted
   */
  async clearSSLCache(domain) {
    if (!this.#mailProcess) return
    if (!this.#mailSocketPath && !this.#mailApiPort) return

    try {
      const payload = {domain: domain || ''}
      if (this.#mailSocketPath) {
        await Odac.core('Http').post('http://localhost/ssl/clear', payload, {
          socketPath: this.#mailSocketPath,
          validateStatus: () => true
        })
      } else {
        await Odac.core('Http').post(`http://127.0.0.1:${this.#mailApiPort}/ssl/clear`, payload)
      }
    } catch (e) {
      error('Failed to clear SSL cache: %s', e.message)
    }
  }

  /**
   * Creates a new mail account via the Go binary API.
   * @param {string} email - Email address
   * @param {string} password - Password
   * @param {string} retype - Password confirmation
   */
  async create(email, password, retype) {
    if (!email || !password || !retype) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (password != retype) return Odac.server('Api').result(false, await __('Passwords do not match.'))

    // Resolve domain (check subdomains)
    let domain = email.split('@')[1]
    if (!Odac.core('Config').config.domains?.[domain]) {
      for (let d in Odac.core('Config').config.domains ?? {}) {
        if (domain.substr(-d.length) != d) continue
        if (Odac.core('Config').config.domains[d].subdomain?.includes(domain.substr(0, domain.length - d.length - 1))) {
          domain = d
          break
        }
      }
      if (!Odac.core('Config').config.domains?.[domain]) {
        return Odac.server('Api').result(false, await __('Domain %s not found.', domain))
      }
    }

    try {
      const res = await this.#apiRequest('POST', '/account', {domain, email, password, retype})
      if (res?.success) {
        this.syncConfig()
        return Odac.server('Api').result(true, await __('Mail account %s created successfully.', email))
      }
      return Odac.server('Api').result(false, res?.message || (await __('Account creation failed.')))
    } catch (e) {
      error('Account creation failed: %s', e.message)
      return Odac.server('Api').result(false, await __('Account creation failed.'))
    }
  }

  /**
   * Deletes a mail account via the Go binary API.
   * @param {string} email - Email address to delete
   */
  async delete(email) {
    if (!email) return Odac.server('Api').result(false, await __('Email address is required.'))

    try {
      const res = await this.#apiRequest('DELETE', '/account', {email})
      if (res?.success) return Odac.server('Api').result(true, await __('Mail account %s deleted successfully.', email))
      return Odac.server('Api').result(false, res?.message || (await __('Account deletion failed.')))
    } catch {
      return Odac.server('Api').result(false, await __('Account deletion failed.'))
    }
  }

  /**
   * Lists mail accounts for a domain via the Go binary API.
   * @param {string} domain - Domain name
   */
  async list(domain) {
    if (!domain) return Odac.server('Api').result(false, await __('Domain is required.'))
    if (!Odac.core('Config').config.domains?.[domain]) return Odac.server('Api').result(false, await __('Domain %s not found.', domain))

    try {
      const res = await this.#apiRequest('GET', `/accounts?domain=${encodeURIComponent(domain)}`)
      if (res?.success) {
        const accounts = res.accounts || []
        return Odac.server('Api').result(true, (await __('Mail accounts for domain %s.', domain)) + '\n' + accounts.join('\n'))
      }
      return Odac.server('Api').result(false, res?.message || (await __('Account list failed.')))
    } catch {
      return Odac.server('Api').result(false, await __('Account list failed.'))
    }
  }

  /**
   * Updates a mail account password via the Go binary API.
   * @param {string} email - Email address
   * @param {string} password - New password
   * @param {string} retype - Password confirmation
   */
  async password(email, password, retype) {
    if (!email || !password || !retype) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (password != retype) return Odac.server('Api').result(false, await __('Passwords do not match.'))

    try {
      const res = await this.#apiRequest('PUT', '/account/password', {email, password, retype})
      if (res?.success) return Odac.server('Api').result(true, await __('Mail account %s password updated successfully.', email))
      return Odac.server('Api').result(false, res?.message || (await __('Password update failed.')))
    } catch {
      return Odac.server('Api').result(false, await __('Password update failed.'))
    }
  }

  // Test helper
  reset() {
    this.#mailProcess = null
    this.#mailSocketPath = null
    this.#mailApiPort = null
  }

  /**
   * Sends an email via the Go binary's outbound SMTP client.
   * Constructs the email payload and delegates to the Go API for delivery.
   * @param {Object} data - Email data with from, to, header, html, text, attachments
   */
  async send(data) {
    if (!data || !data.from || !data.to || !data.header) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (!data.from.value?.[0]?.address) return Odac.server('Api').result(false, await __('Invalid email address.'))
    if (!data.to.value?.[0]?.address) return Odac.server('Api').result(false, await __('Invalid email address.'))

    let domain = data.from.value[0].address.split('@')[1].split('.')
    while (domain.length > 2 && !Odac.core('Config').config.domains?.[domain.join('.')]) domain.shift()
    domain = domain.join('.')
    if (!Odac.core('Config').config.domains?.[domain]) return Odac.server('Api').result(false, await __('Domain %s not found.', domain))

    // Build RFC 2822 compliant MIME message from structured data
    const contentType = data.header?.['Content-Type'] || ''
    const isMultipart = contentType.includes('multipart/alternative')

    // Extract boundary from Content-Type if multipart
    let boundary = null
    if (isMultipart) {
      const match = contentType.match(/boundary="?([^";\s]+)"?/)
      if (match) boundary = match[1]
    }

    let headers = ''
    for (const key in data.header) {
      // Skip Content-Type for now; we rebuild it below if multipart
      if (isMultipart && key === 'Content-Type') continue
      headers += `${key}: ${data.header[key]}\r\n`
    }
    if (!headers.toLowerCase().includes('from:')) headers += `From: ${data.from.value[0].address}\r\n`
    if (!headers.toLowerCase().includes('to:')) headers += `To: ${data.to.value[0].address}\r\n`
    if (!headers.toLowerCase().includes('subject:')) headers += `Subject: ${data.subject ?? ''}\r\n`
    if (!headers.toLowerCase().includes('mime-version:')) headers += 'MIME-Version: 1.0\r\n'

    let body = ''

    if (isMultipart && boundary && data.html) {
      // RFC 2046 multipart/alternative with proper boundary-delimited parts
      headers += `Content-Type: multipart/alternative; boundary="${boundary}"\r\n`
      body = headers + '\r\n'
      if (data.text) {
        body += `--${boundary}\r\n`
        body += 'Content-Type: text/plain; charset=UTF-8\r\n\r\n'
        body += data.text + '\r\n'
      }
      body += `--${boundary}\r\n`
      body += 'Content-Type: text/html; charset=UTF-8\r\n\r\n'
      body += data.html + '\r\n'
      body += `--${boundary}--\r\n`
    } else if (data.html) {
      if (!headers.toLowerCase().includes('content-type:')) {
        headers += 'Content-Type: text/html; charset=UTF-8\r\n'
      }
      body = headers + '\r\n' + data.html
    } else if (data.text) {
      if (!headers.toLowerCase().includes('content-type:')) {
        headers += 'Content-Type: text/plain; charset=UTF-8\r\n'
      }
      body = headers + '\r\n' + data.text
    } else {
      body = headers + '\r\n'
    }

    try {
      const res = await this.#apiRequest('POST', '/send', {
        body: body,
        from: data.from.value[0].address,
        to: data.to.value[0].address
      })
      if (res?.success) return Odac.server('Api').result(true, await __('Mail sent successfully.'))
      return Odac.server('Api').result(false, res?.message || (await __('Mail sending failed.')))
    } catch (e) {
      error('mail.send failed: %s', e.message)
      return Odac.server('Api').result(false, await __('Mail sending failed.'))
    }
  }

  /**
   * Spawns or adopts the Go mail binary process.
   * Follows the exact same pattern as Proxy.js#spawnProxy() and DNS.js#spawnDNS().
   */
  spawnMail() {
    if (this.#mailProcess) return

    const isWindows = os.platform() === 'win32'
    const binaryName = isWindows ? 'odac-mail.exe' : 'odac-mail'
    const binPath = path.resolve(__dirname, '../../bin', binaryName)
    const runDir = path.join(os.homedir(), '.odac', 'run')

    if (!fs.existsSync(runDir)) fs.mkdirSync(runDir, {recursive: true})

    const instanceId = process.env.ODAC_INSTANCE_ID || 'default'
    const pidFile = path.join(runDir, `mail-${instanceId}.pid`)

    if (!isWindows) {
      this.#mailSocketPath = path.join(runDir, `mail-${instanceId}.sock`)
    }

    // 1. Try to adopt existing process (skip in Update Mode)
    const isUpdateMode = process.env.ODAC_UPDATE_MODE === 'true'

    if (!isUpdateMode) {
      try {
        const pid = parseInt(fs.readFileSync(pidFile, 'utf8'))
        process.kill(pid, 0)

        // Validate socket exists (Unix) to prevent PID reuse attacks
        if (!isWindows) {
          if (!fs.existsSync(this.#mailSocketPath)) {
            log(`PID ${pid} exists but socket file is missing. PID reuse detected or Mail crashed. Ignoring orphan...`)
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
            if (!cmdline.includes('odac-mail')) {
              log(`PID ${pid} is active but command line does not match Mail binary. PID reuse detected!`)
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

        log(`Found orphaned Go Mail (PID: ${pid}). Reconnecting...`)

        this.#mailProcess = {
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
          log('Orphaned Mail PID file issue. Cleaning up.')
          try {
            fs.unlinkSync(pidFile)
          } catch {
            /* ignore */
          }
        }
      }
    } else {
      log('Update mode detected. Forcing new Mail instance spawn...')
    }

    if (!fs.existsSync(binPath)) {
      error(`Go mail binary not found at ${binPath}. Please run 'go build -o bin/${binaryName} ./server/mail'`)
      return
    }

    // 2. Start new Mail process
    let env = {...process.env}

    if (!isWindows) {
      env.ODAC_MAIL_SOCKET_PATH = this.#mailSocketPath
      log(`Starting Go Mail (Socket: ${this.#mailSocketPath})...`)
    } else {
      log('Starting Go Mail (TCP Mode)...')
    }

    try {
      // Go binary writes its own log to ~/.odac/logs/mail.log with size-based
      // rotation (server/mail/logrotate.go). stdio fully discarded so a Node
      // restart can't SIGPIPE the detached process during orphan adoption.
      this.#mailProcess = childProcess.spawn(binPath, [], {
        detached: true,
        env: env,
        stdio: ['ignore', 'ignore', 'ignore']
      })

      this.#mailProcess.unref()

      if (this.#mailProcess.pid) {
        try {
          const flags = isUpdateMode ? 'w' : 'wx'
          fs.writeFileSync(pidFile, this.#mailProcess.pid.toString(), {flag: flags})
          log(`Go Mail started with PID ${this.#mailProcess.pid}`)
        } catch (err) {
          if (err.code === 'EEXIST') {
            error(`Race condition detected: PID file ${pidFile} already exists. Stopping redundant Mail instance.`)
            this.#mailProcess.kill()
            this.#mailProcess = null
            return
          }
          throw err
        }
      }

      this.#mailProcess.on('exit', code => {
        error(`Go Mail exited with code ${code}`)
        this.#mailProcess = null
        try {
          fs.unlinkSync(pidFile)
        } catch {
          /* ignore */
        }
      })

      if (this.#syncTimer) clearTimeout(this.#syncTimer)
      this.#syncTimer = setTimeout(() => this.syncConfig(), 1000)

      // Cleanup previous instance files
      const prevId = process.env.ODAC_PREVIOUS_INSTANCE_ID
      if (prevId) {
        if (this.#cleanupTimer) clearTimeout(this.#cleanupTimer)
        this.#cleanupTimer = setTimeout(() => {
          log(`Cleaning up files from previous Mail instance: ${prevId}`)
          const prevPidFile = path.join(runDir, `mail-${prevId}.pid`)
          const prevSockFile = path.join(runDir, `mail-${prevId}.sock`)

          try {
            if (fs.existsSync(prevPidFile)) fs.unlinkSync(prevPidFile)
            if (fs.existsSync(prevSockFile)) fs.unlinkSync(prevSockFile)
            log(`Mail cleanup successful for instance ${prevId}`)
          } catch (e) {
            log(`Warning: Failed to cleanup previous Mail instance files: ${e.message}`)
          }
          this.#cleanupTimer = null
        }, 60000)
      }
    } catch (err) {
      error(`Failed to spawn Go Mail: ${err.message}`)
    }
  }

  /**
   * Starts the Mail service: spawns Go binary and syncs config.
   */
  start() {
    if (this.#active) return
    this.#active = true
    this.spawnMail()
  }

  /**
   * Stops the Mail service: kills the Go binary and cleans up.
   */
  stop() {
    this.#active = false
    if (this.#mailProcess) {
      this.#mailProcess.kill()
      this.#mailProcess = null
      this.#mailApiPort = null
      if (this.#mailSocketPath && fs.existsSync(this.#mailSocketPath)) {
        try {
          fs.unlinkSync(this.#mailSocketPath)
        } catch {
          /* ignore */
        }
      }
    }
    if (this.#syncTimer) {
      clearTimeout(this.#syncTimer)
      this.#syncTimer = null
    }
    if (this.#cleanupTimer) {
      clearTimeout(this.#cleanupTimer)
      this.#cleanupTimer = null
    }
  }

  /**
   * Syncs the full mail configuration to the Go binary.
   * Sends domains, SSL certs, DKIM keys, and IP data for PTR-based delivery.
   * @param {number} retryCount - Internal retry counter
   */
  async syncConfig(retryCount = 0) {
    log('Mail: syncConfig called (Retry: %d)', retryCount)

    if (!this.#mailProcess) return
    if (!this.#mailSocketPath && !this.#mailApiPort) return

    if (this.#mailSocketPath && !fs.existsSync(this.#mailSocketPath)) {
      return
    }

    const domains = Odac.core('Config').config.domains || {}
    const dnsConfig = Odac.core('Config').config.dns || {}
    const ssl = Odac.core('Config').config.ssl || {}

    // Build domain config for Go binary
    const mailDomains = {}
    for (const [name, record] of Object.entries(domains)) {
      const zone = dnsConfig[name]
      const hasMX = zone?.records?.some(r => r.type === 'MX')

      mailDomains[name] = {
        cert: record.cert || {},
        mxEnabled: !!hasMX,
        subdomains: record.subdomain || []
      }
    }

    // Build IP config for PTR-based outbound delivery
    const DNS = Odac.server('DNS')
    const normalizePtr = ip => ({
      ...ip,
      ptr: ip.ptr != null ? String(ip.ptr) : ''
    })

    const ips = {
      ipv4: (DNS?.ips?.ipv4 || []).map(normalizePtr),
      ipv6: (DNS?.ips?.ipv6 || []).map(normalizePtr),
      primary: DNS?.ip || '127.0.0.1'
    }

    const payload = {
      accounts: [],
      domains: mailDomains,
      hostname: os.hostname(),
      ips: ips,
      ssl: ssl
    }

    log('Mail: Syncing %d domains to Go binary', Object.keys(mailDomains).length)

    try {
      if (this.#mailSocketPath) {
        await Odac.core('Http').post('http://localhost/config', payload, {
          socketPath: this.#mailSocketPath,
          validateStatus: () => true
        })
      } else {
        await Odac.core('Http').post(`http://127.0.0.1:${this.#mailApiPort}/config`, payload)
      }
    } catch (e) {
      if (retryCount < 3 && (e.code === 'ECONNREFUSED' || e.code === 'ENOENT' || e.code === 'ECONNRESET')) {
        log(`Mail config sync failed (${e.code}). Retrying in 1s...`)
        await new Promise(r => setTimeout(r, 1000))
        return this.syncConfig(retryCount + 1)
      }
      error(`Failed to sync Mail config to Go binary: ${e.message}`)
    }
  }

  // --- Private Methods ---

  /**
   * Makes an HTTP request to the Go mail binary API.
   * @param {string} method - HTTP method
   * @param {string} endpoint - API endpoint path
   * @param {Object} [data] - Request body
   * @returns {Object} Response data
   */
  async #apiRequest(method, endpoint, data) {
    if (!this.#mailProcess) throw new Error('Mail process not running')
    if (!this.#mailSocketPath && !this.#mailApiPort) throw new Error('Mail API not available')

    const url = this.#mailSocketPath ? `http://localhost${endpoint}` : `http://127.0.0.1:${this.#mailApiPort}${endpoint}`

    const options = this.#mailSocketPath ? {socketPath: this.#mailSocketPath, validateStatus: () => true} : {validateStatus: () => true}

    let response
    switch (method) {
      case 'GET':
        response = await Odac.core('Http').get(url, options)
        break
      case 'POST':
        response = await Odac.core('Http').post(url, data, options)
        break
      case 'PUT':
        response = await Odac.core('Http').put(url, data, options)
        break
      case 'DELETE':
        response = await Odac.core('Http').delete(url, {...options, data})
        break
    }

    return response?.data
  }

  /**
   * Generates a 2048-bit RSA DKIM key pair using native crypto (non-blocking),
   * persists keys to disk, and publishes the public key as a DNS TXT record.
   * Stays in Node.js because it needs to create DNS records via Odac.server('DNS').
   * @param {string} domain - The domain to generate DKIM keys for
   */
  async #dkim(domain) {
    const generateKeyPairAsync = promisify(generateKeyPair)
    const {privateKey, publicKey} = await generateKeyPairAsync('rsa', {
      modulusLength: 2048,
      privateKeyEncoding: {format: 'pem', type: 'pkcs1'},
      publicKeyEncoding: {format: 'pem', type: 'spki'}
    })
    const dkimDir = os.homedir() + '/.odac/cert/dkim'
    const selector = 'default'
    await fsp.mkdir(dkimDir, {recursive: true})
    await fsp.writeFile(dkimDir + '/' + domain + '.key', privateKey)
    await fsp.chmod(dkimDir + '/' + domain + '.key', 0o600)
    await fsp.writeFile(dkimDir + '/' + domain + '.pub', publicKey)
    const publicKeyBase64 = publicKey
      .replace('-----BEGIN PUBLIC KEY-----', '')
      .replace('-----END PUBLIC KEY-----', '')
      .replace(/[\r\n]/g, '')
    if (!Odac.core('Config').config.domains[domain].cert) Odac.core('Config').config.domains[domain].cert = {}
    Odac.core('Config').config.domains[domain].cert.dkim = {
      private: dkimDir + '/' + domain + '.key',
      public: dkimDir + '/' + domain + '.pub',
      selector
    }
    Odac.server('DNS').record({
      type: 'TXT',
      name: `${selector}._domainkey.${domain}`,
      value: `v=DKIM1; k=rsa; p=${publicKeyBase64}`
    })
    if (Odac.core('Config').force) Odac.core('Config').force()
    // Sync updated DKIM config to Go binary
    this.syncConfig()
    log('DKIM 2048-bit keys generated for %s (selector: %s)', domain, selector)
  }
}

module.exports = new Mail()
