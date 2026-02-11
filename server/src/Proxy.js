const noop = () => {}
const {log, error} = typeof Odac !== 'undefined' && Odac.core ? Odac.core('Log', false).init('Proxy') : {log: noop, error: noop}

const childProcess = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')
const axios = require('axios')

class OdacProxy {
  #active = false
  #proxyProcess = null
  #proxySocketPath = null
  #proxyApiPort = null

  check() {
    if (!this.#active) return
    this.spawnProxy()
  }

  spawnProxy() {
    if (this.#proxyProcess) return

    const isWindows = os.platform() === 'win32'
    const proxyName = isWindows ? 'odac-proxy.exe' : 'odac-proxy'
    const binPath = path.resolve(__dirname, '../../bin', proxyName)
    const runDir = path.join(os.homedir(), '.odac', 'run')
    const logDir = path.join(os.homedir(), '.odac', 'logs')

    if (!fs.existsSync(runDir)) fs.mkdirSync(runDir, {recursive: true})
    if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, {recursive: true})

    const instanceId = process.env.ODAC_INSTANCE_ID || 'default'
    const pidFile = path.join(runDir, `proxy-${instanceId}.pid`)
    const logFile = path.join(logDir, 'proxy.log')

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

        // Sync config immediately
        this.syncConfig()
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
      const logFd = fs.openSync(logFile, 'a')

      this.#proxyProcess = childProcess.spawn(binPath, [], {
        detached: true, // Allow running after parent exit
        stdio: ['ignore', logFd, logFd], // Redirect logs to file
        env: env
      })

      this.#proxyProcess.unref() // Don't prevent Node from exiting

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

      // Give it a moment to start
      setTimeout(() => this.syncConfig(), 1000)

      // 3. Cleanup Previous Instance Files (Garbage Collection)
      const prevId = process.env.ODAC_PREVIOUS_INSTANCE_ID
      if (prevId) {
        // Wait for handover to definitely complete (60s)
        setTimeout(() => {
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
        }, 60000)
      }
    } catch (err) {
      error(`Failed to spawn Go Proxy: ${err.message}`)
    }
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
    const proxyDomains = {}

    for (const [domainName, record] of Object.entries(domains)) {
      const app = apps.find(a => a.name === record.appId || a.id === record.appId)

      // If app is not running or not found, we can't proxy to it.
      // However, we should arguably still register it so we can show a "Bad Gateway" or "Not Running" page
      // instead of "Not Found" / Security rejection.
      // For now, consistent with enterprise "fail secure", we only proxy valid, running-capable targets.
      if (!app) {
        if (typeof log !== 'undefined') log('Proxy: App %s not found for domain %s', record.appId, domainName)
        continue
      }

      let port = 0
      let containerIP = ''
      let useInternal = false

      // Determine Port
      if (app.ports && app.ports.length > 0) {
        if (app.ports[0].host) {
          // Priority 1: Use host port if exposed (works for both Local loopback and Docker)
          port = parseInt(app.ports[0].host)
        } else if (app.ports[0].container) {
          // Priority 2: Use internal container port if no host port is exposed
          port = parseInt(app.ports[0].container)
          useInternal = true
        }
      }
      // Priority 3: 'port' property (Legacy or Script apps)
      else if (app.port) {
        port = parseInt(app.port)
      }

      // If no port found, we skip this domain as we don't know where to route it.
      if (!port) {
        if (typeof log !== 'undefined') log('Proxy: No port found for app %s (domain: %s)', app.name, domainName)
        continue
      }

      if (typeof log !== 'undefined')
        log('Proxy: Adding domain %s -> %s:%d (Container: %s)', domainName, app.name, port, useInternal ? 'Yes' : 'No')

      proxyDomains[domainName] = {
        domain: domainName,
        port: port,
        subdomain: record.subdomain || [],
        cert: record.cert || {}, // Pass full cert object (contains ssl: {key, cert})
        // Only send container name if we MUST use internal networking (no host port).
        // If we have a host port, we rely on 127.0.0.1 (default in Go) which is safer for dev environments on host.
        container: useInternal ? app.name : undefined,
        containerIP: containerIP // reserved for future use
      }
    }

    if (typeof log !== 'undefined') log('Proxy: Syncing %d domains', Object.keys(proxyDomains).length)

    const config = {
      domains: proxyDomains,
      firewall: Odac.core('Config').config.firewall || {enabled: true},
      ssl: Odac.core('Config').config.ssl || null
    }

    try {
      if (this.#proxySocketPath) {
        // Unix Socket Request
        await axios.post('http://localhost/config', config, {
          socketPath: this.#proxySocketPath,
          validateStatus: () => true
        })
      } else {
        // TCP Request
        await axios.post(`http://127.0.0.1:${this.#proxyApiPort}/config`, config)
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

  start() {
    this.#active = true
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
  }

  // Test helper
  reset() {
    this.#proxyProcess = null
    this.#proxySocketPath = null
    this.#proxyApiPort = null
  }
}

module.exports = new OdacProxy()
