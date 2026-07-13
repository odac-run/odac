const Docker = require('dockerode')
const {exec} = require('child_process')
const util = require('util')
const fs = require('fs')
const path = require('path')
const os = require('os')
const net = require('net')
const {Writable} = require('stream')
const {StringDecoder} = require('string_decoder')

const execAsync = util.promisify(exec)
const {log, error} = Odac.core('Log', false).init('Updater')

const CONTAINER_NAME = 'odac'
const UPDATE_CONTAINER_NAME = 'odac-update'
const BACKUP_CONTAINER_NAME = 'odac-backup'
const RUNNER_IMAGE = 'docker:cli'

// Env vars the update cycle injects into the incoming container. They describe a
// single handover, so they are stripped from the inherited env before the next
// one is built — otherwise every update appends another copy.
const UPDATE_ENV_KEYS = [
  'ODAC_UPDATE_MODE',
  'ODAC_INSTANCE_ID',
  'ODAC_PREVIOUS_INSTANCE_ID',
  'ODAC_UPDATE_SOCKET_PATH',
  'ODAC_LOG_NAME',
  'ODAC_PREVIOUS_CONTAINER_NAME'
]

class Updater {
  #updating = false
  #isUpdateMode = false
  #image = 'odacrun/odac:latest'
  #docker = new Docker({socketPath: '/var/run/docker.sock'})
  #readyCallbacks = []
  #isReady = false

