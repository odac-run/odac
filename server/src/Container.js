const {log, error} = Odac.core('Log', false).init('Container')
const Docker = require('dockerode')
const path = require('path')
const fs = require('fs')
const os = require('os')
const cp = require('child_process')

const NODE_IMAGE = 'node:lts-alpine'

class Container {
  #docker
  #activeBuilds = new Set() // Track active builds to prevent parallel builds for same app

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
    const image = NODE_IMAGE

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
   * Clones a git repository into a target directory
   * @param {string} gitUrl - Git repository URL
   * @param {string} branch - Branch to clone
   * @param {string} targetDir - Target directory on host
   * @param {string} [token] - Optional access token for private repos
   */
  async cloneRepo(gitUrl, branch, targetDir, token = null) {
    if (!this.available) {
      throw new Error('Docker is not available')
    }

    const hostPath = this.#resolveHostPath(targetDir)
    const image = 'alpine/git'

    log(`Cloning ${branch} branch to ${hostPath}`)

    try {
      await this.#ensureImage(image)

      const containerConfig = {
        Image: image,
        HostConfig: {
          Binds: [`${hostPath}:/repo`],
          AutoRemove: true
        }
      }

      if (token) {
        // Use shell to expand the token variable, keeping it out of the command string
        // Note: We assume the URL starts with https:// as per previous logic
        const urlWithoutProtocol = gitUrl.replace(/^https:\/\//, '')
        containerConfig.Entrypoint = ['/bin/sh', '-c']
        containerConfig.Cmd = [`git clone --depth 1 --branch "${branch}" "https://x-access-token:$GIT_TOKEN@${urlWithoutProtocol}" /repo`]
        containerConfig.Env = [`GIT_TOKEN=${token}`]
      } else {
        // Standard execution for public repos
        containerConfig.Entrypoint = ['git']
        containerConfig.Cmd = ['clone', '--depth', '1', '--branch', branch, gitUrl, '/repo']
      }

      // Create container for git clone
      const container = await this.#docker.createContainer(containerConfig)

      // Start and wait for completion
      await container.start()

      // Wait for container to finish
      const result = await container.wait()

      if (result.StatusCode !== 0) {
        throw new Error(`Git clone failed with exit code ${result.StatusCode}`)
      }

      log('Repository cloned successfully')
      return true
    } catch (err) {
      error(`Failed to clone repository: ${err.message}`)
      throw err
    }
  }
  /**
   * Builds a Docker image from source code using Nixpacks
   * Nixpacks CLI is automatically downloaded and cached on first use
   * @param {string} sourceDir - Source directory on host
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

    const hostPath = this.#resolveHostPath(sourceDir)
    const sandboxImage = 'docker:26-dind' // Downgrade to 26 for better compat with pack

    log(`Building image ${imageName} from ${hostPath} (Isolated Sandbox)`)

    try {
      await this.#ensureImage(sandboxImage)

      // Create a temporary volume or simple bind for output
      // We will assume sourceDir is writable or use a temp dir for output inside hostPath
      // To keep it clean, we output the tarball to the source directory
      // (which is mounted) and then load it from there.

      const runBuild = async (useCache = true) => {
        const packVersion = 'v0.33.2'
        // We need to know arch of the CONTAINER, not the HOST.
        // But docker:dind usually matches host arch.
        // URL for pack cli (using specific version for stability)

        // Shell script to run inside the isolated container
        // 1. Start dockerd
        // 2. Wait for it
        // 3. Install pack (download binary)
        // 4. Build
        // 5. Save image
        // 6. Chown to match host user (optional, but good for cleanup)

        const buildScript = `
          # Force API version for negotiation
          export DOCKER_API_VERSION=1.45

          # 1. Start internal Docker Daemon
          dockerd > /var/log/dockerd.log 2>&1 &
          PID=$!
          
          echo "Waiting for internal Docker Daemon..."
          TIMEOUT=0
          while ! docker info >/dev/null 2>&1; do
            if [ $TIMEOUT -gt 30 ]; then
              echo "Timeout waiting for dockerd"
              echo "--- Daemon Logs ---"
              cat /var/log/dockerd.log
              echo "-------------------"
              exit 1
            fi
            sleep 1
            TIMEOUT=$((TIMEOUT+1))
          done
          echo "Internal Docker Daemon is ready."
          
          cd /app
          BUILD_EXIT=0

          if [ -f "Dockerfile" ]; then
             echo "Dockerfile found! Using native Docker build..."
             docker build -t ${imageName} .
             BUILD_EXIT=$?
          else
             echo "No Dockerfile found. Using Cloud Native Buildpacks..."
             
             # Install pack CLI (Only needed if no Dockerfile)
             ARCH=$(uname -m)
             PACK_URL=""
             if [ "$ARCH" = "aarch64" ]; then
                PACK_URL="https://github.com/buildpacks/pack/releases/download/${packVersion}/pack-${packVersion}-linux-arm64.tgz"
             else
                PACK_URL="https://github.com/buildpacks/pack/releases/download/${packVersion}/pack-${packVersion}-linux.tgz"
             fi
             
             if [ -f "/odac-tools/pack" ]; then
               echo "Using cached pack CLI..."
               cp /odac-tools/pack /usr/local/bin/pack
             else
               echo "Downloading pack CLI from $PACK_URL..."
               wget -qO- "$PACK_URL" | tar -xz -C /usr/local/bin
               # Cache it
               mkdir -p /odac-tools
               cp /usr/local/bin/pack /odac-tools/pack
             fi
             
             echo "Starting pack build..."
             pack build ${imageName} --path . --builder heroku/builder:24 --trust-builder --pull-policy if-not-present --verbose
             BUILD_EXIT=$?
          fi
          
          if [ $BUILD_EXIT -ne 0 ]; then
             echo "Build failed."
             exit $BUILD_EXIT
          fi
          
          # 4. Save Image to tarball
          echo "Saving image to /image.tar..."
          docker save ${imageName} -o /image.tar
          exit $?
        `

        const binds = [
          `${hostPath}:/app`,
          'odac-tools-cache:/odac-tools' // Cache binaries like pack
        ]

        if (useCache) {
          binds.push('odac-build-cache:/var/lib/docker') // Cache docker images/layers
        } else {
          log('[sandbox] Building WITHOUT cache (fresh start)...')
        }

        const container = await this.#docker.createContainer({
          Image: sandboxImage,
          Entrypoint: [],
          Cmd: ['sh', '-c', buildScript],
          Env: ['DOCKER_TLS_CERTDIR='],
          HostConfig: {
            Binds: binds,
            Privileged: true,
            AutoRemove: false
          }
        })

        await container.start()

        let buildLogs = ''
        const stream = await container.logs({
          follow: true,
          stdout: true,
          stderr: true
        })

        stream.on('data', chunk => {
          const line = chunk.toString('utf8').trim()
          if (line) {
            log(`[sandbox] ${line}`)
            buildLogs += line + '\n'
          }
        })

        const result = await container.wait()

        // Wait a bit for stream to flush
        await new Promise(resolve => setTimeout(resolve, 500))

        // If successful, load the image into Host Docker
        if (result.StatusCode === 0) {
          log(`[sandbox] Loading image ${imageName} into host docker...`)
          // We use 'docker load' on the host via exec or direct API?
          // Since we don't have direct API for loading from file easily without reading it into memory,
          // and the file is in hostPath/image.tar, we can use a helper container or stream it.
          // Easiest: Use a small helper container to load it, OR simply read it if small.
          // Images can be large. Better to use a container with socket mounted just for LOADING.
          // Or since this is Container.js running on Host, we can use the 'docker' CLI if installed,
          // But we should stick to 'dockerode'.

          // Dockerode loadImage takes a stream.
          // We can create a read stream from the file on disk (since hostPath is local).

          try {
            const stream = await container.getArchive({path: '/image.tar'})

            const dlTempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'odac-dl-'))
            const tarPath = path.join(dlTempDir, 'output.tar')

            const fileStream = fs.createWriteStream(tarPath)

            await new Promise((resolve, reject) => {
              stream.pipe(fileStream)
              stream.on('end', resolve)
              stream.on('error', reject)
            })

            log(`[sandbox] Extracted archive to ${tarPath}, unpacking...`)

            try {
              cp.execSync(`tar -xf ${tarPath} -C ${dlTempDir}`)

              const imageTarPath = path.join(dlTempDir, 'image.tar')
              if (fs.existsSync(imageTarPath)) {
                log(`[sandbox] Loading image.tar into Docker...`)
                await this.#docker.loadImage(fs.createReadStream(imageTarPath))
                log(`[sandbox] Image loaded successfully.`)
              } else {
                throw new Error('image.tar missing in archive')
              }
            } finally {
              fs.rmSync(dlTempDir, {recursive: true, force: true})
            }
          } catch (loadErr) {
            error(`[sandbox] Failed to load image: ${loadErr.message}`)
            return {exitCode: 1, logs: buildLogs + '\nLoad Failed: ' + loadErr.message}
          }
        }

        // Cleanup container (since AutoRemove is false)
        try {
          await container.remove({force: true})
        } catch {
          // ignore
        }

        return {exitCode: result.StatusCode, logs: buildLogs}
      }

      let result = await runBuild()

      // If npm ci failed, try to fix package-lock and retry
      if (result.exitCode !== 0 && result.logs.includes('npm ci')) {
        log('[build] npm ci failed - attempting to sync package-lock.json...')

        await this.#syncPackageLock(hostPath)

        log('[build] Retrying build...')
        result = await runBuild() // Retry with cache (default)
      }

      // If export failed (disk/cache issue), retry WITHOUT cache
      if (result.exitCode !== 0 && result.logs.includes('failed to export')) {
        log('[build] Export failed (Cache issue?) - Retrying WITHOUT cache...')
        result = await runBuild(false)
      }

      if (result.exitCode !== 0) {
        throw new Error(`Build failed with exit code ${result.exitCode}`)
      }

      log(`Image ${imageName} built successfully`)
      return true
    } catch (err) {
      error(`Failed to build image: ${err.message}`)
      throw err
    } finally {
      // Always clean up the lock, even on error
      this.#activeBuilds.delete(imageName)
    }
  }
  /**
   * Syncs package-lock.json with package.json for Node.js projects
   * @param {string} hostPath - Path to the app source
   */
  async #syncPackageLock(hostPath) {
    log('[build] Syncing package-lock.json...')

    const nodeImage = NODE_IMAGE
    await this.#ensureImage(nodeImage)

    const container = await this.#docker.createContainer({
      Image: nodeImage,
      Cmd: ['npm', 'install', '--package-lock-only'],
      WorkingDir: '/app',
      HostConfig: {
        Binds: [`${hostPath}:/app`],
        AutoRemove: true
      }
    })

    await container.start()
    const result = await container.wait()

    if (result.StatusCode !== 0) {
      throw new Error('Failed to sync package-lock.json')
    }

    log('[build] package-lock.json synced successfully')
    log('[build] TIP: Commit this file to your repository to avoid this step')
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
