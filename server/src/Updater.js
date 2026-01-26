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
   * Initialize Updater.
   * Checks if an update socket exists, meaning we are the new instance in an update process.
   */
  async init() {
    const socketPath = '/app/storage/run/update.sock'

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
    const available = await this.#checkForUpdates()
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
      // 1. Ensure Git is available
      await this.#ensureGit()

      // 2. Prepare Source Directory
      const sourceDir = '/tmp/odac_source'
      if (fs.existsSync(sourceDir)) {
        await execAsync(`rm -rf ${sourceDir}`)
      }

      // 3. Clone Repository
      const branch = this.#targetBranch
      log(`Cloning ${branch} branch...`)
      await execAsync(`git clone -b ${branch} https://github.com/odac-run/odac.git ${sourceDir}`)

      // 4. Build Docker Image
      log('Building Docker Image...')
      // We use the docker CLI for building as it handles context transfer seamlessly
      await execAsync(`cd ${sourceDir} && docker build -t ${this.#image} .`)

      // 5. Cleanup
      await execAsync(`rm -rf ${sourceDir}`)
      log('Build complete.')
      return true
    } catch (e) {
      throw new Error(`Build failed: ${e.message}`)
    }
  }

  async #ensureGit() {
    try {
      await execAsync('git --version')
    } catch {
      log('Git not found. Installing...')
      if (process.platform === 'linux') {
        try {
          // Check for apk (Alpine) - try running it directly
          try {
            await execAsync('apk --version')
            log('Detected Alpine Linux. Installing git via apk...')
            await execAsync('apk add --no-cache git')
            return
          } catch {
            // Not Alpine
          }

          // Check for apt-get (Debian/Ubuntu) - try running it directly
          try {
            // apt-get -v usually returns 0 if installed
            await execAsync('apt-get -v')
            log('Detected Debian/Ubuntu. Installing git via apt-get...')
            // Use DEBIAN_FRONTEND=noninteractive to prevent hanging on prompts
            await execAsync('apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y git')
            return
          } catch {
            // Not Debian
          }

          throw new Error('No supported package manager found (apk, apt-get)')
        } catch (e) {
          throw new Error('Failed to install git: ' + e.message)
        }
      } else {
        throw new Error('Git missing and auto-install not supported on this platform.')
      }
    }
  }

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
      const containerId = CONTAINER_NAME
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
        Env: env.filter(e => !e.startsWith('ODAC_UPDATE_MODE=') && !e.startsWith('ODAC_INSTANCE_ID=')),
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
        createOptions.Env.push('ODAC_UPDATE_SOCKET_PATH=/app/storage/run/update.sock')
        // Use separate log file for update process
        createOptions.Env.push(`ODAC_LOG_NAME=.${newName}`)

        createOptions.HostConfig.NetworkMode = 'host'
        // NOTE: PidMode: 'host' is NOT needed and breaks package managers (apk, apt-get)
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
        const cmd = `sleep 5 && docker stop ${CONTAINER_NAME} && docker rm ${CONTAINER_NAME} && docker rename ${UPDATE_CONTAINER_NAME} ${CONTAINER_NAME} && docker start ${CONTAINER_NAME}`

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

  async #createUpdateListener() {
    const socketPath = '/app/storage/run/update.sock'
    const socketDir = '/app/storage/run'

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

    const server = net.createServer(socket => {
      log('New container connected. Starting stability monitoring...')

      // Monitoring: If socket closes before handover completion, Rollback!
      socket.on('close', async () => {
        if (!handoverCompleted) {
          log('CRITICAL: New container disconnected prematurely! Initiating ROLLBACK...')
          try {
            await this.#fetchContainerLogs(UPDATE_CONTAINER_NAME)

            try {
              const newOne = this.#docker.getContainer(CONTAINER_NAME)
              await newOne.remove({force: true})
              log('Failed new container removed.')
            } catch {
              try {
                const updateOne = this.#docker.getContainer(UPDATE_CONTAINER_NAME)
                await updateOne.remove({force: true})
              } catch {
                /* Ignore */
              }
            }

            const myName = BACKUP_CONTAINER_NAME
            const me = this.#docker.getContainer(myName)
            await me.rename({name: CONTAINER_NAME})
            log('Rollback successful: Restored self to "odac". Continuing operations.')
          } catch (err) {
            error('Rollback failed: %s', err.message)
          }
        }
      })

      socket.on('data', async data => {
        const message = data.toString().trim()
        log('Received: %s', message)

        if (message === 'HANDSHAKE_READY') {
          socket.write('HANDSHAKE_ACK')
        } else if (message === 'TAKEOVER_COMPLETE') {
          log('Stability check passed. New instance is stable.')
          handoverCompleted = true

          try {
            await this.#performHandover()
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

  async #performHandover() {
    log('Performing handover...')
    // Rename current container (backup)
    // Rename new container (primary)
    // Since we are inside the container, we rely on the NEW container taking over
    // while we handle the graceful shutdown of internal services.

    log('Stopping internal services...')
    Odac.server('Server').stop() // Stops App, Web, Mail, etc.

    // Note: process.exit() will be called in #selfDestruct
  }

  async #selfDestruct() {
    log('Old container mission complete. Disabling restart policy and exiting.')

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

    // 1. Remove existing backup if any
    try {
      const backup = this.#docker.getContainer(backupName)
      await backup.remove({force: true})
      log('Previous backup removed.')
    } catch (e) {
      if (e.statusCode !== 404) {
        log('Warning cleaning backup: %s', e.message)
      }
    }

    // 2. Rename old 'odac' to 'odac-backup'
    try {
      const oldContainer = this.#docker.getContainer(targetName)
      await oldContainer.rename({name: backupName})
      log('Old container renamed to backup.')
    } catch (e) {
      // Ignore 404 if 'odac' doesn't exist
      if (e.statusCode !== 404) {
        // If rename fails, try to remove it to clear the name 'odac'
        log('Warning: Could not rename old container to backup: %s. Attempting force remove.', e.message)
        try {
          const oldContainer = this.#docker.getContainer(targetName)
          await oldContainer.remove({force: true})
        } catch (err) {
          log('Critical: Failed to remove old container: %s', err.message)
        }
      }
    }

    // 3. Rename self
    try {
      const me = this.#docker.getContainer(UPDATE_CONTAINER_NAME)
      await me.rename({name: targetName})
      log('Renamed self to %s', targetName)
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
            log('Services started. Waiting 15s for stability...')

            // 3. Wait 15 Seconds
            setTimeout(() => {
              log('Stability check passed (15s). Signaling completion...')
              socket.write('TAKEOVER_COMPLETE')
            }, 15000)
          } catch (e) {
            error('Startup failed: %s', e.message)
            socket.destroy()
            reject(e)
          }
        } else if (message === 'HANDOVER_COMPLETE') {
          clearTimeout(timeout)
          log('Update completed successfully.')
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

  async #getLocalImageId() {
    try {
      // Find the ID of the currently running 'odac' container
      // This assumes the container name is 'odac'
      const {stdout} = await execAsync(`docker inspect --format='{{.Image}}' ${CONTAINER_NAME}`)
      return stdout.trim()
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

      await execAsync(`docker cp ${name}:/app/storage/.odac/logs/${logFileName} ${tempFile}`)

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
      const {stdout} = await execAsync(`docker inspect --format='{{.Id}}' ${this.#image}`)
      return stdout.trim()
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
