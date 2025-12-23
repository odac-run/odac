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
   * Executes a command inside a temporary ephemeral container
   * @param {string} volumePath - Host directory to mount
   * @param {string} command - Command to execute
   */
  async exec(volumePath, command, extraBinds = []) {
    if (!this.available) return false

    const hostPath = this.#resolveHostPath(volumePath)

    // We use run with remove: true to mimic 'docker run --rm'
    try {
      // Stream output to stdout/stderr
      await this.#docker.run('node:lts-alpine', ['sh', '-c', command], [process.stdout, process.stderr], {
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
   * Creates and starts a container
   * @param {string} name - Container and Domain name
   * @param {number} port - External port (Host Port)
   * @param {string} volumePath - Host project directory
   */
  async run(name, port, volumePath, extraBinds = []) {
    if (!this.available) return false

    await this.remove(name)

    const internalPort = 1071
    const hostPath = this.#resolveHostPath(volumePath)

    const bindings = [`${hostPath}:/app`]

    if (extraBinds && Array.isArray(extraBinds)) {
      bindings.push(...extraBinds)
    }

    try {
      log(`Starting container for ${name}...`)

      const container = await this.#docker.createContainer({
        Image: 'node:lts-alpine',
        Cmd: ['sh', '-c', 'npm install && node ./node_modules/odac/index.js'],
        name: name,
        WorkingDir: '/app',
        HostConfig: {
          RestartPolicy: {Name: 'unless-stopped'},
          Binds: bindings,
          PortBindings: {
            [`${internalPort}/tcp`]: [{HostPort: String(port)}]
          }
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

  async stop(name) {
    if (!this.available) return
    try {
      const container = this.#docker.getContainer(name)
      await container.stop()
    } catch {
      /* ignore */
    }
  }

  async remove(name) {
    if (!this.available) return
    try {
      const container = this.#docker.getContainer(name)
      // Force remove equivalent
      await container.remove({force: true})
    } catch {
      /* ignore */
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
    } catch {
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
    } catch {
      return false
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
