const {log, error} = Odac.core('Log', false).init('App')
const fs = require('fs')
const http = require('http')
const path = require('path')
const net = require('net')
const nodeCrypto = require('crypto')
const childProcess = require('child_process')
const os = require('os')
const Logger = require('./Container/Logger')
const Deploy = require('./App/Deploy')
const Create = require('./App/Create')

const SCRIPT_EXTENSIONS = ['.js', '.py', '.php', '.sh', '.rb']
const SCRIPT_RUNNERS = {
  '.js': {image: 'node:lts-alpine', cmd: 'node', local: 'node'},
  '.py': {image: 'python:alpine', cmd: 'python3', local: 'python3', args: ['-u']},
  '.php': {image: 'php:cli-alpine', cmd: 'php', local: 'php'},
  '.rb': {image: 'ruby:alpine', cmd: 'ruby', local: 'ruby'},
  '.sh': {image: 'alpine:latest', cmd: 'sh', local: 'sh'}
}
const SENSITIVE_KEY_PATTERN = /cert|key|pass|salt|secret|token/i

class App {
  #apps = []
  #loaded = false
  #processing = new Set()
  #creating = new Set()
  #logStreams = new Map() // app.name -> logCtrl
  #loggers = new Map() // app.name -> Logger instance

  deploy
  creator

  constructor() {
    // Controlled internal API surface for Deploy/Create submodules.
    // Arrow functions preserve class scope so they can reach private fields,
    // which are otherwise inaccessible from object literals or other classes.
    const api = {
      // State references (Set/Map passed by reference — direct mutation is fine)
      processing: this.#processing,
      creating: this.#creating,
      loggers: this.#loggers,
      logStreams: this.#logStreams,

      // App list access
      getApps: () => this.#apps,
      addApp: app => {
        this.#apps.push(app)
        this.#saveApps()
      },
      filterApps: predicate => {
        this.#apps = this.#apps.filter(predicate)
        this.#saveApps()
      },

      // Single-app accessors
      get: id => this.#get(id),
      set: (id, updates) => this.#set(id, updates),
      saveApps: () => this.#saveApps(),
      getNextId: () => this.#getNextId(),
      generateUniqueName: name => this.#generateUniqueName(name),
      generateRuntimeId: prefix => this.#generateRuntimeId(prefix),
      getLoggerInstance: name => this.#getLoggerInstance(name),
      getGitMetadata: url => this.#getGitMetadata(url),
      preparePorts: ports => this.#preparePorts(ports),
      prepareVolumes: (volumes, appDir) => this.#prepareVolumes(volumes, appDir),

      // Runtime delegation (Create uses these to start fresh apps)
      run: (id, logCtrl) => this.#run(id, logCtrl),
      runGitApp: app => this.#runGitApp(app),
      attachLogger: app => this.#attachLogger(app),
      scanAndSaveHttpStatus: app => this.#scanAndSaveHttpStatus(app),
      stop: id => this.stop(id)
    }

    this.deploy = new Deploy(api)
    this.creator = new Create(api)
  }

  // Lifecycle
  async init() {
    log('Initializing apps...')

    const appPath = Odac.core('Config').config.app?.path
    try {
      if (!appPath) {
        if (!Odac.core('Config').config.app) Odac.core('Config').config.app = {}

        // Check environment variable first (Docker support)
        if (process.env.ODAC_APPS_PATH) {
          Odac.core('Config').config.app.path = process.env.ODAC_APPS_PATH
        } else if (os.platform() === 'win32' || os.platform() === 'darwin') {
          Odac.core('Config').config.app.path = os.homedir() + '/Odac/apps/'
        } else {
          // Default for Linux (Prod & Dev)
          // We prefer relative path inside the container/app structure
          Odac.core('Config').config.app.path = '/app/.odac/apps/'
        }
      }

      // Ensure directory exists
      await fs.promises.mkdir(Odac.core('Config').config.app.path, {recursive: true})
    } catch (e) {
      if (e.code !== 'EEXIST') {
        error('Failed to create apps directory: %s', e.message)
      }
    }
    this.#apps = this.#loadAppsFromConfig()
    this.#loaded = true

    // Best-effort sweep — green log dirs are short-lived by design, so any
    // surviving at startup is an orphan from a past Blue-Green deploy.
    this.deploy.cleanupStaleGreenLogs().catch(e => error('Stale green log sweep failed: %s', e.message))
  }

  async check() {
    this.#apps = this.#loadAppsFromConfig()
    let triggeredRun = false

    for (const app of this.#apps) {
      if (!app.active) continue

      // If we are already processing this app, skip watchdog pulse for it
      if (this.#processing.has(app.id)) continue

      const isRunning = await this.#isAppRunning(app)

      // Re-attach logger for running apps (if missing)
      if (isRunning && !this.#logStreams.has(app.name)) {
        this.#attachLogger(app).catch(e => error('[Watchdog] Failed to reattach logger for %s: %s', app.name, e.message))
      }

      if (!isRunning && app.status === 'running') {
        log('App %s is not running. Restarting...', app.name)
        this.#run(app.id)
        triggeredRun = true
      } else if (!isRunning && !['stopped', 'errored', 'starting', 'installing'].includes(app.status)) {
        this.#run(app.id)
        triggeredRun = true
      }
    }

    if (triggeredRun) {
      await new Promise(resolve => setImmediate(resolve))
    }
  }

  // CRUD Operations
  async start(file) {
    if (!file?.length) {
      return Odac.server('Api').result(false, __('App file not specified.'))
    }

    file = path.resolve(file)

    if (!fs.existsSync(file)) {
      return Odac.server('Api').result(false, __('App file %s not found.', file))
    }

    const existing = this.#apps.find(s => s.file === file)

    if (!existing) {
      const app = this.#add(file, 'script')
      await this.#run(app.id)
      return Odac.server('Api').result(true, __('App %s added successfully.', file))
    }

    if (existing.status !== 'running') {
      await this.#run(existing.id)
      return Odac.server('Api').result(true, __('App %s started successfully.', existing.name))
    }

    return Odac.server('Api').result(false, __('App %s already exists and is running.', file))
  }

  async create(config) {
    return this.creator.create(config)
  }

  async #runGitApp(app) {
    // Canonical identity for API tokens & resource paths (survives Blue-Green rename)
    const appIdentity = app._appIdentity || app.name

    const volumes = [...(app.volumes || [])]
    if (app.dev) {
      // Mount total app directory to /app for live development
      const appDir = path.join(Odac.core('Config').config.app.path, appIdentity)
      volumes.push({host: appDir, container: '/app'})
    }

    // Safety check for legacy apps without ports config
    let port = 3000
    if (app.ports && app.ports.length > 0 && app.ports[0].container) {
      port = app.ports[0].container
    } else {
      // Legacy fix: If no port in config, assume 3000 and SAVE IT
      // so Proxy service can see it immediately
      log('Legacy App Fix: Assigning default port 3000 to app %s', app.name)
      app.ports = [{container: 3000}]
      this.#saveApps()
    }

    const env = this.#resolveEnv(app)

    // API Permission Injection
    if (app.api) {
      const api = Odac.server('Api')
      env.ODAC_API_KEY = api.generateAppToken(appIdentity, app.api)

      // Mount API Socket
      if (api.hostSocketDir) {
        volumes.push({host: api.hostSocketDir, container: '/odac:ro'})
        env.ODAC_API_SOCKET = '/odac/api.sock'
      }
    }

    // Inject PORT
    env.PORT = port.toString()

    const runOptions = {
      image: app.image,
      ports: [],
      volumes,
      devices: app.devices || [],
      env
    }

    if (app.cmd) runOptions.cmd = app.cmd

    // Enterprise Security Exception:
    // In Dev Mode, we mount the host directory which is owned by the host user/root.
    // To prevent 'EACCES: permission denied' when the container tries to write (npm install, logs),
    // we must run the container as root. This is acceptable for development environments.
    if (app.dev) {
      log('Active Dev Mode detected for %s. Forcing container to run as ROOT to handle volume permissions.', app.name)
      runOptions.user = 'root'
    }

    // Fix volume permissions before starting the app container.
    await this.#fixVolumePermissions(volumes)

    await Odac.server('Container').runApp(app.name, runOptions)

    // Start Runtime Logging
    await this.#attachLogger(app)

    // Runtime Port Discovery:
    // If we relied on a default (3000) but didn't actually detect it from image,
    // let's verify if the app is actually listening there or somewhere else.
    // This handles apps that ignore PORT env var (like n8n, ComfyUI).

    this.#pollForPort(app, port)
  }

