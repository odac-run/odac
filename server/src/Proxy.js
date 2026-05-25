const noop = () => {}
const {log, error} = typeof Odac !== 'undefined' && Odac.core ? Odac.core('Log', false).init('Proxy') : {log: noop, error: noop}

const childProcess = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')

class OdacProxy {
  #active = false
  #proxyApiPort = null
  #proxyProcess = null
  #proxySocketPath = null
  #syncTimer = null
  #cleanupTimer = null
  #tunnels = new Map()

  check() {
    if (!this.#active) return
    this.spawnProxy()
  }

  // Test helper
  reset() {
    this.#proxyProcess = null
    this.#proxySocketPath = null
    this.#proxyApiPort = null
  }

  /**
   * Purges cached static assets from the Go proxy's in-memory cache.
   * Called after app deployments to ensure fresh assets are served.
   * @param {string} [domain] - Domain to purge. If omitted, purges all cached assets.
   * @returns {Promise<number>} Number of purged cache entries
   */
  async purgeCache(domain) {
    if (!this.#proxyProcess) return 0
    if (!this.#proxySocketPath && !this.#proxyApiPort) return 0

    const payload = domain ? {domain} : {}

    try {
      let response
      if (this.#proxySocketPath) {
        response = await Odac.core('Http').post('http://localhost/cache/purge', payload, {
          socketPath: this.#proxySocketPath,
          validateStatus: () => true
        })
      } else {
        response = await Odac.core('Http').post(`http://127.0.0.1:${this.#proxyApiPort}/cache/purge`, payload)
      }

      const purged = response.data?.purged || 0
      if (purged > 0) log('Cache purged: %d entries%s', purged, domain ? ` for ${domain}` : '')
      return purged
    } catch (e) {
      error('Failed to purge cache: %s', e.message)
      return 0
    }
  }

  /**
   * Purges cached assets for all domains mapped to a specific app.
   * Called automatically after app redeploy/restart to serve fresh assets.
   * @param {string} appId - App name or ID to purge cache for
   */
  async purgeCacheForApp(appId) {
    if (!appId) return

    const domains = Odac.core('Config').config.domains || {}
    const purged = []

    for (const [domainName, record] of Object.entries(domains)) {
      if (record.appId === appId) {
        purged.push(this.purgeCache(domainName))
      }
    }

    await Promise.all(purged)
  }

  /**
   * Resolves an app's backend target (IP + port) from its config.
   * Single source of truth for port detection and container IP resolution.
   * Used by both domain routing and tunnel routing to avoid duplication.
   * @param {Object} app - App config object from apps array
   * @returns {{host: string, port: number}|null} Resolved backend or null if unresolvable
   */
  async #resolveAppBackend(app) {
    let port = 0
    let host = '127.0.0.1'
    let useInternal = false

    if (app.ports && app.ports.length > 0) {
      if (app.ports[0].host) {
        port = parseInt(app.ports[0].host)
      } else if (app.ports[0].container) {
        port = parseInt(app.ports[0].container)
        useInternal = true
      }
    } else if (app.port) {
      port = parseInt(app.port)
    } else if (app.http && app.http !== false) {
      port = parseInt(app.http)
      useInternal = true
    }

    if (!port) return null

    if (useInternal) {
      try {
        const targetContainerName = app.activeContainerId || app.name
        const containerIP = await Odac.server('Container').getIP(targetContainerName)

        if (containerIP) {
          app.ip = containerIP
          host = containerIP
        } else if (app.ip) {
          host = app.ip
        }
      } catch {
        if (app.ip) host = app.ip
      }
    }

    return {host, port, useInternal}
  }

  /**
   * Replaces the entire tunnel configuration with the provided list.
   * Hub always sends the full tunnel list — missing entries are treated as deleted,
   * new entries are additions. This is a full-replace (reconciliation) operation.
   * @param {Array<{domain: string, token: string, container: string}>} tunnels - Complete tunnel list from Hub
   */
  setTunnels(tunnels) {
    if (!Array.isArray(tunnels)) return Odac.server('Api').result(false, 'Invalid tunnels payload')

    const incoming = new Map()
    for (const t of tunnels) {
      if (t.domain && t.token && t.container) {
        incoming.set(t.domain, {container: t.container, token: t.token})
      }
    }

    // Full replace — incoming list is the single source of truth
    this.#tunnels = incoming

    // Persist: overwrite entirely so removed tunnels are cleaned from disk
    const persist = {}
    for (const [domain, val] of this.#tunnels) {
      persist[domain] = val
    }
    Odac.core('Config').config.tunnels = persist

    log('Tunnel config replaced: %d tunnel(s)', this.#tunnels.size)

    // Sync immediately so Go proxy learns about tunnel domains
    this.syncConfig()
    return Odac.server('Api').result(true, __('%d tunnel(s) configured', this.#tunnels.size))
  }

  spawnProxy() {
    if (this.#proxyProcess) return

    const isWindows = os.platform() === 'win32'
    const proxyName = isWindows ? 'odac-proxy.exe' : 'odac-proxy'
    const binPath = path.resolve(__dirname, '../../bin', proxyName)
    const runDir = path.join(os.homedir(), '.odac', 'run')

    if (!fs.existsSync(runDir)) fs.mkdirSync(runDir, {recursive: true})

    const instanceId = process.env.ODAC_INSTANCE_ID || 'default'
    const pidFile = path.join(runDir, `proxy-${instanceId}.pid`)

    // Set socket path
    if (!isWindows) {
      this.#proxySocketPath = path.join(runDir, `proxy-${instanceId}.sock`)
    }

    // 1. Try to adopt existing process
    // We try to read directly to avoid TOCTOU race condition (checking existence then reading)
    // SKIP adoption if we are in Update Mode (we need a fresh proxy)
    const isUpdateMode = process.env.ODAC_UPDATE_MODE === 'true'

    if (!isUpdateMode) {
      try {
        const pid = parseInt(fs.readFileSync(pidFile, 'utf8'))
        // 1. Check if PID exists/running
        process.kill(pid, 0)

        // 2. Validate it's actually our Proxy (check if socket/port is active)
        // If we are in Socket mode, the socket file MUST exist
        if (!isWindows) {
          if (!fs.existsSync(this.#proxySocketPath)) {
            log(`PID ${pid} exists but socket file is missing. PID reuse detected or proxy crashed. Ignoring orphan...`)
            // We don't kill the process because it might be a random system process reusing the PID
            // Just clean the PID file and proceed to spawn new
            try {
              fs.unlinkSync(pidFile)
            } catch {
              /* ignore */
            }
            throw new Error('Socket missing') // Break to catch block to spawn new
          }
        }

        // 3. Double Check: Verify process name to be sure it is 'odac-proxy'
        // This prevents connecting to a random process that might have reused the PID
        // SECURITY: PID Reuse Attack Vector Mitigation
        try {
          // Simple check: If we can read /proc/PID/cmdline (Linux)
          const procPath = `/proc/${pid}/cmdline`
          if (fs.existsSync(procPath)) {
            const cmdline = fs.readFileSync(procPath, 'utf8')
            if (!cmdline.includes('odac-proxy')) {
              log(`PID ${pid} is active but command line does not match proxy. PID reuse detected!`)
              try {
                fs.unlinkSync(pidFile)
              } catch {
                /* ignore */
              }
              throw new Error('PID reuse detected')
            }
          }
        } catch (e) {
          // If we can't read proc (e.g. permission or Mac), we rely on Socket check above
          if (e.message === 'PID reuse detected') throw e
        }

        log(`Found orphaned Go Proxy (PID: ${pid}). Reconnecting...`)

        // Create a fake process object to manage it
        this.#proxyProcess = {
          pid,
          kill: () => {
            try {
              process.kill(pid)
            } catch {
              /* ignore */
            }
          }
        }

        // Give a moment for other services to initialize (Container, etc)
        if (this.#syncTimer) clearTimeout(this.#syncTimer)
        this.#syncTimer = setTimeout(() => this.syncConfig(), 1000)
        return
      } catch (err) {
        // Logic for when we fail to adopt the process
        if (err.code !== 'ENOENT') {
          // If error is NOT "File not found", it means file exists but maybe corrupt or process dead
          log(`Orphaned proxy PID file issue. Cleaning up.`)
          try {
            fs.unlinkSync(pidFile)
          } catch {
            /* ignore */
          }
        }
        // If err.code IS 'ENOENT', it simply means no PID file exists, so we proceed to start a new one.
      }
    } else {
      log('Update mode detected. Forcing new Proxy instance spawn...')
    }

    if (!fs.existsSync(binPath)) {
      error(`Go proxy binary not found at ${binPath}. Please run 'go build -o bin/${proxyName} ./server/proxy'`)
      return
    }

    // 2. Start new Proxy
    let env = {...process.env}

    if (!isWindows) {
      env.ODAC_SOCKET_PATH = this.#proxySocketPath
      log(`Starting Go Proxy (Socket: ${this.#proxySocketPath})...`)
    } else {
      log(`Starting Go Proxy (TCP Mode)...`)
    }

    try {
      // Go binary writes its own log to ~/.odac/logs/proxy.log with size-based
      // rotation (server/proxy/logrotate.go). stdio fully discarded so a Node
      // restart can't SIGPIPE the detached process during orphan adoption.
      this.#proxyProcess = childProcess.spawn(binPath, [], {
        detached: true,
        stdio: ['ignore', 'ignore', 'ignore'],
        env: env
      })

      this.#proxyProcess.unref()

      if (this.#proxyProcess.pid) {
        try {
          // Use 'wx' flag to ensure we don't overwrite a PID file created by a concurrent process
          // This resolves the TOCTOU (Time-of-check to time-of-use) race condition
          // UNLESS in update mode, where we deliberately take over.
          const flags = isUpdateMode ? 'w' : 'wx'
          fs.writeFileSync(pidFile, this.#proxyProcess.pid.toString(), {flag: flags})
          log(`Go Proxy started with PID ${this.#proxyProcess.pid}`)
        } catch (err) {
          if (err.code === 'EEXIST') {
            error(`Race condition detected: PID file ${pidFile} already exists. Stopping redundant proxy instance.`)
            // Kill the process we just spawned as it is a duplicate/redundant
            this.#proxyProcess.kill()
            this.#proxyProcess = null
            return
          }
          throw err
        }
      }

      this.#proxyProcess.on('exit', code => {
        error(`Go Proxy exited with code ${code}`)
        this.#proxyProcess = null
        try {
          fs.unlinkSync(pidFile)
        } catch {
          /* ignore */
        }
      })

      // Give it a moment to start, then sync config
      if (this.#syncTimer) clearTimeout(this.#syncTimer)
      this.#syncTimer = setTimeout(() => this.syncConfig(), 1000)

      // 3. Cleanup Previous Instance Files (Garbage Collection)
      const prevId = process.env.ODAC_PREVIOUS_INSTANCE_ID
      if (prevId) {
        // Wait for handover to definitely complete (60s)
        if (this.#cleanupTimer) clearTimeout(this.#cleanupTimer)
        this.#cleanupTimer = setTimeout(() => {
          log(`Cleaning up files from previous instance: ${prevId}`)
          const prevPidFile = path.join(runDir, `proxy-${prevId}.pid`)
          const prevSockFile = path.join(runDir, `proxy-${prevId}.sock`)

          try {
            if (fs.existsSync(prevPidFile)) fs.unlinkSync(prevPidFile)
            if (fs.existsSync(prevSockFile)) fs.unlinkSync(prevSockFile)
            log(`Cleanup successful for region ${prevId}`)
          } catch (e) {
            log(`Warning: Failed to cleanup previous instance files: ${e.message}`)
          }
          this.#cleanupTimer = null
        }, 60000)
      }
    } catch (err) {
      error(`Failed to spawn Go Proxy: ${err.message}`)
    }
  }

  /**
   * Removes an ACME HTTP-01 challenge token from the Go proxy.
   * Called after Let's Encrypt completes or abandons validation.
   * @param {string} token - The ACME challenge token to remove
   */
  async deleteACMEChallenge(token) {
    if (!this.#proxyProcess) return
    if (!this.#proxySocketPath && !this.#proxyApiPort) return

    try {
      const payload = {token}
      if (this.#proxySocketPath) {
        await Odac.core('Http').delete('http://localhost/acme/challenge', {
          data: payload,
          socketPath: this.#proxySocketPath,
          validateStatus: () => true
        })
      } else {
        await Odac.core('Http').delete(`http://127.0.0.1:${this.#proxyApiPort}/acme/challenge`, {data: payload})
      }
    } catch (e) {
      if (typeof error !== 'undefined') error('Failed to delete ACME challenge token: %s', e.message)
    }
  }

  /**
   * Sends an ACME HTTP-01 challenge token to the Go proxy for serving.
   * The proxy will respond to Let's Encrypt at /.well-known/acme-challenge/{token}.
   * @param {string} token - The ACME challenge token
   * @param {string} keyAuthorization - The key authorization string to serve
   */
  async setACMEChallenge(token, keyAuthorization) {
    if (!this.#proxyProcess) throw new Error('Proxy process not running')
    if (!this.#proxySocketPath && !this.#proxyApiPort) throw new Error('Proxy API not available')

    const payload = {keyAuthorization, token}
    let response

    if (this.#proxySocketPath) {
      response = await Odac.core('Http').post('http://localhost/acme/challenge', payload, {
        socketPath: this.#proxySocketPath,
        validateStatus: () => true
      })
    } else {
      response = await Odac.core('Http').post(`http://127.0.0.1:${this.#proxyApiPort}/acme/challenge`, payload, {
        validateStatus: () => true
      })
    }

    if (response.status !== 200) {
      throw new Error(`Proxy returned HTTP ${response.status} for ACME challenge`)
    }
  }

  start() {
    this.#active = true

    // Restore persisted tunnel config from previous session
    const saved = Odac.core('Config').config.tunnels
    if (saved && typeof saved === 'object') {
      for (const [domain, val] of Object.entries(saved)) {
        if (val && val.token && val.container) {
          this.#tunnels.set(domain, val)
        }
      }
      if (this.#tunnels.size > 0) {
        log('Restored %d tunnel(s) from config', this.#tunnels.size)
      }
    }

    this.spawnProxy()
  }

  stop() {
    this.#active = false
    if (this.#proxyProcess) {
      this.#proxyProcess.kill() // SIGTERM
      this.#proxyProcess = null
      this.#proxyApiPort = null
      if (this.#proxySocketPath && fs.existsSync(this.#proxySocketPath)) {
        try {
          fs.unlinkSync(this.#proxySocketPath)
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
   * Waits until the Go Proxy binary's public listeners (:80 and :443) are
   * bound and accepting connections. Used by the Updater during the
   * zero-downtime handshake to confirm the new instance is actually serving
   * traffic before the old one releases its overlap listeners.
   *
   * Polls the binary's /ready endpoint over the management socket. The
   * endpoint returns 200 only after both TCP listeners are bound — this is
   * the authoritative signal, not a syncConfig success or a fixed sleep.
   * Config sync is then performed once readiness is confirmed.
   *
   * @param {number} [timeout=10000] - Maximum wait time in milliseconds
   * @returns {Promise<boolean>} True if ready, false on timeout
   */
  async waitForReady(timeout = 10000) {
    const start = Date.now()
    const interval = 200

    while (Date.now() - start < timeout) {
      if (this.#proxyProcess && (this.#proxySocketPath || this.#proxyApiPort)) {
        try {
          let response
          if (this.#proxySocketPath && fs.existsSync(this.#proxySocketPath)) {
            response = await Odac.core('Http').get('http://localhost/ready', {
              socketPath: this.#proxySocketPath,
              validateStatus: () => true,
              timeout: 1000
            })
          } else if (this.#proxyApiPort) {
            response = await Odac.core('Http').get(`http://127.0.0.1:${this.#proxyApiPort}/ready`, {
              validateStatus: () => true,
              timeout: 1000
            })
          }

          if (response && response.status === 200) {
            // Public listeners bound — push config now so the binary serves
            // requests with up-to-date routing the moment it accepts them.
            await this.syncConfig()
            return true
          }
        } catch {
          /* retry */
        }
      }
      await new Promise(r => setTimeout(r, interval))
    }
    return false
  }

  async syncConfig(retryCount = 0) {
    if (typeof log !== 'undefined') log('Proxy: syncConfig called (Retry: %d)', retryCount)

    if (typeof Odac === 'undefined') return
    if (!this.#proxyProcess) return
    if (!this.#proxySocketPath && !this.#proxyApiPort) return

    // Ensure socket exists before sending
    if (this.#proxySocketPath && !fs.existsSync(this.#proxySocketPath)) {
      // Socket not ready yet
      return
    }

    const domains = Odac.core('Config').config.domains || {}
    const apps = Odac.core('Config').config.apps || []

    // Safety: If we have container apps, wait for Docker to be available
    // to prevent falling back to 127.0.0.1 during startup.
    const hasContainerApps = apps.some(a => a.ports?.some(p => p.container && !p.host))
    if (hasContainerApps && retryCount === 0) {
      const container = Odac.server('Container')
      let waits = 0
      // Wait up to 3 seconds for Docker to become available
      while (!container.available && waits < 30) {
        await new Promise(r => setTimeout(r, 100))
        waits++
      }
    }

    const proxyDomains = {}

    // Process all domains in parallel for maximum throughput (Enterprise Scale)
    await Promise.all(
      Object.entries(domains).map(async ([domainName, record]) => {
        const app = apps.find(a => a.name === record.appId || a.id === record.appId)

        if (!app) {
          if (typeof log !== 'undefined') log('Proxy: App %s not found for domain %s', record.appId, domainName)
          return
        }

        const backend = await this.#resolveAppBackend(app)
        if (!backend) {
          if (typeof log !== 'undefined') log('Proxy: No port found for app %s (domain: %s)', app.name, domainName)
          return
        }

        if (typeof log !== 'undefined') {
          const targetType = backend.useInternal ? 'Container Network' : 'Host Network'
          log('Proxy: Routing domain [%s] -> App [%s] via %s (%s:%d)', domainName, app.name, targetType, backend.host, backend.port)
        }

        proxyDomains[domainName] = {
          domain: domainName,
          port: backend.port,
          subdomain: record.subdomain || [],
          cert: record.cert || {},
          container: backend.useInternal ? backend.host : undefined,
          containerIP: backend.host
        }
      })
    )

    // Resolve tunnel backends using the same shared helper
    const tunnelList = []
    for (const [domain, val] of this.#tunnels) {
      const app = apps.find(a => a.name === val.container)
      if (!app) {
        log('Tunnel: App not found for container %s (domain: %s)', val.container, domain)
        continue
      }

      const backend = await this.#resolveAppBackend(app)
      if (!backend) {
        log('Tunnel: No port found for %s (domain: %s)', val.container, domain)
        continue
      }

      tunnelList.push({domain, host: backend.host, port: backend.port, token: val.token})
    }

    if (typeof log !== 'undefined') log('Proxy: Syncing %d domains, %d tunnels', Object.keys(proxyDomains).length, tunnelList.length)

    const config = {
      domains: proxyDomains,
      firewall: Odac.core('Config').config.firewall || {enabled: true},
      memory: {total: os.totalmem(), used: os.totalmem() - os.freemem()},
      ssl: Odac.core('Config').config.ssl || null,
      tunnels: tunnelList
    }

    try {
      if (this.#proxySocketPath) {
        // Unix Socket Request
        await Odac.core('Http').post('http://localhost/config', config, {
          socketPath: this.#proxySocketPath,
          validateStatus: () => true
        })
      } else {
        // TCP Request
        await Odac.core('Http').post(`http://127.0.0.1:${this.#proxyApiPort}/config`, config)
      }
    } catch (e) {
      if (retryCount < 3 && (e.code === 'ECONNREFUSED' || e.code === 'ENOENT' || e.code === 'ECONNRESET')) {
        log(`Config sync failed (${e.code}). Retrying in 1s...`)
        await new Promise(r => setTimeout(r, 1000))
        return this.syncConfig(retryCount + 1)
      }
      error(`Failed to sync config to proxy: ${e.message}`)
    }
  }

  // Removed #handleUpgrade as it is handled by Go Proxy
}

module.exports = new OdacProxy()