  get #channel() {
    return process.env.ODAC_CHANNEL || 'stable'
  }

  get #isBuildMode() {
    return this.#channel !== 'stable' && this.#channel !== 'latest'
  }

  get #targetBranch() {
    return this.#channel === 'beta' ? 'dev' : this.#channel
  }

  /**
   * Returns the base persistent storage path shared across containers.
   * Derives from HOME env to support different volume mount configurations.
   */
  get #basePath() {
    return path.join(os.homedir(), '.odac')
  }

  /**
   * Returns the Unix domain socket path used for the zero-downtime handshake protocol.
   * Must reside on the shared volume so both old and new containers can access it.
   */
  get #socketPath() {
    return path.join(this.#basePath, 'run', 'update.sock')
  }

  get #downloadPath() {
    return path.join(this.#basePath, 'tmp', 'odac_source')
  }

  /**
   * Initialize Updater.
   * Checks if an update socket exists, meaning we are the new instance in an update process.
   */
  async init() {
    const socketPath = this.#socketPath

    if (fs.existsSync(socketPath)) {
      log('Update socket found. Attempting handshake with previous process...')
      try {
        await this.#performHandshake(socketPath)
        this.#isUpdateMode = true
        // Handshake successful, performHandshake handles the rest (waiting for handover which triggers callbacks)
        return
      } catch (e) {
        log('Handshake failed or stale socket: %s. Continuing as normal startup.', e.message)
        // Clean up stale socket
        try {
          fs.unlinkSync(socketPath)
        } catch {
          // Ignore
        }
      }
    }

    // Normal startup (not updating or failed update check)
    // If we are starting up and have an update log name (e.g. after a restart), switch to standard logs
    if (process.env.ODAC_LOG_NAME && process.env.ODAC_LOG_NAME.includes('odac-update')) {
      console.log('ODAC_CMD:SWITCH_LOGS')
    }

    // Self-heal a host left inconsistent by a crashed or rolled-back update.
    // Identity first: #ensureRestartPolicy() looks the container up by name, so
    // the name has to be ours again before the policy can be repaired. Both are
    // idempotent and cheap — a no-op on healthy hosts.
    await this.#ensureIdentity()
    await this.#ensureRestartPolicy()

    this.#triggerReady()
  }

  /**
   * Register a callback to be called when the updater is ready (either immediately or after handover).
   * @param {Function} cb
   */
  onReady(cb) {
    if (this.#isReady) {
      cb()
    } else {
      this.#readyCallbacks.push(cb)
    }
  }

  #triggerReady() {
    if (this.#isReady) return
    this.#isReady = true
    for (const cb of this.#readyCallbacks) {
      try {
        cb()
      } catch (e) {
        error(e)
      }
    }
    this.#readyCallbacks = []
  }

  /**
   * Starts the update process.
   * @param {Object} command - The command object from Hub.
   * @param {Function} sendResponse - Callback to send response back to Hub.
   */
  async start() {
    if (this.#updating) {
      log('Update request blocked: Update already in progress')
      return Odac.server('Api').result(false, 'Update already in progress')
    }
    this.#updating = true

    let available
    try {
      available = await this.#checkForUpdates()
    } catch (e) {
      // A failing check (registry unreachable, docker pull error) must release the
      // latch — otherwise the first network hiccup blocks updates until restart.
      this.#updating = false
      error('Update check failed: %s', e.message)
      return Odac.server('Api').result(false, `Update check failed: ${e.message}`)
    }

    if (!available) {
      log('System is up to date.')
      this.#updating = false
      return Odac.server('Api').result(true, 'System is up to date')
    }
    setTimeout(async () => {
      try {
        await this.download()
        await this.execute()
      } catch (e) {
        this.#updating = false
        error('Update process failed: %s', e.message)
      }
    }, 1)
    return Odac.server('Api').result(true, 'Update process started')
  }

  /**
   * Check if we are running as an updater instance.
   */
  async check() {
    return this.#isUpdateMode
  }

  async #checkForUpdates() {
    if (this.#isBuildMode) {
      log('Custom channel detected (%s). Forcing update check to true.', this.#channel)
      return true
    }

    log('Checking for updates...')
    const localId = await this.#getLocalImageId()

    log(`Pulling ${this.#image}...`)
    // Pull the image to ensure we have the latest metadata and layers
    await execAsync(`docker pull ${this.#image}`)

    const remoteId = await this.#getRemoteImageId()

    if (!localId || !remoteId) {
      log('Failed to determine image IDs. Local: %s, Remote: %s', localId, remoteId)
      return false
    }

    if (localId === remoteId) {
      log('Image is up to date (%s)', localId.substring(0, 12))
      return false
    }

    log('Update available! Local: %s, Remote: %s', localId.substring(0, 12), remoteId.substring(0, 12))
    return true
  }

  /**
   * Downloads the update (image or source) to the local machine.
   * Ensures the artifact is available before execution.
   */
  async download() {
    if (this.#isBuildMode) {
      return this.#buildFromSource()
    }
    // Already downloaded in check() via docker pull
    return true
  }

  async #buildFromSource() {
    log('Starting Build from Source (Beta/Dev)...')

    try {
      // 1. Prepare Source Directory
      const repo = 'https://github.com/odac-run/odac.git'
      const branch = this.#targetBranch

      if (fs.existsSync(this.#downloadPath)) {
        log('Removing previous download...')
        fs.rmSync(this.#downloadPath, {recursive: true, force: true})
      }

      // 2. Clone Repository via Sidecar
      log('Cloning repository via Docker Sidecar...')
      // Sidecar mounts the host bind of #basePath to '/git_target'
      // It clones into '/git_target/tmp/odac_source' which maps to this.#downloadPath in our container
      await this.#gitCloneWithDocker(repo, branch, this.#downloadPath)

      // Verify clone
      if (!fs.existsSync(path.join(this.#downloadPath, 'package.json'))) {
        throw new Error('Clone failed: package.json not found')
      }

      // 3. Build Docker Image
      log('Building Docker Image...')
      // Run docker build from within our container using the mapped socket
      // The context will be sent from our container to the daemon
      await execAsync(`cd ${this.#downloadPath} && docker build -t ${this.#image} .`)

      // 4. Cleanup
      log('Cleaning up source files...')
      fs.rmSync(this.#downloadPath, {recursive: true, force: true})

      log('Build complete.')
      return true
    } catch (e) {
      throw new Error(`Build failed: ${e.message}`)
    }
  }

  async #gitCloneWithDocker(repo, branch, targetDir) {
    log('Using Docker Sidecar for git operations using target: %s', targetDir)

    // Determine host bind mount for our base path to share with sidecar
    const hostBind = await this.#resolveHostBind(this.#basePath)
    if (!hostBind) {
      throw new Error('Could not determine host path for storage volume. Cannot run git sidecar.')
    }

    // Sidecar mounts the resolved host path to '/git_target'
    // It clones into '/git_target/tmp/odac_source'
    const options = {
      Image: 'alpine/git',
      Cmd: ['clone', '-b', branch, '--depth', '1', repo, '/git_target/tmp/odac_source'],
      HostConfig: {
        Binds: [`${hostBind}:/git_target`]
      }
    }

    // Try to pull image first
    try {
      await new Promise(resolve => {
        this.#docker.pull('alpine/git', (err, stream) => {
          if (err) return resolve() // Ignore pull error (maybe offline/cached)
          this.#docker.modem.followProgress(stream, resolve)
        })
      })
    } catch {
      /* ignore */
    }

    const container = await this.#docker.createContainer(options)
    await container.start()
    const streamLog = await container.logs({follow: true, stdout: true, stderr: true})
    streamLog.pipe(process.stdout) // Pipe logs to our stdout

    const data = await container.wait()
    await container.remove()

    if (data.StatusCode !== 0) {
      throw new Error(`Git clone failed with exit code ${data.StatusCode}`)
    }
    log('Git clone via Docker Sidecar successful.')
  }

  /**
   * Orchestrates the update execution process using Zero-Downtime strategy on Linux.
   * Manages container lifecycle, socket handover, and rollback on failure.
   */
  async execute() {
    log('Launching update process...')

    // Clean up previous update logs to avoid confusion
    // Clean up previous update logs to avoid confusion
    try {
      const logDir = path.join(os.homedir(), '.odac', 'logs')
      const files = [`.${UPDATE_CONTAINER_NAME}.log`, `.${UPDATE_CONTAINER_NAME}_err.log`]

      for (const f of files) {
        const p = path.join(logDir, f)
        if (fs.existsSync(p)) {
          fs.unlinkSync(p)
          log('Removed old log file: %s', f)
        }
      }
    } catch (e) {
      log('Warning: Could not clean old logs: %s', e.message)
    }

    try {
      // 1. Get current container info
      const containerId = (await this.#resolveSelfName()) || CONTAINER_NAME
      const container = this.#docker.getContainer(containerId)
      const info = await container.inspect()

      log('Current container found: %s (%s)', info.Name, containerId)

      // 2. Prepare configuration for the new container
      const newName = UPDATE_CONTAINER_NAME
      // Clean up previous update attempt if exists
      try {
        const oldUpdater = this.#docker.getContainer(newName)
        await oldUpdater.remove({force: true})
      } catch {
        /* Ignore */
      }

      const env = info.Config.Env || []
      const binds = info.HostConfig.Binds || []

      // Setup Create Options
      const createOptions = {
        name: newName,
        Image: this.#image,
        Env: env.filter(e => !UPDATE_ENV_KEYS.some(k => e.startsWith(`${k}=`))),
        HostConfig: {
          Binds: binds,
          Privileged: true,
          CapAdd: ['NET_ADMIN', 'NET_BIND_SERVICE'],
          RestartPolicy: {Name: 'unless-stopped'} // Default policy for production
        },
        Tty: true
      }

      // Platform Specific Configuration
      if (process.platform === 'linux') {
        // Linux: Zero Downtime Update Strategy via Socket Handover
        log('Platform: Linux. Using Zero Downtime Update Strategy.')

        createOptions.Env.push('ODAC_UPDATE_MODE=true')
        createOptions.Env.push('ODAC_INSTANCE_ID=' + require('crypto').randomUUID())
        const currentInstanceId = process.env.ODAC_INSTANCE_ID || 'default'
        createOptions.Env.push(`ODAC_PREVIOUS_INSTANCE_ID=${currentInstanceId}`)
        createOptions.Env.push(`ODAC_PREVIOUS_CONTAINER_NAME=${containerId}`)
        createOptions.Env.push(`ODAC_UPDATE_SOCKET_PATH=${this.#socketPath}`)
        // Use separate log file for update process
        createOptions.Env.push(`ODAC_LOG_NAME=.${newName}`)

        createOptions.HostConfig.NetworkMode = 'host'
        createOptions.HostConfig.PidMode = 'host'
        createOptions.HostConfig.RestartPolicy = {Name: 'no'}

        // Initialize Listener FIRST to avoid race condition with fast containers
        // Destructure to get the pending promise without awaiting it (Deadlock fix)
        const {completion: completionPromise} = await this.#createUpdateListener()

        log('Creating new container: %s', newName)
        const newContainer = await this.#docker.createContainer(createOptions)

        log('Starting new container...')
        await newContainer.start()

        // Stream logs from the new container for better observability
        this.#streamLogs(newContainer, newName).catch(e => {
          log('Warning: Failed to attach logs for %s: %s', newName, e.message)
        })

        log('Update container started successfully. Waiting for handover...')
        try {
          await completionPromise
        } catch (e) {
          log('Handover failed: %s. Rolling back...', e.message)
          await newContainer.stop().catch(() => {})
          await newContainer.remove().catch(() => {})
          throw e
        }
      } else {
        // Windows/Mac: Container Swap Strategy via Helper Container
        log(`Platform: ${process.platform}. Using Container Swap Strategy.`)

        if (info.HostConfig.PortBindings) {
          createOptions.HostConfig.PortBindings = info.HostConfig.PortBindings
        }

        log('Creating new container (STOPPED): %s', newName)
        await this.#docker.createContainer(createOptions)

        // Spawn Runner Container to perform the swap
        log('Spawning runner container to perform swap...')

        // Command: Wait 5s, Stop Old, Remove Old, Rename New, Start New
        const cmd = `sleep 5 && docker stop ${containerId} && docker rm ${containerId} && docker rename ${UPDATE_CONTAINER_NAME} ${CONTAINER_NAME} && docker start ${CONTAINER_NAME}`

        const runnerOptions = {
          Image: RUNNER_IMAGE, // Lightweight docker client
          HostConfig: {
            Binds: ['/var/run/docker.sock:/var/run/docker.sock'],
            AutoRemove: true // Remove runner after execution
          },
          Cmd: ['sh', '-c', cmd]
        }

        // Pull docker:cli just in case
        await execAsync(`docker pull ${RUNNER_IMAGE}`)

        const runner = await this.#docker.createContainer(runnerOptions)
        await runner.start()

        log('Runner spawned. Handing over control and exiting...')
        process.exit(0)
      }
    } catch (e) {
      throw new Error(`Failed to execute update: ${e.message}`)
    }
  }

  /**
   * Creates the update listener socket for the handshake protocol.
   * Returns a promise that resolves when the socket is listening, fulfilling the race condition fix.
   * @returns {Promise<{completion: Promise<boolean>}>}
   */
  async #createUpdateListener() {
    const socketPath = this.#socketPath
    const socketDir = path.dirname(socketPath)

    if (!fs.existsSync(socketDir)) fs.mkdirSync(socketDir, {recursive: true})
    if (fs.existsSync(socketPath)) fs.unlinkSync(socketPath)

    let resolveCompletion, rejectCompletion
    const completionPromise = new Promise((resolve, reject) => {
      resolveCompletion = resolve
      rejectCompletion = reject
    })

    // Extended timeout for stability check (e.g. 5 minutes total)
    const globalTimeout = setTimeout(() => {
      if (server) server.close()
      rejectCompletion(new Error('Update process timed out globally'))
    }, 300000)

    let handoverCompleted = false
    let servicesRestarted = false

    const server = net.createServer(socket => {
      log('New container connected. Starting handover...')

      // Monitoring: If socket closes before handover completion, Rollback!
      socket.on('close', async () => {
        if (handoverCompleted) return

        log('CRITICAL: New container disconnected prematurely! Initiating ROLLBACK...')

        // Tear this listener down before anything else. The rollback restarts our
        // services through System.init(), which re-enters Updater.init(); if the
        // socket were still listening, init() would find it, handshake with *this*
        // process and drive takeOver() inside the old container — renaming us to
        // 'odac-backup' and leaving no container named 'odac' at all.
        clearTimeout(globalTimeout)
        try {
          server.close()
        } catch {
          /* Ignore */
        }
        try {
          fs.unlinkSync(socketPath)
        } catch {
          /* Ignore */
        }

        try {
          await this.#fetchContainerLogs(UPDATE_CONTAINER_NAME)

          // Establish who we are before removing anything: a rollback must never
          // force-remove the container it is running inside. Before takeOver we
          // still own 'odac' and the failed container is 'odac-update'; after
          // takeOver we are 'odac-backup' and the failed one has claimed 'odac'.
          const selfName = await this.#resolveSelfName()
          const failedName = selfName === BACKUP_CONTAINER_NAME ? CONTAINER_NAME : UPDATE_CONTAINER_NAME

          // Restart services immediately
          if (!servicesRestarted) {
            log('Restarting services for rollback...')
            servicesRestarted = true
            await Odac.server('System').init() // Restart all services
            log('Services restarted successfully')
          }

          // Clean up the failed container
          try {
            await this.#docker.getContainer(failedName).remove({force: true})
            log('Failed new container removed: %s', failedName)
          } catch (e) {
            if (e.statusCode !== 404) log('Warning: could not remove %s: %s', failedName, e.message)
          }

          // Restore our name, if takeOver already renamed us away from it
          try {
            await this.#docker.getContainer(BACKUP_CONTAINER_NAME).rename({name: CONTAINER_NAME})
            log('Restored self to "%s".', CONTAINER_NAME)
          } catch (e) {
            if (e.statusCode !== 404) error('Failed to restore name: %s', e.message)
          }

          // takeOver() revoked our restart policy on the way out; take it back.
          await this.#ensureRestartPolicy()
          log('Rollback successful. Continuing operations.')
        } catch (err) {
          error('Rollback failed: %s', err.message)
        }

        // Unblock execute(), which is still awaiting the completion promise.
        // Without this it hangs until the 5-minute global timeout, and #updating
        // stays true — blocking every later update attempt.
        rejectCompletion(new Error('Rolled back: new container disconnected prematurely'))
      })

      socket.on('data', async data => {
        const message = data.toString().trim()
        log('Received: %s', message)

        // --- ZERO-DOWNTIME HANDSHAKE PROTOCOL ---
        // Phase 1 (HANDSHAKE_READY): Old instance releases non-critical ports (Mail, DNS, Api).
        //                            Web stays UP to serve requests during overlap (SO_REUSEPORT).
        // Phase 2 (HANDSHAKE_ACK):   Old instance signals New instance to bind ports.
        // Phase 3 (WEB_READY):       New instance confirms Web is active. Old instance stops Web.
        // Phase 4 (TAKEOVER_COMPLETE): Final stability confirmed. Old instance self-destructs.

        if (message === 'HANDSHAKE_READY') {
          log('New container ready. Stopping services (except Web & DNS) and releasing ports...')

          // Stop all services EXCEPT Web and DNS (Mail, Api, Hub)
          // Web and DNS continue running due to SO_REUSEPORT - new container will overlap
          try {
            Odac.server('System').stop(true) // exceptWeb = true (also keeps DNS alive)
            log('Non-overlap services stopped. Ports released. Web & DNS still running via SO_REUSEPORT.')
          } catch (e) {
            error('Failed to stop services: %s', e.message)
          }

          // Send ACK - NEW can now start ALL services (including Web & DNS with SO_REUSEPORT overlap)
          socket.write('HANDSHAKE_ACK')
          log('ACK sent. New container can now bind ports (Web & DNS will overlap via SO_REUSEPORT).')
        } else if (message === 'WEB_READY') {
          log("New container's Web & DNS are stable. Stopping old overlap services now...")

          // Stop Web and DNS services immediately - NEW's services are proven to work
          try {
            Odac.server('Proxy').stop()
            Odac.server('DNS').stop()
            log('Old Web & DNS stopped. Zero-downtime handover complete (overlap: ~3s).')
          } catch (e) {
            error('Failed to stop overlap services: %s', e.message)
          }
        } else if (message === 'TAKEOVER_COMPLETE') {
          log('Stability check passed. New instance is stable.')
          handoverCompleted = true

          try {
            // Services already stopped, just cleanup and exit
            socket.write('HANDOVER_COMPLETE')
            resolveCompletion(true)
          } catch (e) {
            socket.write(`HANDOVER_FAILED:${e.message}`)
            rejectCompletion(e)
          } finally {
            clearTimeout(globalTimeout)
            setTimeout(() => {
              socket.end()
              server.close()
              this.#selfDestruct()
            }, 1000)
          }
        }
      })
    })

    server.on('error', e => {
      clearTimeout(globalTimeout)
      rejectCompletion(e)
    })

    return new Promise((resolveListening, rejectListening) => {
      server.listen(socketPath, () => {
        fs.chmodSync(socketPath, 0o666)
        log('Listening on update socket: %s', socketPath)
        // Return object to prevent await from unwrapping the inner promise immediately (Deadlock fix)
        resolveListening({completion: completionPromise})
      })
      server.on('error', err => rejectListening(err))
    })
  }

  /**
   * Best-effort resolution of the name of the container we are running inside.
   *
   * The usual "hostname is the container id" trick is useless here: the update
   * container runs with NetworkMode 'host', so its hostname is the host's. We
   * instead read the id out of the paths Docker injects into our mount table and
   * cgroup, and fall back to "whichever update-cycle container is running".
   *
   * @returns {Promise<string|null>} Container name without the leading slash.
   */
  async #resolveSelfName() {
    const hostName = os.hostname()
    if (/^[0-9a-f]{12}$/.test(hostName)) {
      try {
        const info = await this.#docker.getContainer(hostName).inspect()
        return info.Name.replace(/^\//, '')
      } catch {
        /* Not this one */
      }
    }

    const probes = [
      ['/proc/self/mountinfo', /\/docker\/containers\/([0-9a-f]{64})/],
      ['/proc/self/cgroup', /[-/]docker[-/]([0-9a-f]{64})/]
    ]

    for (const [file, pattern] of probes) {
      try {
        const match = fs.readFileSync(file, 'utf8').match(pattern)
        if (!match) continue
        const info = await this.#docker.getContainer(match[1]).inspect()
        return info.Name.replace(/^\//, '')
      } catch {
        /* Probe unavailable on this kernel/storage driver; try the next */
      }
    }

    // Fallback: the live ODAC process necessarily inhabits one of the
    // update-cycle containers, and only one of them is running — us.
    for (const name of [CONTAINER_NAME, BACKUP_CONTAINER_NAME, UPDATE_CONTAINER_NAME]) {
      try {
        const info = await this.#docker.getContainer(name).inspect()
        if (info.State?.Running) return name
      } catch {
        /* Not this one */
      }
    }

    return null
  }

  /**
   * Reclaims the 'odac' container name when a crashed or rolled-back update left
   * it unowned.
   *
   * execute() resolves the current container strictly by name, so a host stranded
   * under the name 'odac-backup' can never self-update again — every attempt dies
   * on a 404 before it does anything. Reclaiming the name is what makes such a
   * host recoverable without an operator running `docker rename` by hand.
   */
  async #ensureIdentity() {
    if (!fs.existsSync('/.dockerenv')) return

    try {
      await this.#docker.getContainer(CONTAINER_NAME).inspect()
      return // The name is owned; nothing to reclaim.
    } catch (e) {
      if (e.statusCode !== 404) {
        error('Failed to inspect %s: %s', CONTAINER_NAME, e.message)
        return
      }
    }

    const selfName = await this.#resolveSelfName()
    if (!selfName || selfName === CONTAINER_NAME) {
      error('No container owns "%s" and self-identification failed. Rename manually.', CONTAINER_NAME)
      return
    }

    try {
      await this.#docker.getContainer(selfName).rename({name: CONTAINER_NAME})
      log('Identity reclaimed: "%s" renamed to "%s".', selfName, CONTAINER_NAME)
    } catch (e) {
      error('Failed to reclaim identity from "%s": %s', selfName, e.message)
    }
  }

  /**
   * Ensures the live 'odac' container carries a persistent restart policy.
   *
   * The zero-downtime path deliberately creates the incoming container with
   * RestartPolicy 'no' so Docker cannot resurrect it if the handover fails.
   * Once that container renames itself to 'odac' it becomes the production
   * instance and must adopt the persistent policy — `docker rename` moves the
   * name, not the HostConfig. Without this the host reboots and 'odac' stays
   * down while every other container comes back up.
   *
   * Only a missing/'no' policy is repaired, so an operator who deliberately
   * chose 'always' keeps their choice.
   */
  async #ensureRestartPolicy() {
    // Guard: outside a container there is no 'odac' container that is us.
    if (!fs.existsSync('/.dockerenv')) return

    try {
      const container = this.#docker.getContainer(CONTAINER_NAME)
      const info = await container.inspect()
      const current = info.HostConfig?.RestartPolicy?.Name || 'no'

      if (current !== 'no') return

      await container.update({RestartPolicy: {Name: 'unless-stopped'}})
      log('Restart policy repaired: "no" -> "unless-stopped"')
    } catch (e) {
      if (e.statusCode === 404) return
      error('Failed to ensure restart policy: %s', e.message)
    }
  }

  async #selfDestruct() {
    log('Old container mission complete. Stopping overlap services and exiting.')

    // Stop Web and DNS services now (were kept running during handover for zero-downtime)
    try {
      Odac.server('Proxy').stop()
      Odac.server('DNS').stop()
      log('Web & DNS services stopped.')
    } catch (e) {
      error('Failed to stop overlap services: %s', e.message)
    }

    // Disable restart policy to prevent Docker from restarting this container
    try {
      const container = this.#docker.getContainer(BACKUP_CONTAINER_NAME)
      await container.update({RestartPolicy: {Name: 'no'}})
      log('Restart policy disabled.')
    } catch (e) {
      error('Failed to disable restart policy: %s', e.message)
    }

    process.exit(0)
  }

  async #takeOver() {
    log('Taking over container identity...')
    const targetName = CONTAINER_NAME
    const backupName = BACKUP_CONTAINER_NAME
    const previousName = process.env.ODAC_PREVIOUS_CONTAINER_NAME || targetName

    // 1. Remove existing backup if any
    try {
      if (previousName !== backupName) {
        const backup = this.#docker.getContainer(backupName)
        await backup.remove({force: true})
        log('Previous backup removed.')
      } else {
        try {
          const leftover = this.#docker.getContainer(targetName)
          await leftover.remove({force: true})
          log('Leftover target container removed.')
        } catch (err) {
          if (err.statusCode !== 404) {
            log('Warning: Could not remove leftover target container: %s', err.message)
          }
        }
      }
    } catch (e) {
      if (e.statusCode !== 404) {
        log('Warning cleaning backup: %s', e.message)
      }
    }

    // 2. Rename old container to backup
    try {
      if (previousName !== backupName) {
        const oldContainer = this.#docker.getContainer(previousName)
        await oldContainer.rename({name: backupName})
        log('Old container renamed to backup.')
      } else {
        log('Old container is already named backup.')
      }
    } catch (e) {
      // Ignore 404 if old container doesn't exist
      if (e.statusCode !== 404) {
        // If rename fails, try to remove it to clear the name
        log('Warning: Could not rename old container to backup: %s. Attempting force remove.', e.message)
        try {
          const oldContainer = this.#docker.getContainer(previousName)
          await oldContainer.remove({force: true})
        } catch (err) {
          log('Critical: Failed to remove old container: %s', err.message)
        }
      }
    }

    // 3. Revoke the backup's restart policy *before* granting our own.
    //
    // Invariant: at most one container may carry a persistent restart policy at
    // any instant. Proxy/DNS/Mail bind with SO_REUSEPORT, so two ODAC instances
    // resurrected by Docker after a host reboot would not collide on ports —
    // they would silently split traffic, run two Hub connections and two ACME
    // clients. selfDestruct() also revokes this, but it only runs ~12s later
    // after the stability check; a hard reboot inside that window must not find
    // two auto-starting containers.
    //
    // The cost is a millisecond-wide gap where no container auto-starts. That
    // is recoverable with `docker start odac` (init() then repairs the policy);
    // a split brain is not.
    try {
      const backup = this.#docker.getContainer(backupName)
      await backup.update({RestartPolicy: {Name: 'no'}})
      log('Backup restart policy revoked.')
    } catch (e) {
      if (e.statusCode !== 404) error('Failed to revoke backup restart policy: %s', e.message)
    }

    // 4. Rename self
    try {
      const me = this.#docker.getContainer(UPDATE_CONTAINER_NAME)
      await me.rename({name: targetName})
      log('Renamed self to %s', targetName)

      // 5. Adopt the persistent restart policy. Must happen *after* the rename:
      // until we own the 'odac' name a failed handover should stay dead rather
      // than be restarted by Docker as a half-migrated instance.
      await this.#ensureRestartPolicy()
    } catch (e) {
      error('Failed to rename self: %s', e.message)
    }
  }

  async #performHandshake(socketPath) {
    return new Promise((resolve, reject) => {
      const socket = net.createConnection(socketPath)

      // Timeout just for the initial connection and handshake
      // The stability wait is handled inside logic
      const timeout = setTimeout(() => {
        socket.destroy()
        reject(new Error('Handshake timeout'))
      }, 60000)

      socket.on('connect', () => {
        socket.write('HANDSHAKE_READY')
      })

      socket.on('data', async data => {
        const message = data.toString().trim()

        if (message === 'HANDSHAKE_ACK') {
          log('Ack received. Taking over & Starting stability timer (15s)...')

          try {
            // 1. Take Over Name
            await this.#takeOver()

            // 2. Start Services (Trigger Ready)
            this.#triggerReady()
            log('Services started. Waiting for Proxy & DNS readiness...')

            // 3. Wait for Proxy and DNS to be fully ready (config synced)
            const [proxyReady, dnsReady] = await Promise.all([
              Odac.server('Proxy').waitForReady(10000),
              Odac.server('DNS').waitForReady(10000)
            ])

            if (!proxyReady || !dnsReady) {
              log('WARNING: Readiness check incomplete (Proxy: %s, DNS: %s). Proceeding anyway.', proxyReady, dnsReady)
            } else {
              log('Proxy & DNS confirmed ready with config synced.')
            }

            log('Signaling old container to stop Web...')
            socket.write('WEB_READY')

            // 4. Wait 12 Seconds - General System Stability Check
            setTimeout(() => {
              log('General stability check passed (15s total). Signaling completion...')
              socket.write('TAKEOVER_COMPLETE')
            }, 12000)
          } catch (e) {
            error('Startup failed: %s', e.message)
            socket.destroy()
            reject(e)
          }
        } else if (message === 'HANDOVER_COMPLETE') {
          clearTimeout(timeout)
          log('Update completed successfully.')

          // We are the live instance now, not an updater. The container keeps
          // ODAC_UPDATE_MODE=true in its env for the rest of its life (env cannot
          // be rewritten on a running container), and Proxy/DNS/Mail read it on
          // every start() to decide whether to adopt an existing process. Clear it
          // in-process so a later restart adopts instead of blindly respawning.
          delete process.env.ODAC_UPDATE_MODE

          // Signal Watchdog to switch to standard logs
          console.log('ODAC_CMD:SWITCH_LOGS')
          socket.end()
          try {
            fs.unlinkSync(socketPath)
          } catch {
            /* Ignore */
          }
          resolve(true)
        } else if (message.startsWith('HANDOVER_FAILED')) {
          socket.end()
          reject(new Error(message))
        }
      })

      socket.on('error', err => {
        // If we crash or disconnect, the old instance handles rollback
        reject(err)
      })
    })
  }

  /**
   * Resolves the host-side path for a given container mount point by inspecting current container binds.
   * Required for sidecar containers that need to mount the same shared volume.
   * @param {string} containerPath - The mount path inside the current container (e.g. /app/.odac)
   * @returns {Promise<string|null>} The host path, or null if not found.
   */
  async #resolveHostBind(containerPath) {
    try {
      const container = this.#docker.getContainer(CONTAINER_NAME)
      const info = await container.inspect()
      const binds = info.HostConfig.Binds || []

      for (const bind of binds) {
        const parts = bind.split(':')
        if (parts.length >= 2 && parts[1] === containerPath) {
          return parts[0]
        }
      }

      // Fallback: check Mounts for named volumes
      const mounts = info.Mounts || []
      for (const mount of mounts) {
        if (mount.Destination === containerPath) {
          return mount.Name || mount.Source
        }
      }

      log('No host bind found for container path: %s', containerPath)
      return null
    } catch (e) {
      log('Could not resolve host bind for %s: %s', containerPath, e.message)
      return null
    }
  }

  async #getLocalImageId() {
    try {
      const selfName = (await this.#resolveSelfName()) || CONTAINER_NAME
      const info = await this.#docker.getContainer(selfName).inspect()
      return info.Image
    } catch (e) {
      log('Could not get local image ID: %s', e.message)
      return null
    }
  }

  async #fetchContainerLogs(name) {
    const tempFile = `/tmp/${name}-crash.log`

    try {
      // 1. Try to copy internal log file
      // We use docker cp because the log file is inside the container's filesystem
      // and might contain details not printed to stdout
      const logFileName = name === UPDATE_CONTAINER_NAME ? `.${name}.log` : '.odac.log'

      await execAsync(`docker cp ${name}:${this.#basePath}/logs/${logFileName} ${tempFile}`)

      if (fs.existsSync(tempFile)) {
        const content = fs.readFileSync(tempFile, 'utf8')
        // Get last 100 lines
        const lines = content.split('\n') // This might be memory heavy if file is huge, but usually fine for logs
        const lastLines = lines.slice(-100).join('\n')

        log(`--- INTERNAL LOGS FOR ${name} ---`)
        log(lastLines)
        log('-----------------------------------')

        try {
          fs.unlinkSync(tempFile)
        } catch {
          /* Ignore */
        }
      }
    } catch (e) {
      log('Could not fetch internal logs via cp: %s. Trying standard logs...', e.message)

      // 2. Fallback to standard docker logs
      try {
        const container = this.#docker.getContainer(name)
        const buffer = await container.logs({stdout: true, stderr: true, tail: 50})
        log(`--- DOCKER STD LOGS (FALLBACK) FOR ${name} ---`)
        log(buffer.toString('utf8'))
        log('-----------------------------------')
      } catch (err) {
        log('Could not fetch docker logs: %s', err.message)
      }
    }
  }

  async #getRemoteImageId() {
    try {
      const info = await this.#docker.getImage(this.#image).inspect()
      return info.Id
    } catch (e) {
      log('Could not get remote image ID: %s', e.message)
      return null
    }
  }

  async #streamLogs(container, name) {
    try {
      const stream = await container.logs({
        follow: true,
        stdout: true,
        stderr: true
      })

      const createLogStream = (isError = false) => {
        const decoder = new StringDecoder('utf8')
        let buffer = ''

        return new Writable({
          write(chunk, encoding, next) {
            buffer += decoder.write(chunk)
            const lines = buffer.split('\n')
            buffer = lines.pop() // Keep the last partial line in the buffer

            for (const line of lines) {
              if (line.trim()) {
                const prefix = isError ? '[NEW_VERSION:ERR]' : '[NEW_VERSION]'
                log(`${prefix} ${line}`)
              }
            }
            next()
          }
        })
      }

      // Demultiplex stdout and stderr
      container.modem.demuxStream(stream, createLogStream(false), createLogStream(true))
    } catch (e) {
      log('Failed to attach log stream for %s: %s', name, e.message)
    }
  }
}

module.exports = new Updater()