  async #pollForPort(app, expectedPort, attempts = 0) {
    if (attempts >= 60) return // Give up after 60 seconds (selfhosted apps may take longer to bind)

    try {
      const container = Odac.server('Container')
      const listeningPorts = await container.getListeningPorts(app.name)

      if (listeningPorts.length > 0) {
        // App has started listening!

        // If expected port is found, we are matched and good to go.
        if (listeningPorts.includes(expectedPort)) return

        // If expected port is NOT found (but some other port is),
        // we should not immediately jump to config update.
        // It might be an ephemeral/debug port (like 45769) while the main port (3000) is starting.
        // We give it 5 seconds (attempts < 5) to see if the expected port appears.
        if (attempts >= 5) {
          let preferred = null

          // HTTP Probe: When multiple ports are open, identify the actual HTTP port
          // by sending a HEAD request. DB/Redis/WS-only ports won't respond to HTTP.
          const containerIP = await container.getIP(app.name)
          if (containerIP && listeningPorts.length > 1) {
            preferred = await this.#detectHttpPort(containerIP, listeningPorts)
            if (preferred) {
              log('Auto-Discovery: HTTP probe identified port %d for app %s', preferred, app.name)
            }
          }

          // Fallback: well-known HTTP ports, then first available
          if (!preferred) {
            preferred = listeningPorts.find(p => [80, 8080, 3000, 5000].includes(p)) || listeningPorts[0]
          }

          log('Auto-Discovery: App %s is listening on port %d (expected %d). Updating config...', app.name, preferred, expectedPort)
          app.ports = [{container: preferred}]

          // Cache container IP for zero-downtime Proxy routing
          if (containerIP) app.ip = containerIP

          this.#saveApps()

          // Trigger Proxy Sync to apply new port/IP
          Odac.server('Proxy').syncConfig()
          return
        }

        // Return fallthrough to setTimeout for retry
      }
    } catch {
      // Ignore errors during polling (container might not be ready yet)
    }

    // Retry after 1 second
    setTimeout(() => this.#pollForPort(app, expectedPort, attempts + 1), 1000)
  }

  /**
   * Probes multiple container ports to detect which one speaks HTTP.
   * Sends a minimal HEAD request to each port in parallel with a short timeout.
   * Non-HTTP services (databases, message queues, raw TCP) will not respond with valid HTTP.
   *
   * @param {string} ip - Container IP address (Docker network)
   * @param {number[]} ports - Array of listening ports to probe
   * @param {number} [timeout=1500] - Per-port probe timeout in ms
   * @returns {Promise<number|null>} The detected HTTP port, or null if none found
   */
  async #detectHttpPort(ip, ports, timeout = 2500) {
    const probes = ports.map(
      port =>
        new Promise(resolve => {
          const req = http.request(
            {
              hostname: ip,
              method: 'HEAD',
              path: '/',
              port,
              timeout
            },
            res => {
              // Any HTTP response (even 4xx/5xx) confirms this port speaks HTTP
              res.destroy()
              resolve(port)
            }
          )

          req.on('error', () => resolve(null))
          req.on('timeout', () => {
            req.destroy()
            resolve(null)
          })

          req.end()
        })
    )

    const results = await Promise.all(probes)
    const httpPorts = results.filter(p => p !== null)

    if (httpPorts.length === 0) return null

