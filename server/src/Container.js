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
    } catch (err) {
      Odac.core('Config').config.container.available = false
      error(`Docker is not available: ${err.message}`)
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

    // Handle .odac paths to parent directory if in ODAC_DEV mode
    if (process.env.ODAC_DEV === 'true' && localPath.includes('.odac/')) {
      const relPath = localPath.substring(localPath.indexOf('.odac/'))
      return path.join(path.dirname(process.env.ODAC_HOST_ROOT), relPath)
    }

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
    if (!this.available) return false

    try {
      const container = this.#docker.getContainer(name)
      if (!(await this.isRunning(name))) {
        error(`Container ${name} is not running`)
        return false
      }

      // Create exec instance
      const exec = await container.exec({
        Cmd: ['sh', '-c', command],
        AttachStdout: true,
        AttachStderr: true
      })

      // Start exec and stream output
      const stream = await exec.start({})

      // Dockerode returns a stream which might be multiplexed (header+payload)
      // For simplicity in this context, we just pipe it to process stdout/stderr
      // In a robust implementation, we should demux it
      container.modem.demuxStream(stream, process.stdout, process.stderr)

      // Wait for command completion
      return new Promise(resolve => {
        stream.on('end', () => resolve(true))
        stream.on('error', err => {
          error(`Exec stream error: ${err.message}`)
          resolve(false)
        })
      })
    } catch (err) {
      error(`Failed to exec command in ${name}: ${err.message}`)
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
      await this.#ensureImage('node:lts-alpine')

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
    } catch (err) {
      if (err.statusCode !== 404) {
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
   * Returns the Docker instance
   */
  get docker() {
    return this.#docker
  }
}

module.exports = new Container()
