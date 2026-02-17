const {log, error} = Odac.core('Log', false).init('Container')
const Docker = require('dockerode')
const path = require('path')

const NODE_IMAGE = 'node:lts-alpine'

const Builder = require('./Container/Builder')
const Logger = require('./Container/Logger')

class Container {
  #docker
  #builder
  #activeBuilds = new Set() // Track active builds to prevent parallel builds for same app
  #buildLoggers = new Map() // appName -> Logger instance

  async getLastBuildLog(appName) {
    try {
      const logger = new Logger(appName)
      await logger.init() // Ensure dirs exist
      return logger.readLastBuildLog()
    } catch {
      return ''
    }
  }

  subscribeToBuildLogs(appName, cb) {
    const logger = this.#buildLoggers.get(appName)
    if (!logger) return null
    return logger.subscribe(cb)
  }

  constructor() {
    if (!Odac.core('Config').config.container) Odac.core('Config').config.container = {}

    // Initialize dockerode using default socket or from env DOCKER_HOST
    this.#docker = new Docker()
    this.#builder = new Builder(this.#docker)

    this.#checkAvailability()
  }

  get available() {
    return Odac.core('Config').config.container?.available || false
  }

  async #checkAvailability() {
    try {
      await this.#docker.ping()
      Odac.core('Config').config.container.available = true
      log('Docker is available')
    } catch {
      Odac.core('Config').config.container.available = false
      error('Docker is not available')
    }
  }

  /**
   * Ensures that the specified docker network exists
   * @param {string} networkName
   */
  async #ensureNetwork(networkName) {
    try {
      const networks = await this.#docker.listNetworks()
      const found = networks.find(n => n.Name === networkName)
      if (!found) {
        log(`Creating network ${networkName}...`)
        await this.#docker.createNetwork({
          Name: networkName,
          Driver: 'bridge'
        })
      }
    } catch (err) {
      error(`Failed to ensure network ${networkName}: ${err.message}`)
    }
  }

  /**
   * Resolves container path to host path (for DooD support)
   * @param {string} localPath
   */
  #resolveHostPath(localPath) {
    if (!process.env.ODAC_HOST_ROOT) {
      log(`[DEBUG] resolveHostPath: No ODAC_HOST_ROOT, returning as-is: ${localPath}`)
      return localPath
    }
    if (localPath.startsWith('/app')) {
      const result = path.join(process.env.ODAC_HOST_ROOT, localPath.substring(4))
      log(`[DEBUG] resolveHostPath: /app prefix found. ${localPath} -> ${result}`)
      return result
    }
    if (!path.isAbsolute(localPath)) {
      const absPath = path.resolve(localPath)
      if (absPath.startsWith('/app')) {
        const result = path.join(process.env.ODAC_HOST_ROOT, absPath.substring(4))
        log(`[DEBUG] resolveHostPath: Relative path resolved. ${localPath} -> ${result}`)
        return result
      }
    }
    log(`[DEBUG] resolveHostPath: No transformation. ${localPath}`)
    return localPath
  }

  /**
   * Helper to pull image if not exists
   */
  async #ensureImage(imageName) {
    try {
      const image = this.#docker.getImage(imageName)
      const inspect = await image.inspect().catch(() => null)
      if (!inspect) {
        log(`Pulling image ${imageName}...`)
        await new Promise((resolve, reject) => {
          this.#docker.pull(imageName, (err, stream) => {
            if (err) return reject(err)
            this.#docker.modem.followProgress(stream, onFinished, onProgress)

            function onFinished(err, output) {
              if (err) return reject(err)
              resolve(output)
            }
            function onProgress() {
              // Optional: log progress
            }
          })
        })
        log(`Image ${imageName} pulled successfully.`)
      }
    } catch (err) {
      error(`Failed to pull image ${imageName}: ${err.message}`)
      throw err
    }
  }

  // ... (previous methods)

  /**
   * Builds a Docker image from source code using Native Builder
   * @param {string} sourceDir - Internal source directory
   * @param {string} imageName - Name for the built image
   * @param {string} [appName] - Optional app name for logging context
   * @param {Object} [activeLogger] - Optional active logger instance
   */
  async build(sourceDir, imageName, appName = null, activeLogger = null) {
    if (!this.available) {
      throw new Error('Docker is not available')
    }

    // Prevent parallel builds for the same image
    if (this.#activeBuilds.has(imageName)) {
      throw new Error(`Build already in progress for ${imageName}`)
    }
    this.#activeBuilds.add(imageName)

    // Setup Logger for Streaming
    let logger = activeLogger
    if (!logger && appName) {
      try {
        logger = new Logger(appName)
        await logger.init()
        this.#buildLoggers.set(appName, logger)
      } catch (e) {
        error('Failed to init build logger for %s: %s', appName, e.message)
      }
    }

    try {
      const hostPath = this.#resolveHostPath(sourceDir)
      const name = appName || path.basename(sourceDir)

      // We pass both paths to the builder:
      // internalPath: for reading package.json and writing temp Dockerfiles (FS ops)
      // hostPath: for mounting into the runner containers (Docker ops)
      await this.#builder.build(
        {
          internalPath: sourceDir,
          hostPath: hostPath,
          appName: name, // Pass explicit or derived appName
          logger: logger // Use the prepared logger
        },
        imageName
      )

      return true
    } finally {
      this.#activeBuilds.delete(imageName)
      if (appName) this.#buildLoggers.delete(appName)
    }
  }

  /**
   * Clones a git repository using an isolated container
   * @param {string} url - Git URL
   * @param {string} [branch] - Branch to clone (uses remote default if omitted)
   * @param {string} targetDir - Host directory to clone into
   * @param {string} [token] - Optional auth token
   * @param {Object} [activeLogger] - Optional logger instance
   */
  async cloneRepo(url, branch, targetDir, token, activeLogger = null) {
    if (!this.available) throw new Error('Docker is not available')

    const gitImage = 'alpine/git'
    if (activeLogger) activeLogger.startPhase('pull_git_image')
    await this.#ensureImage(gitImage)
    if (activeLogger) activeLogger.endPhase('pull_git_image', true)

    // Securely construct URL with token inside the container env if needed
    // But better: use git credentials helper or header.
    // For simplicity and security, we'll strip protocol and re-add with token if https
    let secureUrl = url
    if (token && url.startsWith('https://')) {
      // Basic auth insertion: https://oauth2:TOKEN@github.com/...
      const noProto = url.substring(8)
      secureUrl = `https://oauth2:${token}@${noProto}`
    }

    log(`[Git] Cloning ${url} (branch: ${branch || 'default'}) into isolated sandbox...`)

    const hostPath = this.#resolveHostPath(targetDir)

    // Run ephemeral git container
    const container = await this.#docker.createContainer({
      Image: gitImage,
      Cmd: ['clone', '--depth', '1', ...(branch ? ['--branch', branch] : []), secureUrl, '.'],
      WorkingDir: '/git',
      HostConfig: {
        Binds: [`${hostPath}:/git`],
        Privileged: false // SECURITY: Rootless git
      }
    })

    try {
      await container.start()
      const result = await container.wait()

      if (result.StatusCode !== 0) {
        // Fetch logs to debug failure
        const logs = await container.logs({stdout: true, stderr: true})
        let logStr = logs ? logs.toString('utf8') : 'No logs available'

        // Sanitize sensitive token from logs
        if (token) {
          logStr = logStr.replaceAll(token, '*****')
        }

        log(`[Git] Container Logs: ${logStr}`)
        throw new Error(`Git clone failed with exit code ${result.StatusCode}. Logs: ${logStr}`)
      }

      log('[Git] Clone successful.')
    } catch (err) {
      error(`[Git] Clone failed: ${err.message}`)
      throw err
    } finally {
      container.remove({force: true}).catch(() => {})
    }
  }

  /**
   * Fetches latest changes from a git repository into an existing clone
   * Uses ephemeral container to avoid requiring git in app containers
   * @param {string} url - Git remote URL
   * @param {string} branch - Branch to fetch
   * @param {string} targetDir - Host directory containing the existing clone
   * @param {string} [token] - Optional auth token for private repos
   * @param {string} [commitSha] - Optional specific commit to reset to
   * @param {Object} [activeLogger] - Optional logger instance
   */
  async fetchRepo(url, branch, targetDir, token, commitSha, activeLogger = null) {
    if (!this.available) throw new Error('Docker is not available')

    const gitImage = 'alpine/git'
    if (activeLogger) activeLogger.startPhase('pull_git_image')
    await this.#ensureImage(gitImage)
    if (activeLogger) activeLogger.endPhase('pull_git_image', true)

    let secureUrl = url
    if (token && url.startsWith('https://')) {
      const noProto = url.substring(8)
      secureUrl = `https://oauth2:${token}@${noProto}`
    }

    log('[Git] Fetching updates (branch: %s, commit: %s)...', branch || 'default', commitSha || 'HEAD')

    const hostPath = this.#resolveHostPath(targetDir)

    // Pass all dynamic values via env vars to prevent shell injection
    const envVars = [`GIT_BRANCH=${branch}`]
    if (commitSha) envVars.push(`GIT_COMMIT_SHA=${commitSha}`)
    if (token) {
      envVars.push(`GIT_REMOTE_URL=${secureUrl}`)
      envVars.push(`GIT_ORIGINAL_URL=${url}`)
    }

    // Build git command: fetch specific commit or branch head
    let gitCmd = commitSha
      ? 'git fetch --depth 1 origin "$GIT_COMMIT_SHA" && git reset --hard "$GIT_COMMIT_SHA"'
      : 'git fetch --depth 1 origin "$GIT_BRANCH" && git reset --hard "origin/$GIT_BRANCH"'

    // Temporarily inject authenticated URL for private repos, restore original after fetch
    if (token) {
      gitCmd = 'git remote set-url origin "$GIT_REMOTE_URL" && ' + gitCmd + ' && git remote set-url origin "$GIT_ORIGINAL_URL"'
    }

    const container = await this.#docker.createContainer({
      Image: gitImage,
      Entrypoint: ['sh', '-c'],
      Cmd: [gitCmd],
      WorkingDir: '/git',
      Env: envVars,
      HostConfig: {
        Binds: [`${hostPath}:/git`],
        Privileged: false
      }
    })

    try {
      await container.start()
      const result = await container.wait()

      if (result.StatusCode !== 0) {
        const logs = await container.logs({stdout: true, stderr: true})
        let logStr = logs ? logs.toString('utf8') : 'No logs available'
        if (token) logStr = logStr.replaceAll(token, '*****')
        log('[Git] Container Logs: %s', logStr)
        throw new Error(`Git fetch failed with exit code ${result.StatusCode}. Logs: ${logStr}`)
      }

      log('[Git] Fetch and reset successful.')
    } catch (err) {
      error('[Git] Fetch failed: %s', err.message)
      throw err
    } finally {
      container.remove({force: true}).catch(() => {})
    }
  }

  /**
   * Executes a command inside an existing running container
   * @param {string} name - Container name
   * @param {string} command - Command to execute
   */
  async execInContainer(name, command) {
    if (!this.available) throw new Error('Docker not available')

    try {
      const container = this.#docker.getContainer(name)
      if (!(await this.isRunning(name))) {
        throw new Error(`Container ${name} is not running`)
      }

      // Create exec instance
      const exec = await container.exec({
        Cmd: ['sh', '-c', command],
        AttachStdout: true,
        AttachStderr: true
      })

      // Start exec and stream output
      const stream = await exec.start({})

      return new Promise((resolve, reject) => {
        let output = ''
        let errorOutput = ''

        container.modem.demuxStream(
          stream,
          {
            write: chunk => {
              output += chunk.toString('utf8')
            }
          },
          {
            write: chunk => {
              errorOutput += chunk.toString('utf8')
            }
          }
        )

        stream.on('end', async () => {
          try {
            const data = await exec.inspect()
            if (data.ExitCode !== 0) {
              reject(new Error(`Command failed (Exit Code ${data.ExitCode}): ${errorOutput || output}`))
            } else {
              resolve(output)
            }
          } catch (e) {
            reject(e)
          }
        })

        stream.on('error', reject)
      })
    } catch (err) {
      throw new Error(`Failed to exec command in ${name}: ${err.message}`)
    }
  }

  /**
   * Creates and starts a container
   * @param {string} name - Container and Domain name
   * @param {number} port - External port (Host Port)
   * @param {string} volumePath - Host project directory
   */
  async run(name, port, volumePath, extraBinds = [], options = {}) {
    if (!this.available) return false

    await this.remove(name)

    const internalPort = 1071
    const hostPath = this.#resolveHostPath(volumePath)

    const bindings = [`${hostPath}:/app`]

    // Mount API socket directory for container communication (bypasses network issues)
    const socketDir = Odac.server('Api').hostSocketDir
    if (socketDir) {
      const hostSocketDir = this.#resolveHostPath(socketDir)
      bindings.push(`${hostSocketDir}:/odac:ro`)
    }

    if (extraBinds && Array.isArray(extraBinds)) {
      bindings.push(...extraBinds)
    }

    const envArr = []
    if (options && options.env) {
      for (const [key, val] of Object.entries(options.env)) {
        envArr.push(`${key}=${val}`)
      }
    }

    try {
      const networkName = 'odac-network'
      await this.#ensureNetwork(networkName)

      log(`Starting container for ${name}...`)
      await this.#ensureImage(NODE_IMAGE)

      const container = await this.#docker.createContainer({
        Image: NODE_IMAGE,
        Cmd: [
          'sh',
          '-c',
          'if [ ! -d node_modules ] || [ package.json -nt node_modules ]; then npm install; fi; npm run build --if-present; exec npm start'
        ],
        name: name,
        WorkingDir: '/app',
        Env: envArr,
        HostConfig: {
          RestartPolicy: {Name: 'unless-stopped'},
          Binds: bindings,
          NetworkMode: networkName,
          ExtraHosts: ['host.docker.internal:host-gateway']
        },
        ExposedPorts: {
          [`${internalPort}/tcp`]: {}
        }
      })

      await container.start()
      return true
    } catch (err) {
      error(`Failed to start container for ${name}: ${err.message}`)
      return false
    }
  }

  /**
   * Generic method to run any app container
   * @param {string} name - Unique container name
   * @param {Object} options - Configuration options
   * @param {string} options.image - Docker image name
   * @param {Array} options.ports - Array of port mappings [{host: 3000, container: 80}]
   * @param {Array} options.volumes - Array of volume mappings [{host: '/path', container: '/data'}]
   * @param {Object} options.env - Environment variables {KEY: 'VALUE'}
   * @param {Array} options.cmd - Command to run (optional)
   * @param {string} options.user - User to run as (optional, e.g., 'root')
   */
  async runApp(name, options) {
    if (!this.available) return false

    await this.remove(name)

    const bindings = []
    if (options.volumes) {
      for (const vol of options.volumes) {
        const hostPath = this.#resolveHostPath(vol.host)
        bindings.push(`${hostPath}:${vol.container}`)
      }
    }

    const portBindings = {}
    const exposedPorts = {}

    if (options.ports) {
      for (const port of options.ports) {
        const portKey = `${port.container}/tcp`
        const bindIp = port.ip || '127.0.0.1'
        portBindings[portKey] = [{HostPort: String(port.host), HostIp: bindIp}]
        exposedPorts[portKey] = {}
      }
    }

    const envArr = []
    if (options.env) {
      for (const [key, val] of Object.entries(options.env)) {
        envArr.push(`${key}=${val}`)
      }
    }

    try {
      const networkName = 'odac-network'
      await this.#ensureNetwork(networkName)

      log(`Starting app container ${name} (${options.image})...`)
      await this.#ensureImage(options.image)

      const containerConfig = {
        Image: options.image,
        name: name,
        Env: envArr,
        HostConfig: {
          RestartPolicy: {Name: 'unless-stopped'},
          Binds: bindings,
          PortBindings: portBindings,
          NetworkMode: networkName
        },
        ExposedPorts: exposedPorts
      }

      if (options.cmd) {
        containerConfig.Cmd = options.cmd
      }

      if (options.user) {
        containerConfig.User = options.user
      }

      const container = await this.#docker.createContainer(containerConfig)

      await container.start()
      return true
    } catch (err) {
      error(`Failed to start app container ${name}: ${err.message}`)
      throw err
    }
  }

  async stop(name) {
    if (!this.available) return
    try {
      const container = this.#docker.getContainer(name)
      await container.stop()
    } catch (err) {
      if (err.statusCode !== 404 && err.statusCode !== 304) {
        error(`Failed to stop container ${name}: ${err.message}`)
      }
    }
  }

  async remove(name) {
    if (!this.available) return
    try {
      const container = this.#docker.getContainer(name)
      // Force remove equivalent
      await container.remove({force: true})
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to remove container ${name}: ${err.message}`)
      }
    }
  }

  /**
   * Streams container logs
   * @param {string} name
   * @returns {Promise<import('stream').Readable>}
   */
  async logs(name) {
    if (!this.available) return null
    try {
      const container = this.#docker.getContainer(name)
      // Follow logs, include stdout and stderr
      const stream = await container.logs({
        follow: true,
        stdout: true,
        stderr: true
      })
      return stream
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to get logs for ${name}: ${err.message}`)
      }
      return null
    }
  }

  /**
   * Checks if container is running
   * @param {string} name
   * @returns {Promise<boolean>}
   */
  async isRunning(name) {
    if (!this.available) return false
    try {
      const container = this.#docker.getContainer(name)
      const data = await container.inspect()
      return data.State.Running
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to check if running ${name}: ${err.message}`)
      }
      return false
    }
  }

  /**
   * Lists all containers
   * @returns {Promise<Array>}
   */
  async list() {
    if (!this.available) return []
    try {
      const containers = await this.#docker.listContainers({all: true})
      return containers.map(c => ({
        id: c.Id.substring(0, 12),
        names: c.Names,
        image: c.Image,
        state: c.State,
        status: c.Status,
        created: c.Created,
        ports: c.Ports
      }))
    } catch (err) {
      error(`Failed to list containers: ${err.message}`)
      return []
    }
  }

  /**
   * Returns the container IP address
   * @param {string} name
   * @returns {Promise<string|null>}
   */
  async getIP(name) {
    if (!this.available) return null
    try {
      const container = this.#docker.getContainer(name)
      const data = await container.inspect()
      // Try to get IP from odac-network first, then fallback to first available network
      const networks = data.NetworkSettings.Networks
      if (networks['odac-network']) {
        return networks['odac-network'].IPAddress
      }
      return Object.values(networks)[0]?.IPAddress || null
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to get IP for ${name}: ${err.message}`)
      }
      return null
    }
  }

  /**
   * Returns container statistics (CPU, Memory, Network)
   * @param {string} name
   * @returns {Promise<Object|null>}
   */
  async getStats(name) {
    if (!this.available) return null
    try {
      const container = this.#docker.getContainer(name)
      const stats = await container.stats({stream: false})

      let cpuPercent = 0.0
      const cpuDelta = stats.cpu_stats.cpu_usage.total_usage - stats.precpu_stats.cpu_usage.total_usage
      const systemDelta = stats.cpu_stats.system_cpu_usage - stats.precpu_stats.system_cpu_usage
      const onlineCpus = stats.cpu_stats.online_cpus || stats.cpu_stats.cpu_usage.percpu_usage?.length || 1

      if (systemDelta > 0 && cpuDelta > 0) {
        cpuPercent = (cpuDelta / systemDelta) * onlineCpus * 100.0
      }

      const memUsage = stats.memory_stats.usage || 0
      const memLimit = stats.memory_stats.limit || 0
      const memPercent = memLimit > 0 ? (memUsage / memLimit) * 100.0 : 0

      let rxBytes = 0
      let txBytes = 0
      if (stats.networks) {
        for (const net of Object.values(stats.networks)) {
          rxBytes += net.rx_bytes
          txBytes += net.tx_bytes
        }
      }

      return {
        cpu_percent: parseFloat(cpuPercent.toFixed(2)),
        memory: {
          usage: memUsage,
          limit: memLimit,
          percent: parseFloat(memPercent.toFixed(2))
        },
        network: {
          rx_bytes: rxBytes,
          tx_bytes: txBytes
        },
        pids: stats.pids_stats.current || 0,
        timestamp: Date.now()
      }
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to get stats for ${name}: ${err.message}`)
      }
      return null
    }
  }

  /**
   * Returns the container environment variables
   * @param {string} name
   * @returns {Promise<Object>}
   */
  async getEnv(name) {
    if (!this.available) return {}
    try {
      const container = this.#docker.getContainer(name)
      const data = await container.inspect()
      const env = {}
      for (const e of data.Config.Env) {
        const parts = e.split('=')
        const key = parts[0]
        const val = parts.slice(1).join('=')
        env[key] = val
      }
      return env
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to get Env for ${name}: ${err.message}`)
      }
      return {}
    }
  }

  /**
   * Retrieves ExposedPorts from image configuration
   * @param {string} imageName
   * @returns {Promise<number[]>} Array of exposed ports
   */
  async getImageExposedPorts(imageName) {
    if (!this.available) return []
    try {
      const image = this.#docker.getImage(imageName)
      const data = await image.inspect()
      const exposed = data.Config.ExposedPorts || {}
      return Object.keys(exposed)
        .map(p => parseInt(p.split('/')[0]))
        .filter(p => !isNaN(p))
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to inspect image ${imageName}: ${err.message}`)
      }
      return []
    }
  }

  /**
   * Detects actively listening ports inside the container by reading /proc/net/tcp
   * This is much faster and reliable than port scanning.
   * works on all linux containers (alpine, debian etc)
   * @param {string} name - Container name
   * @returns {Promise<number[]>} Array of listening ports
   */
  async getListeningPorts(name) {
    if (!this.available) return []
    try {
      const ports = new Set()

      // Helper to parse proc file content
      const parseProc = output => {
        const lines = output.split('\n').filter(l => l.trim().length > 0)
        for (let i = 1; i < lines.length; i++) {
          const parts = lines[i].trim().split(/\s+/)
          if (parts.length < 4) continue

          const localAddress = parts[1]
          const state = parts[3]

          if (state === '0A') {
            const portHex = localAddress.split(':')[1]
            const ipHex = localAddress.split(':')[0]

            if (portHex) {
              const port = parseInt(portHex, 16)

              // Filter out loopback addresses (127.0.0.1)
              // 0100007F = 127.0.0.1 (IPv4 Loopback in Little Endian)
              // 00000000000000000000000001000000 = ::1 (IPv6 Loopback)
              const isLoopback = ipHex === '0100007F' || ipHex === '00000000000000000000000001000000'

              if (!isLoopback && port > 0 && port < 60000) {
                ports.add(port)
              }
            }
          }
        }
      }

      // Read both IPv4 and IPv6 tables
      // Some apps bind only to IPv6 (::) which covers IPv4 too
      const [tcp4, tcp6] = await Promise.all([
        this.execInContainer(name, 'cat /proc/net/tcp').catch(() => ''),
        this.execInContainer(name, 'cat /proc/net/tcp6').catch(() => '')
      ])

      if (tcp4) parseProc(tcp4)
      if (tcp6) parseProc(tcp6)

      return Array.from(ports)
    } catch {
      return []
    }
  }

  /**
   * Returns the Docker instance
   */
  get docker() {
    return this.#docker
  }

  /**
   * Gets detailed status of a container
   * @param {string} name
   * @returns {Promise<Object>} { running: boolean, restarts: number, startTime: string }
   */
  async getStatus(name) {
    if (!this.available) return {running: false, restarts: 0}
    try {
      const container = this.#docker.getContainer(name)
      const data = await container.inspect()
      return {
        running: data.State.Running,
        restarts: data.RestartCount || 0,
        startTime: data.State.StartedAt
      }
    } catch (err) {
      if (err.statusCode !== 404) {
        error(`Failed to get status for ${name}: ${err.message}`)
      }
      return {running: false, restarts: 0}
    }
  }
}

module.exports = new Container()