    // If multiple ports respond to HTTP, prefer well-known HTTP ports
    return httpPorts.find(p => [80, 443, 8080, 3000, 5000, 8000].includes(p)) || httpPorts[0]
  }

  async stopAll() {
    log('Stopping all apps...')
    for (const app of [...this.#apps]) {
      await this.stop(app.id)
    }
  }

  #getLoggerInstance(appName) {
    let logger = this.#loggers.get(appName)
    if (!logger) {
      logger = new Logger(appName)
      this.#loggers.set(appName, logger)
    }
    return logger
  }

  async #getLogger(appName) {
    const logger = this.#getLoggerInstance(appName)
    // Ensure initialized (safe to call multiple times)
    await logger.init()
    return logger
  }

  async stop(id) {
    const app = this.#get(id)
    if (!app) return Odac.server('Api').result(false, __('App ID %s not found.', id))

    if (app.status === 'stopped') {
      return Odac.server('Api').result(true, __('App %s is already stopped.', app.name))
    }

    try {
      if (app.pid) {
        try {
          process.kill(app.pid)
        } catch (e) {
          if (e.code !== 'ESRCH') throw e
        }
      }

      if (Odac.server('Container').available) {
        await Odac.server('Container').stop(app.name)
      }

      this.#set(app.id, {status: 'stopped', pid: null, active: false})

      // Cleanup log stream
      const logCtrl = this.#logStreams.get(app.name)
      if (logCtrl && typeof logCtrl.end === 'function') {
        logCtrl.end()
      }
      this.#logStreams.delete(app.name)

      return Odac.server('Api').result(true, __('App %s stopped.', app.name))
    } catch (e) {
      return Odac.server('Api').result(false, e.message)
    }
  }

  /**
   * Subscribes to realtime logs of an application
   * @param {string} appName
   * @param {function} callback ({t, d, ts}) => void
   * @returns {function} unsubscribe
   */
  subscribeToLogs(appName, callback) {
    // Determine active stream presence (optional, but good for check)
    // We allow subscription even if stream is momentarily down (restarting)
    let logger = this.#loggers.get(appName)

    if (!logger) {
      if (!this.#logStreams.has(appName)) {
        // If no active stream and no logger, check if app exists at all
        const app = this.#apps.find(a => a.name === appName)
        if (!app) {
          log('No log stream found for %s. Active: %s', appName, [...this.#logStreams.keys()].join(','))
          return null
        }
        // Create logger for known app (even if stopped)
        logger = new Logger(appName)
        this.#loggers.set(appName, logger)
        // Fire and forget init, not strictly needed for subscribe
        logger.init().catch(() => {})
      } else {
        // Should not happen: stream exists but logger doesn't (legacy/race)
        // Recover by creating one
        logger = new Logger(appName)
        this.#loggers.set(appName, logger)
      }
    }

    // Subscribe using Logger's mechanism
    return logger.subscribe(callback, 'runtime')
  }

  async delete(id, {purge = true} = {}) {
    const app = this.#get(id)

    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    log('Deleting app %s (force-cleanup any in-flight blue/green containers)', app.name)

    // User has already double-confirmed in the UI — delete must succeed regardless of
    // restart/redeploy/create state. Releasing in-flight locks lets concurrent flows
    // abandon gracefully; mid-flight #set() calls become no-ops once we drop the app
    // from #apps below.
    this.#processing.delete(app.id)
    this.#creating.delete(app.name)

    const container = Odac.server('Container')

    try {
      await this.stop(app.id)
    } catch (e) {
      error('Delete[%s]: stop failed: %s', app.name, e.message)
    }

    if (container.available) {
      try {
        await container.remove(app.name)
      } catch (e) {
        error('Delete[%s]: remove failed: %s', app.name, e.message)
      }
    }

    // Sweep any Blue-Green companions left behind by an in-flight ZDD deploy.
    // Without this, a green container can survive as a ghost under its temporary
    // `<app.name>-green-<ts>_<hex>` name (or worse, get renamed to app.name after we
    // already removed it). Targets: app.activeContainerId + Docker name pattern match.
    await this.deploy.sweepGreenContainersFor(app.name, app.activeContainerId)

    this.#apps = this.#apps.filter(s => s.name !== app.name && s.id !== app.id)
    this.#saveApps()

    const logCtrl = this.#logStreams.get(app.name)
    if (logCtrl && typeof logCtrl.end === 'function') logCtrl.end()
    this.#logStreams.delete(app.name)

    const logger = this.#loggers.get(app.name)
    this.#loggers.delete(app.name)
    try {
      container.unregisterBuildLogger(app.name)
    } catch {
      /* ignore */
    }

    if (purge) {
      if (logger) await logger.destroy()
      try {
        const appDir = path.join(Odac.core('Config').config.app.path, app.name)
        await fs.promises.rm(appDir, {recursive: true, force: true})
      } catch (e) {
        error('Failed to remove app directory for %s: %s', app.name, e.message)
      }
    }

    // Cascading delete: Remove associated domains
    try {
      await Odac.server('Domain').deleteByApp(app.name)
    } catch (e) {
      error('Failed to delete domains for app %s: %s', app.name, e.message)
    }

    // Notify Hub
    Odac.server('Hub').trigger('app.list')

    return Odac.server('Api').result(true, __('App %s deleted successfully.', app.name))
  }

  async restart(id) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    log('Restarting app %s', app.name)

    // Check if Zero-Downtime Deployment (ZDD) is applicable.
    // ZDD requires: 1) Git-based app (deterministic image builds), 2) At least one domain for Proxy routing.
    // Container (recipe) apps are excluded — they use pre-built images where Blue-Green adds
    // complexity without benefit (no build step, just pull & restart).
    const domainsConfig = Odac.core('Config').config.domains || {}
    const hasDomains = Object.values(domainsConfig).some(d => d.appId === app.name || d.appId === app.id)

    if (app.type === 'git' && hasDomains) {
      log('ZDD enabled for %s (Has Domains). Executing Blue-Green restart.', app.name)

      // Concurrency guard: prevent parallel operations on the same app
      if (this.#processing.has(app.id)) {
        return Odac.server('Api').result(false, __('App %s is already being processed.', app.name))
      }

      let greenContainerName = null
      this.#processing.add(app.id)

      try {
        greenContainerName = `${app.name}-green-${this.#generateRuntimeId()}`
        await this.deploy.performBlueGreenDeploy(app, greenContainerName, {
          operation: 'Restart',
          runGreenContainer: async () => {
            const greenApp = {...app, name: greenContainerName, _appIdentity: app.name}
            if (app.type === 'git') {
              await this.#runGitApp(greenApp)
            } else {
              await this.#runContainer(greenApp)
            }
          },
          setStarting: true
        })

        // Notify Hub
        Odac.server('Hub').trigger('app.list')
        return Odac.server('Api').result(true, __('App %s restarted successfully with zero-downtime.', app.name))
      } catch (e) {
        error('Failed to restart app %s with ZDD: %s', app.name, e.message)
        this.#set(app.id, {status: 'errored', updated: Date.now()})
        return Odac.server('Api').result(false, __('Failed to restart app %s: %s', app.name, e.message))
      } finally {
        this.#processing.delete(app.id)
      }
    }

    // Standard Restart (No Domains or Script App)
    // Concurrency guard: prevent parallel operations on the same app
    if (this.#processing.has(app.id)) {
      return Odac.server('Api').result(false, __('App %s is already being processed.', app.name))
    }

    this.#processing.add(app.id)
    log('Standard restart for %s (No Domains or Script App). Stopping old container first.', app.name)

    try {
      // Stop the app first
      await this.stop(app.id)

      // Wait a brief moment to ensure resources are released (optional but often helpful in container envs)
      await new Promise(resolve => setTimeout(resolve, 1000))

      // Start it again
      this.#set(id, {active: true})

      // Release the lock before #run() so it can acquire its own processing lock.
      // The outer guard already rejected concurrent callers at method entry.
      this.#processing.delete(app.id)

      if (await this.#run(app.id)) {
        // Notify Hub
        Odac.server('Hub').trigger('app.list')
        return Odac.server('Api').result(true, __('App %s restarted successfully.', app.name))
      }

      return Odac.server('Api').result(false, __('Failed to restart app %s.', app.name))
    } catch (e) {
      error('Failed to restart app %s: %s', app.name, e.message)
      this.#set(app.id, {status: 'errored', updated: Date.now()})
      return Odac.server('Api').result(false, __('Failed to restart app %s: %s', app.name, e.message))
    } finally {
      this.#processing.delete(app.id)
    }
  }

  async redeploy(payload) {
    const {container: appName, url, token, branch, commitSha} = payload

    if (!appName) {
      return Odac.server('Api').result(false, __('Missing container name'))
    }

    const app = this.#get(appName)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', appName))
    }

    if (app.type !== 'git') {
      return Odac.server('Api').result(false, __('Redeploy is only supported for git apps.'))
    }

    // Validate URL if overridden (same rules as #createFromGit)
    if (url) {
      if (/[;&|`$(){}<>\n\r]/.test(url)) {
        return Odac.server('Api').result(false, __('Invalid Git URL: Contains illegal characters.'))
      }
      if (!url.match(/^(https?|git|ssh|ftps?|rsync):\/\//) && !url.match(/^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/)) {
        return Odac.server('Api').result(false, __('Invalid Git URL: Unsupported protocol.'))
      }
    }

    // Validate commitSha format (hex-only, 6-40 chars)
    if (commitSha && !/^[a-f0-9]{6,40}$/i.test(commitSha)) {
      return Odac.server('Api').result(false, __('Invalid commit SHA format.'))
    }

    // Validate branch name: block git argument injection (--upload-pack) and shell metacharacters
    if (branch && (branch.startsWith('-') || /[;&|`$(){}<>\n\r]/.test(branch))) {
      return Odac.server('Api').result(false, __('Invalid branch name format.'))
    }
    const targetBranch = branch || app.branch || 'main'

    // Concurrency guard: prevent parallel operations on the same app
    if (this.#processing.has(app.id)) {
      return Odac.server('Api').result(false, __('App %s is already being processed.', app.name))
    }

    let greenContainerName = null
    this.#processing.add(app.id)

    // Register logger IMMEDIATELY to prevent race conditions with Hub requests
    const logger = this.#getLoggerInstance(app.name)
    Odac.server('Container').registerBuildLogger(app.name, logger)

    log('Redeploying app %s (branch: %s, commit: %s)', app.name, targetBranch, commitSha || 'HEAD')
    let logCtrl
    try {
      const appsPath = Odac.core('Config').config.app.path
      const appDir = path.join(appsPath, app.name)

      // Defense-in-depth: prevent path traversal before any destructive fs ops
      if (!path.resolve(appDir).startsWith(path.resolve(appsPath) + path.sep)) {
        return Odac.server('Api').result(false, __('Invalid application directory.'))
      }

      const targetUrl = url || app.url
      const imageName = app.image || `odac-app-${app.name}`

      // Step 1: Fetch latest code (app still running → zero-downtime during fetch)
      this.#set(app.id, {status: 'updating'})
      const container = Odac.server('Container')
      const hasGit = await fs.promises
        .access(path.join(appDir, '.git'))
        .then(() => true)
        .catch(() => false)

      await logger.init()

      const buildId = this.#generateRuntimeId('build')
      logCtrl = logger.createBuildStream(buildId, {
        image: imageName,
        strategy: 'git-app'
      })

      const gitPhase = hasGit ? 'git_pull' : 'git_clone'
      if (logCtrl) logCtrl.startPhase(gitPhase)

      if (hasGit) {
        // Fast path: incremental fetch (delta download only)
        await container.fetchRepo(targetUrl, targetBranch, appDir, token, commitSha, logCtrl)
      } else {
        // Fallback: fresh clone for legacy apps where .git was removed by Builder
        log('No .git found in %s, performing fresh clone', app.name)
        await fs.promises.rm(appDir, {recursive: true, force: true})
        await fs.promises.mkdir(appDir, {recursive: true})
        await container.cloneRepo(targetUrl, targetBranch, appDir, token, logCtrl)
      }
      if (logCtrl) logCtrl.endPhase(gitPhase, true)

      // Step 2: Rebuild image (app still running on old image)
      this.#set(app.id, {status: 'building'})
      await Odac.server('Container').build(appDir, imageName, app.name, {
        stream: logCtrl.stream,
        start: logCtrl.startPhase,
        end: logCtrl.endPhase,
        finalize: () => {}, // Delay finalization until deployment is complete
        subscribe: logCtrl.subscribe
      })

      // Check if Zero-Downtime Deployment (ZDD) is applicable.
      // ZDD requires the Proxy to route traffic, meaning the app must have at least one domain.
      const domainsConfig = Odac.core('Config').config.domains || {}
      const hasDomains = Object.values(domainsConfig).some(d => d.appId === app.name || d.appId === app.id)

      if (hasDomains) {
        // --- Zero-Downtime Deployment (Blue-Green) ---
        log('ZDD enabled for %s (Has Domains). Executing Blue-Green switch.', app.name)
        greenContainerName = `${app.name}-green-${this.#generateRuntimeId()}`
        await this.deploy.performBlueGreenDeploy(app, greenContainerName, {
          logCtrl,
          operation: 'Redeploy',
          runGreenContainer: async () => {
            const greenApp = {...app, name: greenContainerName, _appIdentity: app.name}
            await this.#runGitApp(greenApp)
          }
        })
      } else {
        // --- Standard Redeploy (No Domains) ---
        log('Standard redeploy for %s (No Domains). Stopping old container first.', app.name)

        // Step 3: Stop running container (minimal downtime starts here)
        if (logCtrl) logCtrl.startPhase('stop_old_container')
        await this.stop(app.id)
        if (logCtrl) logCtrl.endPhase('stop_old_container', true)

        // Step 4: Restart with new image
        this.#set(app.id, {active: true, status: 'starting'})

        if (logCtrl) logCtrl.startPhase('start_new_container')
        await this.#runGitApp(app)
        if (logCtrl) logCtrl.endPhase('start_new_container', true)

        this.#set(app.id, {status: 'running', started: Date.now()})

        this.#scanAndSaveHttpStatus(app).catch(e => error('HTTP scan failed for %s: %s', app.name, e.message))

        if (logCtrl) logCtrl.startPhase('proxy_propagation')
        Odac.server('Proxy').syncConfig()
        Odac.server('Proxy').purgeCacheForApp(app.id)
        if (logCtrl) logCtrl.endPhase('proxy_propagation', true)
      }

      // Persist updated metadata
      const updates = {}
      if (commitSha) updates.commitSha = commitSha
      if (branch || url) {
        if (branch) updates.branch = targetBranch
        if (url) updates.url = url // Save updated URL if provided

        // Sync with the git object structure
        if (app.git) {
          const gitMetadata = this.#getGitMetadata(targetUrl)
          updates.git = {
            ...app.git,
            repo: gitMetadata.repo,
            branch: targetBranch,
            provider: gitMetadata.provider
          }
        }
      }

      if (Object.keys(updates).length) this.#set(app.id, updates)

      Odac.server('Hub').trigger('app.list')
      if (logCtrl) await logCtrl.finalize(true)
      return Odac.server('Api').result(true, __('App %s redeployed successfully.', app.name))
    } catch (err) {
      error('Redeploy failed for %s: %s', app.name, err.message)
      if (logCtrl) {
        logCtrl.stream.write(`[Error] ${err.message}\n`)
        await logCtrl.finalize(false)
      }

      // Cleanup leaked green container if ZDD failed mid-flight
      if (greenContainerName) {
        try {
          // Verify if it exists before trying to kill it
          const status = await Odac.server('Container').getStatus(greenContainerName)
          if (status && status.running) {
            log('ZDD Cleanup: Removing leaked temporary container %s due to redeploy abort.', greenContainerName)
            await Odac.server('Container').stop(greenContainerName)
            await Odac.server('Container').remove(greenContainerName)
          }
        } catch {
          /* ignore cleanup errors */
        }
        await this.deploy.cleanupGreenArtifacts(greenContainerName)
      }

      this.#set(app.id, {status: 'errored'})
      return Odac.server('Api').result(false, __('Redeploy failed: %s', err.message))
    } finally {
      this.#processing.delete(app.id)
      Odac.server('Container').unregisterBuildLogger(app.name)
    }
  }

  // Status & Listing
  async getBuildStats(id) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    try {
      const logger = new Logger(app.name)
      const stats = await logger.getDailySummary()

      return Odac.server('Api').result(true, stats)
    } catch (e) {
      error('Failed to get build stats for %s: %s', app.name, e.message)
      return Odac.server('Api').result(false, e.message)
    }
  }

  /**
   * Returns the environment variables for a specific app in structured format.
   * Manual envs and linked app envs are returned separately for frontend display.
   * Sensitive values (pass, key, secret, token, cert, salt) are masked.
   * @param {string|number} id - App id, name, or file
   * @returns {object} Api.result with { manual: {}, linked: [{app, env}] }
   */
  getEnv(id) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    const envConfig = app.env || {}
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)

    // Sanitize manual envs
    const manual = this.#sanitizeEnv(this.#getManualEnv(envConfig))

    // Resolve linked apps and sanitize each
    const linked = []
    const linkedNames = isNewStructure ? envConfig.linked || [] : []

    for (const name of linkedNames) {
      const linkedApp = this.#get(name)
      if (!linkedApp) continue

      const linkedEnvConfig = linkedApp.env || {}
      const linkedManual = this.#getManualEnv(linkedEnvConfig)
      linked.push({app: name, env: this.#sanitizeEnv(linkedManual)})
    }

    return Odac.server('Api').result(true, {manual, linked})
  }

  /**
   * Removes specified keys from the app's manual environment variables.
   * Accepts an array of key names for batch deletion.
   * @param {string|number} id - App id, name, or file
   * @param {string[]} keys - Array of env key names to remove
   * @returns {object} Api.result
   */
  deleteEnv(id, keys) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!Array.isArray(keys) || keys.length === 0) {
      return Odac.server('Api').result(false, __('Invalid keys payload. Expected a non-empty array.'))
    }

    const envConfig = app.env || {}
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)
    const manual = this.#getManualEnv(envConfig)

    let removedCount = 0
    for (const key of keys) {
      if (Object.prototype.hasOwnProperty.call(manual, key)) {
        delete manual[key]
        removedCount++
      }
    }

    if (isNewStructure) {
      app.env.manual = manual
    } else {
      app.env = {manual, linked: []}
    }

    this.#saveApps()
    return Odac.server('Api').result(true, __('Removed %d key(s) from %s. Restart required to apply.', removedCount, app.name))
  }

  /**
   * Links another app's manual env vars to this app.
   * Linked envs are resolved at runtime via #resolveEnv.
   * @param {string|number} id - App id, name, or file
   * @param {string} target - Name of the app to link
   * @returns {object} Api.result
   */
  linkEnv(id, target) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!target || typeof target !== 'string') {
      return Odac.server('Api').result(false, __('Invalid target. Expected an app name.'))
    }

    if (app.name === target) {
      return Odac.server('Api').result(false, __('Cannot link an app to itself.'))
    }

    const targetApp = this.#get(target)
    if (!targetApp) {
      return Odac.server('Api').result(false, __('Target app %s not found.', target))
    }

    const envConfig = app.env || {}
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)

    if (isNewStructure) {
      const linked = new Set(envConfig.linked || [])
      linked.add(target)
      app.env.linked = [...linked]
    } else {
      app.env = {
        manual: envConfig,
        linked: [target]
      }
    }

    this.#saveApps()
    return Odac.server('Api').result(true, __('Linked %s to %s. Restart required to apply.', target, app.name))
  }

  /**
   * Merges provided key-value pairs into the app's manual environment variables.
   * Does not restart the container — caller must trigger restart separately.
   * @param {string|number} id - App id, name, or file
   * @param {object} env - Key-value pairs to merge into manual envs
   * @returns {object} Api.result
   */
  setEnv(id, env) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!env || typeof env !== 'object' || Array.isArray(env)) {
      return Odac.server('Api').result(false, __('Invalid env payload. Expected an object.'))
    }

    const envConfig = app.env || {}
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)

    if (isNewStructure) {
      app.env.manual = {...(envConfig.manual || {}), ...env}
    } else {
      // Migrate legacy flat env to structured format
      app.env = {
        manual: {...envConfig, ...env},
        linked: []
      }
    }

    this.#saveApps()
    return Odac.server('Api').result(true, __('Environment updated for %s. Restart required to apply.', app.name))
  }

  /**
   * Removes an app link from this app's linked env list.
   * @param {string|number} id - App id, name, or file
   * @param {string} target - Name of the app to unlink
   * @returns {object} Api.result
   */
  unlinkEnv(id, target) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!target || typeof target !== 'string') {
      return Odac.server('Api').result(false, __('Invalid target. Expected an app name.'))
    }

    const envConfig = app.env || {}
    const linked = envConfig.linked || []

    if (!linked.includes(target)) {
      return Odac.server('Api').result(false, __('App %s is not linked to %s.', target, app.name))
    }

    app.env.linked = linked.filter(name => name !== target)
    this.#saveApps()
    return Odac.server('Api').result(true, __('Unlinked %s from %s. Restart required to apply.', target, app.name))
  }

  // Port & Volume Management

  /**
   * Replaces the port mappings for an app with a new set.
   * Validates each entry for correct structure and port range (1-65535).
   * Auto-assigns host ports when 'auto' is specified.
   * @param {string|number} id - App id, name, or file
   * @param {Array<{host: number|string, container: number}>} ports - New port mappings
   * @returns {object} Api.result
   */
  async setPorts(id, ports) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!Array.isArray(ports)) {
      return Odac.server('Api').result(false, __('Invalid ports payload. Expected an array.'))
    }

    // Validate each port mapping
    for (const entry of ports) {
      if (!entry || typeof entry !== 'object') {
        return Odac.server('Api').result(false, __('Invalid port entry. Expected {host, container}.'))
      }
      if (entry.container === undefined || entry.container === null) {
        return Odac.server('Api').result(false, __('Each port entry must have a container port.'))
      }
      const containerPort = Number(entry.container)
      if (!Number.isInteger(containerPort) || containerPort < 1 || containerPort > 65535) {
        return Odac.server('Api').result(false, __('Invalid container port: %s. Must be 1-65535.', entry.container))
      }
      if (entry.host !== undefined && entry.host !== 'auto') {
        const hostPort = Number(entry.host)
        if (!Number.isInteger(hostPort) || hostPort < 1 || hostPort > 65535) {
          return Odac.server('Api').result(false, __('Invalid host port: %s. Must be 1-65535 or "auto".', entry.host))
        }
      }
    }

    // Resolve 'auto' host ports
    const resolved = await this.#preparePorts(ports)
    app.ports = resolved
    this.#saveApps()

    return Odac.server('Api').result(true, __('Ports updated for %s. Restart required to apply.', app.name))
  }

  /**
   * Replaces the volume mappings for an app with a new set.
   * Validates each entry for correct structure and ensures container paths are absolute.
   * Named (relative) host paths are resolved under the app's directory for isolation.
   * @param {string|number} id - App id, name, or file
   * @param {Array<{host: string, container: string}>} volumes - New volume mappings
   * @returns {object} Api.result
   */
  setVolumes(id, volumes) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!Array.isArray(volumes)) {
      return Odac.server('Api').result(false, __('Invalid volumes payload. Expected an array.'))
    }

    // Validate each volume mapping
    for (const entry of volumes) {
      if (!entry || typeof entry !== 'object') {
        return Odac.server('Api').result(false, __('Invalid volume entry. Expected {host, container}.'))
      }
      if (!entry.container || typeof entry.container !== 'string') {
        return Odac.server('Api').result(false, __('Each volume entry must have a container path string.'))
      }
      if (!entry.host || typeof entry.host !== 'string') {
        return Odac.server('Api').result(false, __('Each volume entry must have a host path string.'))
      }
    }

    // Resolve named volumes relative to app directory
    const appDir = path.join(Odac.core('Config').config.app.path, app.name)
    app.volumes = this.#prepareVolumes(volumes, appDir)
    this.#saveApps()

    return Odac.server('Api').result(true, __('Volumes updated for %s. Restart required to apply.', app.name))
  }

  /**
   * Connects a hardware device to an application container.
   * @param {string|number} id - App id, name, or file
   * @param {string} hostPath - Path to the device on the host (e.g. /dev/ttyACM0)
   * @param {string} [containerPath] - Path to map inside the container (defaults to hostPath)
   * @returns {object} Api.result
   */
  deviceAdd(id, hostPath, containerPath = null) {
    const app = this.#get(id)
    if (!app) return Odac.server('Api').result(false, __('App %s not found.', id))

    if (!hostPath) return Odac.server('Api').result(false, __('Missing host device path.'))

    if (!app.devices) app.devices = []

    // Prevent duplicates
    const existing = app.devices.find(d => d.host === hostPath)
    if (existing) {
      existing.container = containerPath || hostPath
    } else {
      app.devices.push({host: hostPath, container: containerPath || hostPath})
    }

    this.#saveApps()
    return Odac.server('Api').result(true, __('Device %s added to %s. Restart required.', hostPath, app.name))
  }

  /**
   * Removes a device mapping from an application.
   * @param {string|number} id - App id, name, or file
   * @param {string} hostPath - Host path of the device to remove
   * @returns {object} Api.result
   */
  deviceDelete(id, hostPath) {
    const app = this.#get(id)
    if (!app) return Odac.server('Api').result(false, __('App %s not found.', id))

    if (!app.devices) return Odac.server('Api').result(true, __('No devices connected to %s.', app.name))

    app.devices = app.devices.filter(d => d.host !== hostPath)
    this.#saveApps()

    return Odac.server('Api').result(true, __('Device %s removed from %s. Restart required.', hostPath, app.name))
  }

  /**
   * Updates the Docker networks for a running app container.
   * Validates network names and delegates to Container.setNetworks.
   * @param {string|number} id - App id, name, or file
   * @param {string[]} networks - Desired network names
   * @returns {object} Api.result
   */
  async setNetworks(id, networks) {
    const app = this.#get(id)
    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    if (!Array.isArray(networks)) {
      return Odac.server('Api').result(false, __('Invalid networks payload. Expected an array of network names.'))
    }

    // Validate each network name (alphanumeric, hyphens, underscores)
    const validName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/
    for (const net of networks) {
      if (typeof net !== 'string' || !validName.test(net)) {
        return Odac.server('Api').result(false, __('Invalid network name: %s', net))
      }
    }

    const container = Odac.server('Container')
    const status = await container.getStatus(app.name)
    if (!status.running) {
      return Odac.server('Api').result(false, __('App %s is not running. Start the app first.', app.name))
    }

    const result = await container.setNetworks(app.name, networks)
    if (!result.success) {
      return Odac.server('Api').result(false, result.message || __('Failed to update networks for %s.', app.name))
    }

    return Odac.server('Api').result(true, __('Networks updated for %s: %s', app.name, result.networks.join(', ')))
  }

  async list(detailed = false) {
    if (this.#apps.length === 0) {
      this.#apps = this.#loadAppsFromConfig()
    }
    const container = Odac.server('Container')
    const cleanApps = []

    for (const app of this.#apps) {
      const copy = {...app}
      const statusInfo = await container.getStatus(app.name)
      const isRunning = statusInfo.running

      try {
        const logger = new Logger(app.name)
        const lastBuild = await logger.getLastBuild()
        if (lastBuild) {
          copy.build = {
            id: lastBuild.id,
            status: lastBuild.status,
            duration: lastBuild.duration,
            errors: lastBuild.errors,
            warnings: lastBuild.warnings,
            phases: (lastBuild.phases || []).map(p => {
              if (p.status === 'failed' || p.errors > 0) return 2
              if (p.warnings > 0) return 1
              return 0
            })
          }
        }

        const healthStats = await logger.getHealth()
        copy.health = healthStats.logs || []
      } catch {
        /* ignore logger errors in list */
      }

      copy.status = isRunning ? 'running' : 'stopped'
      if (statusInfo.networks) {
        copy.networks = statusInfo.networks
      }
      if (isRunning && statusInfo.startTime) {
        copy.started = new Date(statusInfo.startTime).getTime()
      }

      if (detailed === true) {
        delete copy.pid
        delete copy.ip
        delete copy.uptime

        const appIdentity = copy._appIdentity || copy.name
        const internalAppDir = copy.file ? path.dirname(copy.file) : path.join(Odac.core('Config').config.app.path, appIdentity)
        copy.path = container.resolveHostPath ? container.resolveHostPath(internalAppDir) : internalAppDir

        // Security: Expose only env keys, not values
        // Structure: { manual: [KEY1, KEY2], linked: [APP1, APP2] }
        const rawEnv = copy.env || {}
        if (rawEnv.manual || Array.isArray(rawEnv.linked)) {
          copy.env = {
            manual: Object.keys(rawEnv.manual || {}),
            linked: rawEnv.linked || []
          }
        } else {
          // Legacy support
          copy.env = {
            manual: Object.keys(rawEnv),
            linked: []
          }
        }

        // Container path to Host path resolution for volumes
        if (Array.isArray(copy.volumes)) {
          copy.volumes = copy.volumes.map(vol => ({
            host: container.resolveHostPath ? container.resolveHostPath(vol.host) : vol.host,
            container: vol.container
          }))
        }

        cleanApps.push(copy)
      } else {
        cleanApps.push({
          name: copy.name,
          image: copy.image,
          status: copy.status
        })
      }
    }

    return Odac.server('Api').result(true, cleanApps)
  }

  // Private: App Data Management
  #loadAppsFromConfig() {
    const apps = Odac.core('Config').config.apps
    return Array.isArray(apps) ? apps : []
  }

  #get(id) {
    if (!this.#loaded) {
      this.#apps = this.#loadAppsFromConfig()
      this.#loaded = true
    }

    return this.#apps.find(app => app.id === id || app.name === id || app.file === id) || false
  }

  #add(file, type = 'script') {
    let name = path.basename(file)

    if (type === 'script' && SCRIPT_EXTENSIONS.some(ext => name.endsWith(ext))) {
      name = name.split('.').slice(0, -1).join('.')
    }

    name = this.#generateUniqueName(name)

    const app = {
      id: this.#getNextId(),
      name,
      file,
      type,
      active: true,
      created: Date.now()
    }

    this.#apps.push(app)
    this.#saveApps()

    return app
  }

  #set(id, updates) {
    const app = this.#get(id)
    if (!app) return false

    Object.assign(app, updates)
    this.#saveApps()

    return true
  }

  #saveApps() {
    const cleanApps = this.#apps.map(app => {
      const copy = {...app}
      // Remove runtime/ephemeral properties
      delete copy._appIdentity
      delete copy.status
      delete copy.pid
      delete copy.uptime
      delete copy.build
      delete copy.health
      delete copy.ip
      delete copy.started // Maybe keep started? No, restart resets it.
      return copy
    })
    Odac.core('Config').config.apps = cleanApps
  }

  // Private: App Execution
  async #run(id, logCtrl = null) {
    const app = this.#get(id)
    if (!app) return false

    // Prevent concurrent runs for the same app
    if (this.#processing.has(id)) {
      log('App %s is already being processed. Skipping duplicate run.', app.name)
      return true
    }

    this.#processing.add(id)

    log('Starting app %s (Type: %s)...', app.name, app.type)
    this.#set(id, {status: 'starting', updated: Date.now()})

    try {
      if (app.type === 'script') {
        await this.#runScript(app)
      } else if (app.type === 'container') {
        await this.#runContainer(app, logCtrl)
      } else if (app.type === 'git') {
        await this.#runGitApp(app)
      }

      this.#set(id, {status: 'running', started: Date.now()})

      this.#scanAndSaveHttpStatus(app).catch(e => error('HTTP scan failed for %s: %s', app.name, e.message))

      // Trigger Proxy Sync after every successful start/restart.
      // Container IP changes on restart; without this, Proxy routes to the old (dead) IP -> 502
      Odac.server('Proxy').syncConfig()

      return true
    } catch (err) {
      error('Failed to start app %s: %s', app.name, err.message)
      this.#set(id, {status: 'errored', updated: Date.now()})
      return false
    } finally {
      this.#processing.delete(id)
    }
  }

  async #runScript(app) {
    if (!Odac.server('Container').available) {
      return this.#runScriptLocal(app)
    }
    return this.#runScriptContainer(app)
  }

  async #runScriptLocal(app) {
    const dir = path.dirname(app.file)
    const filename = path.basename(app.file)
    const ext = path.extname(filename)
    const runner = SCRIPT_RUNNERS[ext] || SCRIPT_RUNNERS['.js']

    const cmd = runner.local
    const args = [...(runner.args || []), filename]

    const logger = await this.#getLogger(app.name)

    // Create daily rotating log stream
    const logCtrl = logger.createRuntimeStream()
    this.#logStreams.set(app.name, logCtrl)

    log(`Spawning local process for ${app.name}: ${cmd} ${args.join(' ')}`)

    const child = childProcess.spawn(cmd, args, {
      cwd: dir,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: {...process.env, ODAC_APP: 'true'}
    })

    this.#set(app.id, {pid: child.pid})

    child.stdout.on('data', data => {
      logCtrl.write(`[LOG] [${Date.now()}] ${data}`)
    })

    child.stderr.on('data', data => {
      logCtrl.error(`[ERR] [${Date.now()}] ${data}`)
    })

    child.on('exit', (code, signal) => {
      log(`App ${app.name} exited with code ${code} signal ${signal}`)
      logCtrl.write(`[LOG] [${Date.now()}] App exited with code ${code} signal ${signal}\n`)
      logCtrl.end()
      this.#set(app.id, {status: 'stopped', pid: null, active: false})
    })

    child.on('error', err => {
      error(`Failed to start local app ${app.name}: ${err.message}`)
      logCtrl.error(`[ERR] [${Date.now()}] Failed to start app: ${err.message}\n`)
      logCtrl.end()
      this.#set(app.id, {status: 'errored', pid: null})
    })
  }

  async #runScriptContainer(app) {
    const filename = path.basename(app.file)
    const dir = path.dirname(app.file)
    const ext = path.extname(filename)
    const runner = SCRIPT_RUNNERS[ext] || SCRIPT_RUNNERS['.js']

    // Canonical identity for API tokens (survives Blue-Green rename)
    const appIdentity = app._appIdentity || app.name

    const env = {ODAC_APP: 'true'}
    const volumes = [{host: dir, container: '/app'}]

    // API Permission Injection
    if (app.api) {
      const api = Odac.server('Api')
      env.ODAC_API_KEY = api.generateAppToken(appIdentity, app.api)

      if (api.hostSocketDir) {
        volumes.push({host: api.hostSocketDir, container: '/odac:ro'})
        env.ODAC_API_SOCKET = '/odac/api.sock'
      }
    }

    await Odac.server('Container').runApp(app.name, {
      image: runner.image,
      cmd: [runner.cmd, ...(runner.args || []), filename],
      volumes,
      devices: app.devices || [],
      env
    })
  }

  async #runContainer(app, logCtrl = null) {
    const container = Odac.server('Container')

    if (!container.available) {
      throw new Error('Docker is not available via Container service.')
    }

    // Canonical identity for API tokens (survives Blue-Green rename)
    const appIdentity = app._appIdentity || app.name

    // Pull image FIRST so all subsequent inspections (port, user) have metadata available.
    // Without this, getImageExposedPorts and getImageUser fail on un-pulled images → null results.
    if (typeof container.ensureImage === 'function') {
      await container.ensureImage(app.image, logCtrl)
    }

    // Port Resolution: Detect from image EXPOSE or assign default
    const port = await this.#resolveContainerPort(app)

    const env = this.#resolveEnv(app)
    const volumes = [...(app.volumes || [])]

    // Inject PORT env so well-behaved apps can discover their port
    if (port) env.PORT = port.toString()

    // API Permission Injection
    if (app.api) {
      const api = Odac.server('Api')
      env.ODAC_API_KEY = api.generateAppToken(appIdentity, app.api)

      if (api.hostSocketDir) {
        volumes.push({host: api.hostSocketDir, container: '/odac:ro'})
        env.ODAC_API_SOCKET = '/odac/api.sock'
      }
    }

    // Fix volume permissions before starting the app container.
    // Host-created directories default to root:root 0755, but many images run as
    // non-root (e.g., node:1000). chmod 0777 ensures any container user can write.
    await this.#fixVolumePermissions(volumes)

    const runOptions = {
      image: app.image,
      // Only pass ports with host binding to Docker. Internal-only ports (container-only)
      // are metadata for Proxy routing and must NOT be sent as Docker PortBindings.
      ports: (app.ports || []).filter(p => p.host),
      volumes,
      devices: app.devices || [],
      env
    }

    if (app.cmd) runOptions.cmd = app.cmd

    await container.runApp(app.name, runOptions, logCtrl)

    await this.#attachLogger(app)

    // Runtime Port Discovery:
    // Verify the container is actually listening on the expected port.
    // Handles images that ignore PORT env var or have no EXPOSE metadata.
    this.#pollForPort(app, port || 3000)
  }

  /**
   * Ensures volume host directories are writable by any container user.
   * Many Docker images run as non-root (e.g., node:1000) but host-created
   * directories default to root:root 0755, causing EACCES on write.
   *
   * Normalizes host-native paths (e.g. /var/odac/...) to container-internal
   * paths (/app/...) so mkdir/chmod operates through the existing bind mount.
   * This is the most reliable approach — no init containers, no user detection,
   * works regardless of image USER directive or /etc/passwd contents.
   *
   * @param {Array<{host: string, container: string}>} volumes - Volume mappings
   */
  async #fixVolumePermissions(volumes) {
    if (!volumes || volumes.length === 0) return

    const appsPath = path.resolve(Odac.core('Config').config.app.path)
    const hostRoot = process.env.ODAC_HOST_ROOT

    for (const vol of volumes) {
      // Skip read-only mounts
      if (vol.container.endsWith(':ro')) continue
      // Skip unresolved or non-absolute host paths
      if (!vol.host || !path.isAbsolute(vol.host)) continue

      // Normalize host-native paths to container-internal paths for FS operations.
      // Volume configs may store host paths (e.g. /var/odac/.odac/apps/...) which
      // don't exist inside the container. Convert to /app/... so mkdir/chmod
      // operates through the existing /app bind mount → reflected on host too.
      let fsPath = vol.host
      if (hostRoot && fsPath.startsWith(hostRoot)) {
        fsPath = path.join('/app', fsPath.substring(hostRoot.length))
      }

      const resolvedFsPath = path.resolve(fsPath)
      if (!resolvedFsPath.startsWith(appsPath)) {
        error('FixVolPerms: Skipping chmod on path outside app directory for security: %s', vol.host)
        continue
      }

      try {
        await fs.promises.mkdir(fsPath, {recursive: true})
        await fs.promises.chmod(fsPath, 0o777)
        log('FixVolPerms: Set 0777 on %s (from %s)', fsPath, vol.host)
      } catch (e) {
        error('FixVolPerms: chmod failed for %s: %s', fsPath, e.message)
      }
    }
  }

  /**
   * Resolves the internal port for a container app.
   * Priority: existing config > image EXPOSE metadata > default (3000).
   * Persists the result so Proxy can route immediately.
   *
   * @param {object} app - App record
   * @returns {Promise<number>} Resolved container port
   */
  async #resolveContainerPort(app) {
    // Priority 1: Already configured
    if (app.ports && app.ports.length > 0 && app.ports[0].container) {
      return app.ports[0].container
    }

    // Priority 2: Auto-detect from Docker image EXPOSE
    if (app.image) {
      try {
        const exposed = await Odac.server('Container').getImageExposedPorts(app.image)
        if (exposed && exposed.length > 0) {
          const detected = exposed[0]
          log('Port Auto-Detect: Discovered port %d from image EXPOSE for app %s', detected, app.name)
          app.ports = [{container: detected}]
          this.#saveApps()
          return detected
        }
      } catch (e) {
        error('Port Auto-Detect: Failed to inspect image for app %s: %s', app.name, e.message)
      }
    }

    // Priority 3: Fallback to default
    log('Port Auto-Detect: No port info available for app %s. Assigning default 3000.', app.name)
    app.ports = [{container: 3000}]
    this.#saveApps()
    return 3000
  }

  async #attachLogger(app) {
    if (this.#logStreams.has(app.name)) return

    try {
      const logger = await this.#getLogger(app.name)
      const logCtrl = logger.createRuntimeStream()
      this.#logStreams.set(app.name, logCtrl)

      const stream = await Odac.server('Container').logs(app.name)
      if (stream) {
        const container = Odac.server('Container').docker.getContainer(app.name)
        container.modem.demuxStream(stream, {write: chunk => logCtrl.write(chunk)}, {write: chunk => logCtrl.error(chunk)})
      }

      log('Attached log stream to active app: %s', app.name)
    } catch (e) {
      error('Failed to attach logger to app %s: %s', app.name, e.message)
    }
  }

  /**
   * Scans container ports to detect if an HTTP server is running.
   * Saves the detected port or 'false' to the app config for newly added apps.
   * @param {object} app - The application record
   */
  async #scanAndSaveHttpStatus(app) {
    if (app.http !== undefined && app.http !== false) return

    let newHttp = false

    try {
      const container = Odac.server('Container')
      const targetContainer = app.activeContainerId || app.name

      // If docker is unavailable or app runs locally as script, skip container scanning
      if (container && container.available && !app.pid) {
        let attempts = 0
        let listeningPorts = []

        while (attempts < 120) {
          try {
            listeningPorts = await container.getListeningPorts(targetContainer)
            if (listeningPorts.length > 0) break
          } catch {
            // Background scan polling silently ignores intermittent errors
          }
          await new Promise(resolve => setTimeout(resolve, 500))
          attempts++
        }

        if (listeningPorts.length > 0) {
          const containerIP = await container.getIP(targetContainer)
          if (containerIP) {
            let httpPort = null
            let probeAttempts = 0

            // Retry the HTTP probe up to 10 times because some apps open their
            // TCP ports early but take time to actually serve HTTP traffic.
            // Re-fetch listening ports on each attempt so newly opened ports
            // (e.g. the actual HTTP port appearing after an ephemeral/internal one)
            // are included in the probe set.
            while (probeAttempts < 30) {
              const currentPorts = await container.getListeningPorts(targetContainer).catch(() => listeningPorts)
              httpPort = await this.#detectHttpPort(containerIP, currentPorts, 2500)
              if (httpPort !== null) break
              await new Promise(resolve => setTimeout(resolve, 1000))
              probeAttempts++
            }

            if (httpPort !== null) {
              newHttp = httpPort
            }
          }
        }
      }
    } catch (e) {
      log('HTTP scan failed for %s: %s', app.name, e.message)
    }

    if (app.http !== newHttp) {
      this.#set(app.id, {http: newHttp})
      Odac.server('Hub').trigger('app.list')
    }
  }

  // Private: Helpers
  async #isAppRunning(app) {
    if (app.type === 'container' || Odac.server('Container').available) {
      return Odac.server('Container').isRunning(app.name)
    }

    if (app.pid) {
      try {
        process.kill(app.pid, 0)
        return true
      } catch {
        return false
      }
    }

    return false
  }

  #generateRuntimeId(prefix = '') {
    const suffix = `${Date.now()}_${nodeCrypto.randomBytes(4).toString('hex')}`
    return prefix ? `${prefix}_${suffix}` : suffix
  }

  #generateUniqueName(baseName) {
    let name = baseName
    let counter = 1

    while (this.#get(name)) {
      name = `${baseName}-${counter}`
      counter++
    }

    return name
  }

  /**
   * Parses git URL to extract provider and repo in user/repo format.
   * Securely identifies the provider by strictly verifying the hostname
   * to prevent spoofing via subdomains or deceptive paths.
   *
   * @param {string} url - Git source URL (HTTPS or SSH format)
   * @returns {{repo: string, provider: 'github'|'gitlab'|'bitbucket'|'git'}}
   */
  #getGitMetadata(url) {
    if (!url) {
      return {repo: '', provider: 'git'}
    }

    let hostname = ''
    let repo = ''

    try {
      if (url.includes('://')) {
        // HTTPS patterns: https://github.com/user/repo.git
        const urlObj = new URL(url)
        hostname = urlObj.hostname.toLowerCase()
        const pathname = urlObj.pathname.replace(/\/$/, '')
        const parts = pathname.split('/')
        if (parts.length >= 2) {
          const repoPart = parts.pop()
          const userPart = parts.pop()
          repo = `${userPart}/${repoPart}`
        }
      } else if (url.includes('@')) {
        // SSH patterns: git@github.com:user/repo.git
        const parts = url.split('@')
        if (parts.length > 1) {
          const hostPart = parts[1].split(':')
          hostname = hostPart[0].toLowerCase()
          if (hostPart.length > 1) {
            repo = hostPart[1]
          }
        }
      }

      // Cleanup: Remove .git suffix and handle fallback
      if (repo.endsWith('.git')) {
        repo = repo.slice(0, -4)
      } else if (!repo && !hostname) {
        repo = url // Fallback for local paths or malformed input
      }
    } catch {
      repo = url
    }

    // Strict Provider Detection
    let provider = 'git'
    const providers = {
      'bitbucket.org': 'bitbucket',
      'github.com': 'github',
      'gitlab.com': 'gitlab'
    }

    for (const [domain, name] of Object.entries(providers)) {
      // Matches 'example.com' exactly or sub-domains like 'git.example.com'
      if (hostname === domain || hostname.endsWith(`.${domain}`)) {
        provider = name
        break
      }
    }

    return {repo, provider}
  }

  #getNextId() {
    return this.#apps.reduce((maxId, app) => Math.max(app.id, maxId), -1) + 1
  }

  // Private: Recipe Preparation
  #prepareVolumes(recipeVolumes, appDir) {
    if (!recipeVolumes) return []

    return recipeVolumes.map(vol => {
      let host = vol.host

      // Host-to-container path normalization (DooD support):
      // When users add volumes via UI, they may provide host-native paths
      // (e.g. /var/odac/.odac/apps/...). These must be converted back to
      // container-internal /app/... paths so that:
      //   1) resolveHostPath correctly transforms them for Docker daemon
      //   2) #fixVolumePermissions can create/chmod dirs inside the container
      const hostRoot = process.env.ODAC_HOST_ROOT
      if (host && hostRoot && path.isAbsolute(host) && host.startsWith(hostRoot)) {
        host = path.join('/app', host.substring(hostRoot.length))
      }

      // Named volumes (non-absolute paths like 'data', 'config', 'workspace')
      // are resolved under the app's dedicated directory for isolation.
      if (host && !path.isAbsolute(host)) {
        host = path.join(appDir, host)
        if (!fs.existsSync(host)) fs.mkdirSync(host, {recursive: true})
      }

      return {host, container: vol.container}
    })
  }

  async #preparePorts(recipePorts) {
    if (!recipePorts) return []

    const ports = []
    for (const port of recipePorts) {
      const hostPort = port.host === 'auto' ? await this.#findAvailablePort(30000) : port.host
      ports.push({host: hostPort, container: port.container})
    }

    return ports
  }

  // Private: Port Management
  async #findAvailablePort(start) {
    let port = start
    while (await this.#isPortInUse(port)) port++
    return port
  }

  #isPortInUse(port) {
    return new Promise(resolve => {
      const server = net.createServer()

      server.on('connection', socket => {
        socket.on('error', () => {})
      })

      server.once('error', err => {
        resolve(err.code === 'EADDRINUSE')
      })

      server.once('listening', () => {
        server.close()
        resolve(false)
      })

      server.listen(port, '127.0.0.1')
    })
  }

  /**
   * Masks sensitive values in an env object based on SENSITIVE_KEY_PATTERN.
   * @param {object} env - Raw key-value env pairs
   * @returns {object} Sanitized env with sensitive values replaced by '***'
   */
  #sanitizeEnv(env) {
    const sanitized = {}
    for (const [key, value] of Object.entries(env)) {
      sanitized[key] = SENSITIVE_KEY_PATTERN.test(key) ? '***' : value
    }
    return sanitized
  }

  /**
   * Helper to extract manual envs handling both legacy and new structures.
   * @param {object} envConfig - The app.env object
   * @returns {object} The manual env key-value pairs
   */
  #getManualEnv(envConfig) {
    if (!envConfig) return {}
    // If it has .manual or .linked, it's the new structure.
    // Otherwise it's legacy flat structure.
    // Note: checking .linked presence is important because an app might have linked apps but empty manual envs.
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)
    return isNewStructure ? envConfig.manual || {} : envConfig
  }

  #resolveEnv(app, includeSystem = true) {
    const finalEnv = includeSystem ? {HOST: '0.0.0.0', ODAC_APP: 'true'} : {}
    const envConfig = app.env || {}

    // Check if new structure (has manual or linked prop)
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)

    // 1. Resolve Linked Apps (New Structure only)
    if (isNewStructure && Array.isArray(envConfig.linked)) {
      for (const linkName of envConfig.linked) {
        const linkedApp = this.#get(linkName)
        if (linkedApp) {
          // Pull manual envs from linked app (recursive linking not supported yet to avoid loops)
          const linkedEnvConfig = linkedApp.env || {}
          Object.assign(finalEnv, this.#getManualEnv(linkedEnvConfig))
        }
      }
    }

    // 2. Apply Manual Envs (Overrides linked)
    Object.assign(finalEnv, this.#getManualEnv(envConfig))

    return finalEnv
  }
}

module.exports = new App()
