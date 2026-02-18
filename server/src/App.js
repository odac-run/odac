const {log, error} = Odac.core('Log', false).init('App')
const fs = require('fs')
const path = require('path')
const net = require('net')
const nodeCrypto = require('crypto')
const childProcess = require('child_process')
const os = require('os')
const Logger = require('./Container/Logger')

const SCRIPT_EXTENSIONS = ['.js', '.py', '.php', '.sh', '.rb']
const SCRIPT_RUNNERS = {
  '.js': {image: 'node:lts-alpine', cmd: 'node', local: 'node'},
  '.py': {image: 'python:alpine', cmd: 'python3', local: 'python3', args: ['-u']},
  '.php': {image: 'php:cli-alpine', cmd: 'php', local: 'php'},
  '.rb': {image: 'ruby:alpine', cmd: 'ruby', local: 'ruby'},
  '.sh': {image: 'alpine:latest', cmd: 'sh', local: 'sh'}
}

class App {
  #apps = []
  #loaded = false
  #processing = new Set()
  #creating = new Set()

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
  }

  async check() {
    this.#apps = this.#loadAppsFromConfig()

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
      } else if (!isRunning && !['stopped', 'errored', 'starting', 'installing'].includes(app.status)) {
        this.#run(app.id)
      }
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
    // Support both string (legacy) and object config
    // String: create("mysql")
    // Object: create({type: "app", app: "postgres", name: "postgres-2--xyz"})
    // Object: create({type: "github", repo: "...", token: "...", name: "myapp"})

    if (typeof config === 'string') {
      if (/^(https?|git|ssh):\/\//.test(config) || /^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/.test(config)) {
        const name = this.#generateUniqueName(path.basename(config, '.git').replace(/[^a-zA-Z0-9-]/g, '-'))
        config = {type: 'git', url: config, name}
      } else {
        config = {type: 'app', app: config}
      }
    }

    log('Creating app: %j', config)

    // Validate config
    if (!config.type) {
      return Odac.server('Api').result(false, __('Missing config type'))
    }

    switch (config.type) {
      case 'app':
        return this.#createFromRecipe(config)
      case 'git':
        return this.#createFromGit(config)
      case 'github':
        return this.#createFromGithub(config)
      default:
        return Odac.server('Api').result(false, __('Unknown config type: %s', config.type))
    }
  }

  async #createFromRecipe(config) {
    const {app: appType, name: customName} = config

    if (!appType) {
      log('createFromRecipe: Missing app type')
      return Odac.server('Api').result(false, __('Missing app type'))
    }

    log('createFromRecipe: Fetching recipe for %s', appType)

    let recipe
    try {
      recipe = await Odac.server('Hub').getApp(appType)
      log('createFromRecipe: Recipe received: %j', recipe)
    } catch (e) {
      error('createFromRecipe: Failed to fetch recipe: %s', e)
      return Odac.server('Api').result(false, __('Could not find recipe for %s: %s', appType, e))
    }

    const name = customName || this.#generateUniqueName(recipe.name)
    log('createFromRecipe: Using name: %s', name)

    if (this.#get(name)) {
      log('createFromRecipe: App %s already exists', name)
      return Odac.server('Api').result(false, __('App %s already exists', name))
    }

    if (this.#creating.has(name)) {
      log('createFromRecipe: App %s is already being created', name)
      return Odac.server('Api').result(false, __('App %s is already being created', name))
    }
    this.#creating.add(name)

    try {
      const appDir = path.join(Odac.core('Config').config.app.path, name)
      if (!fs.existsSync(appDir)) fs.mkdirSync(appDir, {recursive: true})

      const app = {
        id: this.#getNextId(),
        name,
        type: 'container',
        image: recipe.image,
        ports: await this.#preparePorts(recipe.ports),
        volumes: this.#prepareVolumes(recipe.volumes, appDir),
        env: this.#mergeRecipeEnv(recipe, config.env),
        active: true,
        created: Date.now(),
        status: 'installing'
      }

      log('createFromRecipe: App config: %j', app)

      this.#apps.push(app)
      this.#saveApps()

      try {
        log('createFromRecipe: Starting app...')
        if (await this.#run(app.id)) {
          log('createFromRecipe: App started successfully')
          // Notify Hub
          Odac.server('Hub').trigger('app.list')
          return Odac.server('Api').result(true, __('App %s created successfully.', name))
        }
        throw new Error('Failed to start app container. Check logs for details.')
      } catch (e) {
        error('createFromRecipe: Failed to start app: %s', e.message)
        this.#apps = this.#apps.filter(s => s.id !== app.id)
        this.#saveApps()
        return Odac.server('Api').result(false, e.message)
      }
    } finally {
      this.#creating.delete(name)
    }
  }

  async #createFromGithub(config) {
    // Legacy - redirect to git
    return this.#createFromGit(config)
  }

  async #createFromGit(config) {
    const {url, token, branch, name, dev = false, env = {}, port = 3000} = config

    log('createFromGit: Starting git deployment')
    log('createFromGit: URL: %s, Branch: %s, Name: %s', url, branch, name)

    // Validate required fields
    if (!url) {
      return Odac.server('Api').result(false, __('Missing git URL'))
    }

    // Security: Validate Git URL to prevent Command Injection
    // Block dangerous shell characters that could be used to chain commands
    if (/[;&|`$(){}<>]/.test(url)) {
      return Odac.server('Api').result(false, __('Invalid Git URL: Contains illegal characters.'))
    }

    // Validate protocol (optional but recommended)
    if (!url.match(/^(https?|git|ssh|ftps?|rsync):\/\//) && !url.match(/^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/)) {
      return Odac.server('Api').result(false, __('Invalid Git URL: Unsupported protocol.'))
    }
    if (!name) {
      return Odac.server('Api').result(false, __('Missing app name'))
    }

    // Check if name already exists
    if (this.#get(name)) {
      return Odac.server('Api').result(false, __('App %s already exists', name))
    }

    if (this.#creating.has(name)) {
      return Odac.server('Api').result(false, __('App %s is already being created', name))
    }
    this.#creating.add(name)

    try {
      // Validate the app name to prevent path traversal.
      if (path.basename(name) !== name) {
        return Odac.server('Api').result(false, __('Invalid app name.'))
      }

      const appDir = path.join(Odac.core('Config').config.app.path, name)
      log('createFromGit: App directory: %s', appDir)

      if (fs.existsSync(appDir)) {
        log('createFromGit: Removing existing directory')
        fs.rmSync(appDir, {recursive: true, force: true})
      }
      fs.mkdirSync(appDir, {recursive: true})

      // Build git URL with token if provided
      // Security update: We now pass the token separately via env var to prevent exposure in process/docker history

      const imageName = `odac-app-${name}`
      let logCtrl = null

      try {
        const logger = await this.#getLogger(name)
        const buildId = `build_${Date.now()}`
        logCtrl = logger.createBuildStream(buildId, {
          image: imageName,
          strategy: 'git-app'
        })

        // Step 1: Clone repository
        log('createFromGit: Cloning repository...')
        if (logCtrl) logCtrl.startPhase('git_clone')
        await Odac.server('Container').cloneRepo(url, branch, appDir, token, logCtrl)
        if (logCtrl) logCtrl.endPhase('git_clone', true)
        log('createFromGit: Clone successful')

        // Step 2: Build with Native Builder
        log('createFromGit: Building image...')
        await Odac.server('Container').build(appDir, imageName, name, {
          stream: logCtrl.stream,
          start: logCtrl.startPhase,
          end: logCtrl.endPhase,
          finalize: () => {} // Delay finalization until deployment is complete
        })
        log('createFromGit: Build successful')

        // Auto-detect port from Image EXPOSE if not manually specified
        let detectedPort = port
        if (!config.port) {
          try {
            const exposed = await Odac.server('Container').getImageExposedPorts(imageName)
            if (exposed && exposed.length > 0) {
              detectedPort = exposed[0]
              log('createFromGit: Auto-detected port from image: %d', detectedPort)
            }
          } catch (e) {
            log('createFromGit: Failed to detect port from image: %s', e.message)
          }
        }

        // Step 3: Create app record
        const gitMetadata = this.#getGitMetadata(url)
        const app = {
          id: this.#getNextId(),
          name,
          type: 'git',
          git: {
            repo: gitMetadata.repo,
            branch: branch || 'main',
            provider: gitMetadata.provider
          },
          url,
          branch,
          image: imageName,
          env: {
            manual: env.manual || Array.isArray(env.linked) ? env.manual || {} : env,
            linked: env.manual || Array.isArray(env.linked) ? env.linked || [] : config.linked || []
          },
          // Store internal port for Proxy routing (Metadata only, does not expose to host)
          ports: [{container: parseInt(detectedPort)}],
          dev,
          active: true,
          created: Date.now(),
          status: 'starting'
        }

        this.#apps.push(app)
        this.#saveApps()

        // Step 4: Run the container
        log('createFromGit: Starting container...')
        if (logCtrl) logCtrl.startPhase('start_new_container')
        await this.#runGitApp(app)
        if (logCtrl) logCtrl.endPhase('start_new_container', true)

        this.#set(app.id, {status: 'running', started: Date.now()})
        log('createFromGit: App started successfully')

        // Notify Hub
        Odac.server('Hub').trigger('app.list')

        if (logCtrl) await logCtrl.finalize(true)
        return Odac.server('Api').result(true, __('App %s deployed successfully.', name))
      } catch (e) {
        error('createFromGit: Failed: %s', e.message)
        if (logCtrl) await logCtrl.finalize(false)
        if (fs.existsSync(appDir)) {
          fs.rmSync(appDir, {recursive: true, force: true})
        }
        return Odac.server('Api').result(false, e.message)
      }
    } finally {
      this.#creating.delete(name)
    }
  }

  async #runGitApp(app) {
    const volumes = []
    if (app.dev) {
      // Mount total app directory to /app for live development
      const appDir = path.join(Odac.core('Config').config.app.path, app.name)
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
      env.ODAC_API_KEY = api.generateAppToken(app.name, app.api)

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
      env
    }

    // Enterprise Security Exception:
    // In Dev Mode, we mount the host directory which is owned by the host user/root.
    // To prevent 'EACCES: permission denied' when the container tries to write (npm install, logs),
    // we must run the container as root. This is acceptable for development environments.
    if (app.dev) {
      log('Active Dev Mode detected for %s. Forcing container to run as ROOT to handle volume permissions.', app.name)
      runOptions.user = 'root'
    }

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
    if (attempts >= 20) return // Give up after 20 seconds

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
          // Timeout reached, accept the random port
          // Prefer 80/8080/3000 if available in the list to avoid random ancillary ports
          const preferred = listeningPorts.find(p => [80, 8080, 3000, 5000].includes(p)) || listeningPorts[0]

          log('Auto-Discovery: App %s is listening on port %d (expected %d). Updating config...', app.name, preferred, expectedPort)
          app.ports = [{container: preferred}]

          // Also try to capture and save the current IP to "warm up" the cache
          try {
            const currentIP = await container.getIP(app.name)
            if (currentIP) app.ip = currentIP
          } catch {
            /* ignore */
          }

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

  async stopAll() {
    log('Stopping all apps...')
    for (const app of [...this.#apps]) {
      await this.stop(app.id)
    }
  }

  #logStreams = new Map() // app.name -> logCtrl
  #loggers = new Map() // app.name -> Logger instance

  async #getLogger(appName) {
    let logger = this.#loggers.get(appName)
    if (!logger) {
      logger = new Logger(appName)
      this.#loggers.set(appName, logger)
    }
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
    return logger.subscribe(callback)
  }

  async delete(id) {
    const app = this.#get(id)

    if (!app) {
      return Odac.server('Api').result(false, __('App %s not found.', id))
    }

    await this.stop(app.id)
    this.#apps = this.#apps.filter(s => s.name !== app.name && s.id !== app.id)
    this.#saveApps()

    await Odac.server('Container').remove(app.name)
    this.#loggers.delete(app.name)

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

    // Stop the app first
    await this.stop(app.id)

    // Wait a brief moment to ensure resources are released (optional but often helpful in container envs)
    await new Promise(resolve => setTimeout(resolve, 1000))

    // Start it again
    this.#set(id, {active: true})
    if (await this.#run(app.id)) {
      // Notify Hub
      Odac.server('Hub').trigger('app.list')
      return Odac.server('Api').result(true, __('App %s restarted successfully.', app.name))
    }

    return Odac.server('Api').result(false, __('Failed to restart app %s.', app.name))
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
      if (/[;&|`$(){}<>]/.test(url)) {
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
    if (branch && (branch.startsWith('-') || /[;&|`$(){}<>]/.test(branch))) {
      return Odac.server('Api').result(false, __('Invalid branch name format.'))
    }

    const targetBranch = branch || app.branch || 'main'

    // Concurrency guard: prevent parallel operations on the same app
    if (this.#processing.has(app.id)) {
      return Odac.server('Api').result(false, __('App %s is already being processed.', app.name))
    }

    this.#processing.add(app.id)
    log('Redeploying app %s (branch: %s, commit: %s)', app.name, targetBranch, commitSha || 'HEAD')
    let logCtrl = null

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

      const logger = await this.#getLogger(app.name)
      const buildId = `build_${Date.now()}`
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
        finalize: () => {} // Delay finalization until deployment is complete
      })

      // Step 3: Stop running container (minimal downtime starts here)
      if (logCtrl) logCtrl.startPhase('stop_old_container')
      await this.stop(app.id)
      if (logCtrl) logCtrl.endPhase('stop_old_container', true)

      // Step 4: Restart with new image (inline instead of delegating to #run
      // to keep #processing lock held for the entire operation lifecycle)
      this.#set(app.id, {active: true, status: 'starting'})

      if (logCtrl) logCtrl.startPhase('start_new_container')
      await this.#runGitApp(app)
      if (logCtrl) logCtrl.endPhase('start_new_container', true)

      this.#set(app.id, {status: 'running', started: Date.now()})

      if (logCtrl) logCtrl.startPhase('proxy_propagation')
      Odac.server('Proxy').syncConfig()
      if (logCtrl) logCtrl.endPhase('proxy_propagation', true)

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
      this.#set(app.id, {status: 'errored'})
      return Odac.server('Api').result(false, __('Redeploy failed: %s', err.message))
    } finally {
      this.#processing.delete(app.id)
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
      if (isRunning && statusInfo.startTime) {
        copy.started = new Date(statusInfo.startTime).getTime()
      }

      if (detailed === true) {
        delete copy.pid
        delete copy.ip
        delete copy.uptime

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
  async #run(id) {
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
        await this.#runContainer(app)
      } else if (app.type === 'git') {
        await this.#runGitApp(app)
      }

      this.#set(id, {status: 'running', started: Date.now()})

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

    const env = {ODAC_APP: 'true'}
    const volumes = [{host: dir, container: '/app'}]

    // API Permission Injection
    if (app.api) {
      const api = Odac.server('Api')
      env.ODAC_API_KEY = api.generateAppToken(app.name, app.api)

      if (api.hostSocketDir) {
        volumes.push({host: api.hostSocketDir, container: '/odac:ro'})
        env.ODAC_API_SOCKET = '/odac/api.sock'
      }
    }

    await Odac.server('Container').runApp(app.name, {
      image: runner.image,
      cmd: [runner.cmd, ...(runner.args || []), filename],
      volumes,
      env
    })
  }

  async #runContainer(app) {
    if (!Odac.server('Container').available) {
      throw new Error('Docker is not available via Container service.')
    }

    const env = this.#resolveEnv(app)
    const volumes = [...(app.volumes || [])]

    // API Permission Injection
    if (app.api) {
      const api = Odac.server('Api')
      env.ODAC_API_KEY = api.generateAppToken(app.name, app.api)

      if (api.hostSocketDir) {
        volumes.push({host: api.hostSocketDir, container: '/odac:ro'})
        env.ODAC_API_SOCKET = '/odac/api.sock'
      }
    }

    await Odac.server('Container').runApp(app.name, {
      image: app.image,
      ports: app.ports,
      volumes,
      env
    })

    // Start Runtime Logging
    // Start Runtime Logging
    // Start Runtime Logging
    await this.#attachLogger(app)
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

  #getNextId() {
    return this.#apps.reduce((maxId, app) => Math.max(app.id, maxId), -1) + 1
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
   * @param {string} url - Git URL
   * @returns {object} - {repo, provider}
   */
  #getGitMetadata(url) {
    if (!url) return {repo: '', provider: 'git'}
    const lower = url.toLowerCase()
    let provider = 'git'
    if (lower.includes('github.com')) provider = 'github'
    else if (lower.includes('gitlab.com')) provider = 'gitlab'
    else if (lower.includes('bitbucket.org')) provider = 'bitbucket'

    let repo = ''
    try {
      if (url.includes('@') && url.includes(':')) {
        // SSH: git@github.com:user/repo.git
        repo = url.split(':').pop()
      } else {
        // HTTPS: https://github.com/user/repo.git
        const parts = url.replace(/\/$/, '').split('/')
        if (parts.length >= 2) {
          const repoPart = parts.pop()
          const userPart = parts.pop()
          repo = `${userPart}/${repoPart}`
        }
      }

      if (repo.endsWith('.git')) repo = repo.slice(0, -4)
    } catch {
      repo = url
    }

    return {repo, provider}
  }

  #generatePassword(length = 16) {
    return nodeCrypto
      .randomBytes(Math.ceil(length / 2))
      .toString('hex')
      .slice(0, length)
  }

  // Private: Recipe Preparation
  #prepareVolumes(recipeVolumes, appDir) {
    if (!recipeVolumes) return []

    return recipeVolumes.map(vol => {
      let host = vol.host

      if (host === 'data') {
        host = path.join(appDir, 'data')
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

  #prepareEnv(recipeEnv) {
    if (!recipeEnv) return {}

    const env = {}
    for (const [key, value] of Object.entries(recipeEnv)) {
      if (typeof value === 'object' && value.generate) {
        env[key] = this.#generatePassword(value.length || 16)
      } else {
        env[key] = value
      }
    }

    return env
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

  // Private: Env Resolution
  #mergeRecipeEnv(recipe, userEnv = {}) {
    const defaultEnv = this.#prepareEnv(recipe.env)
    const defaultLinked = recipe.linked || []

    const userIsStructured = userEnv.manual || Array.isArray(userEnv.linked)
    const userManual = userIsStructured ? userEnv.manual || {} : userEnv
    const userLinked = userIsStructured ? userEnv.linked || [] : []

    // Merge: User overrides recipe defaults
    const manual = {...defaultEnv, ...userManual}

    // Linked: Recipe + User (Merge Arrays, Unique)
    const linkedSet = new Set([...defaultLinked, ...userLinked])
    const linked = [...linkedSet]

    return {manual, linked}
  }

  #resolveEnv(app) {
    const finalEnv = {ODAC_APP: 'true'}
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
          const linkedManual = linkedEnvConfig.manual || (linkedEnvConfig.linked ? {} : linkedEnvConfig)
          Object.assign(finalEnv, linkedManual)
        }
      }
    }

    // 2. Apply Manual Envs (Overrides linked)
    // If legacy, treat whole object as manual. If new, use .manual
    const manual = isNewStructure ? envConfig.manual || {} : envConfig
    Object.assign(finalEnv, manual)

    return finalEnv
  }
}

module.exports = new App()
