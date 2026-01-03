const {log, error} = Odac.core('Log', false).init('Container')
const Docker = require('dockerode')
const path = require('path')

class Container {
  #docker

  constructor() {
    if (!Odac.core('Config').config.container) Odac.core('Config').config.container = {}

    // Initialize dockerode using default socket or from env DOCKER_HOST
    this.#docker = new Docker()

    this.#checkAvailability()
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

  get available() {
    return Odac.core('Config').config.container.available ?? false
  }

  /**
   * Resolves container path to host path (for DooD support)
   * @param {string} localPath
   */
  #resolveHostPath(localPath) {
    if (!process.env.ODAC_HOST_ROOT) return localPath
    if (localPath.startsWith('/app')) {
      return path.join(process.env.ODAC_HOST_ROOT, localPath.substring(4))
    }
    if (!path.isAbsolute(localPath)) {
      const absPath = path.resolve(localPath)
      if (absPath.startsWith('/app')) {
        return path.join(process.env.ODAC_HOST_ROOT, absPath.substring(4))
      }
    }
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

  /**
   * Executes a command inside a temporary ephemeral container
   * @param {string} volumePath - Host directory to mount
   * @param {string} command - Command to execute
   */
  async exec(volumePath, command, extraBinds = []) {
    if (!this.available) return false

    const hostPath = this.#resolveHostPath(volumePath)
    const image = 'node:lts-alpine'

    // We use run with remove: true to mimic 'docker run --rm'
    try {
      await this.#ensureImage(image)

      // Stream output to stdout/stderr
      await this.#docker.run(image, ['sh', '-c', command], [process.stdout, process.stderr], {
        HostConfig: {
          Binds: [`${hostPath}:/app`, ...extraBinds],
          AutoRemove: true
        },
        WorkingDir: '/app'
      })
      return true
    } catch (err) {
      error(`Container exec error: ${err.message}`)
      return false
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
      await this.#ensureImage('node:lts-alpine')

      const container = await this.#docker.createContainer({
        Image: 'node:lts-alpine',
        Cmd: [
          'sh',
          '-c',
          'if [ ! -d node_modules ] || [ package.json -nt node_modules ]; then npm install; fi; if [ ! -d route ]; then i=0; while [ ! -d route ] && [ $i -lt 24 ]; do node ./node_modules/odac/index.js; echo "Waiting for project init..."; sleep 5; i=$((i+1)); done; if [ ! -d route ]; then echo "Timeout waiting for project init"; exit 1; fi; fi; exec node ./node_modules/odac/index.js'
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
   * Returns the Docker instance
   */
  get docker() {
    return this.#docker
  }
}

module.exports = new Container()
