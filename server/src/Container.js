const {log, error} = Odac.core('Log', false).init('Container')
const childProcess = require('child_process')
const path = require('path')

class Container {
  constructor() {
    if (!Odac.core('Config').config.container) Odac.core('Config').config.container = {}

    try {
      childProcess.execSync('docker -v', {stdio: 'ignore'})
      Odac.core('Config').config.container.available = true
      log('Docker is available')
    } catch {
      Odac.core('Config').config.container.available = false
      error('Docker is not available')
    }
  }

  get available() {
    return Odac.core('Config').config.container.available
  }

  /**
   * Resolves container path to host path (for DooD support)
   * @param {string} localPath
   */
  #resolveHostPath(localPath) {
    if (!process.env.ODAC_HOST_ROOT) return localPath
    // If path starts with /app, replace it with ODAC_HOST_ROOT
    if (localPath.startsWith('/app')) {
      return path.join(process.env.ODAC_HOST_ROOT, localPath.substring(4))
    }
    // If path is relative (e.g. ./sites/...), make it absolute and transform
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
   * @param {string} volumePath - Host directory to mount (from container's perspective)
   * @param {string} command - Command to execute
   */
  exec(volumePath, command) {
    if (!this.available) return false

    const hostPath = this.#resolveHostPath(volumePath)

    const cmd = ['run', '--rm', '-v', `${hostPath}:/app`, '-w', '/app', 'node:lts-alpine', 'sh', '-c', command]

    try {
      childProcess.execFileSync('docker', cmd, {stdio: 'inherit'})
      return true
    } catch (e) {
      error(`Container exec error: ${e.message}`)
      return false
    }
  }

  /**
   * Creates and starts a container
   * @param {string} name - Container and Domain name
   * @param {number} port - External port (Host Port)
   * @param {string} volumePath - Host project directory
   */
  run(name, port, volumePath) {
    if (!this.available) return false

    // Clean up old container if exists
    this.remove(name)

    // Internal port used by the app inside container
    const internalPort = 1071
    const hostPath = this.#resolveHostPath(volumePath)

    const cmd = [
      'run',
      '-d',
      '--name',
      name,
      '--restart',
      'unless-stopped',
      '-p',
      `${port}:${internalPort}`,
      '-v',
      `${hostPath}:/app`,
      '-w',
      '/app',
      'node:lts-alpine',
      'sh',
      '-c',
      `npm install && node ./node_modules/odac/index.js`
    ]

    try {
      log(`Starting container for ${name}...`)
      childProcess.execFileSync('docker', cmd)
      return true
    } catch (e) {
      error(`Failed to start container for ${name}: ${e.message}`)
      return false
    }
  }

  stop(name) {
    if (!this.available) return
    try {
      childProcess.execSync(`docker stop ${name}`, {stdio: 'ignore'})
    } catch {
      /* ignore */
    }
  }

  remove(name) {
    if (!this.available) return
    try {
      childProcess.execSync(`docker rm -f ${name}`, {stdio: 'ignore'})
    } catch {
      /* ignore */
    }
  }

  /**
   * Streams container logs
   * @param {string} name
   * @returns {ChildProcess}
   */
  logs(name) {
    if (!this.available) return null
    return childProcess.spawn('docker', ['logs', '-f', name])
  }

  /**
   * Checks if container is running
   * @param {string} name
   * @returns {boolean}
   */
  isRunning(name) {
    if (!this.available) return false
    try {
      const output = childProcess
        .execSync(`docker inspect --format='{{.State.Running}}' ${name}`, {stdio: ['ignore', 'pipe', 'ignore']})
        .toString()
        .trim()
      return output === 'true'
    } catch {
      return false
    }
  }
}

module.exports = new Container()
