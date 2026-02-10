const {log, error} = Odac.core('Log', false).init('Container')
const Docker = require('dockerode')
const path = require('path')

const NODE_IMAGE = 'node:lts-alpine'

const Builder = require('./Container/Builder')

class Container {
  #docker
  #builder
  #activeBuilds = new Set() // Track active builds to prevent parallel builds for same app

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
   */
  async build(sourceDir, imageName) {
    if (!this.available) {
      throw new Error('Docker is not available')
    }

    // Prevent parallel builds for the same image
    if (this.#activeBuilds.has(imageName)) {
      throw new Error(`Build already in progress for ${imageName}`)
    }
    this.#activeBuilds.add(imageName)

    try {
      const hostPath = this.#resolveHostPath(sourceDir)

      // We pass both paths to the builder:
      // internalPath: for reading package.json and writing temp Dockerfiles (FS ops)
      // hostPath: for mounting into the runner containers (Docker ops)
      await this.#builder.build(
        {
          internalPath: sourceDir,
          hostPath: hostPath
        },
        imageName
      )

      return true
    } finally {
      this.#activeBuilds.delete(imageName)
    }
  }

  /**
   * Clones a git repository using an isolated container
   * @param {string} url - Git URL
   * @param {string} [branch] - Branch to clone (uses remote default if omitted)
   * @param {string} targetDir - Host directory to clone into
   * @param {string} [token] - Optional auth token
   */
  async cloneRepo(url, branch, targetDir, token) {
    if (!this.available) throw new Error('Docker is not available')

    const gitImage = 'alpine/git'
    await this.#ensureImage(gitImage)

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

    try {
      // Run ephemeral git container
      const container = await this.#docker.createContainer({
        Image: gitImage,
        Cmd: ['clone', '--depth', '1', ...(branch ? ['--branch', branch] : []), secureUrl, '.'],
        WorkingDir: '/git',
        HostConfig: {
          Binds: [`${hostPath}:/git`],
          AutoRemove: true,
          Privileged: false // SECURITY: Rootless git
        }
      })

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
      bindings.push(`${hostSocketDir}:/run/odac:ro`)
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
   * Returns the Docker instance
   */
  get docker() {
    return this.#docker
  }
}

module.exports = new Container()
