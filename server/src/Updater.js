const Docker = require('dockerode')
const {exec} = require('child_process')
const util = require('util')
const execAsync = util.promisify(exec)
const {log, error} = Odac.core('Log', false).init('Updater')

class Updater {
  #updating = false
  #isUpdateMode = false
  #image = 'odacrun/odac:latest'
  #docker = new Docker({socketPath: '/var/run/docker.sock'})
  #readyCallbacks = []
  #isReady = false

  /**
   * Initialize Updater.
   * Checks if an update socket exists, meaning we are the new instance in an update process.
   */
  async init() {
    const fs = require('fs')
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
  async start(command, sendResponse) {
    if (this.#updating) {
      log('Update request blocked: Update already in progress')
      if (sendResponse) sendResponse({success: false, message: 'Update already in progress'})
      return
    }

    this.#updating = true
    log('Update process started via Hub command')
    if (sendResponse) sendResponse({success: true, message: 'Update process started'})

    try {
      const available = await this.#checkForUpdates()
      if (!available) {
        log('System is up to date.')
        this.#updating = false
        return
      }

      await this.download()
      await this.execute()
    } catch (e) {
      this.#updating = false
      error('Update process failed: %s', e.message)
    }
  }

  /**
   * Check if we are running as an updater instance.
   */
  async check() {
    return this.#isUpdateMode
  }

  async #checkForUpdates() {
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
    // Already downloaded in check() via docker pull
    return true
  }

  async execute() {
    log('Launching update container...')

    try {
      // 1. Get current container info
      // Hostname inside container is usually the Container ID (short version)
      const containerId = process.env.HOSTNAME
      const container = this.#docker.getContainer(containerId)
      const info = await container.inspect()

      log('Current container found: %s (%s)', info.Name, containerId)

      // 2. Prepare configuration for the new container
      const newName = 'odac-update'

      // Clean up previous update attempt if exists
      try {
        const oldUpdater = this.#docker.getContainer(newName)
        await oldUpdater.remove({force: true})
      } catch {
        // Ignore if not exists
      }

      // We copy essential configurations: Binds, NetworkMode, Env
      const env = info.Config.Env || []

      // Ensure /var/run/odac is mounted or available
      // Since we use the same Binds as the old container, and the old container MUST have access to it
      // (because we created the socket there), we assume it's either a volume or inside the container's writable layer.
      // IF it's just a path inside container, the NEW container won't see the socket unless we mount a shared volume.
      // CRITICAL: We need a shared volume for the socket!

      const binds = info.HostConfig.Binds || []
      // Check if we have a bind for /var/run/odac, if not, we must add one dynamically?
      // Actually, we can just use the Host system's temporary directory if we are in Host Network mode...
      // BUT, we are in a container.
      // So, let's assume standard ODAC deployment has a volume for this OR we use /app/storage/run if that's shared.

      // Better approach: Use the existing Docker Socket mount trace or just rely on 'odac-storage' volume.
      // Let's assume the socket path '/var/run/odac/update.sock' is on a shared volume.
      // If not, we should use a path we KNOW is shared, like '/app/storage/update.sock'.

      // Let's change the socket path to reside in /app/storage/run/ which is definitely persisted/shared
      // if volumes are set up correctly.

      // Add special flag for the new instance
      env.push('ODAC_UPDATE_MODE=true')
      env.push('ODAC_UPDATE_SOCKET_PATH=/app/storage/run/update.sock')

      const createOptions = {
        name: newName,
        Image: this.#image,
        Env: env,
        HostConfig: {
          Binds: binds,
          NetworkMode: 'host', // Critical for SO_REUSEPORT
          Privileged: true, // Required for Docker management
          PidMode: 'host',
          RestartPolicy: {Name: 'no'} // Monitor it manually first
        }
      }

      log('Creating new container: %s', newName)
      const newContainer = await this.#docker.createContainer(createOptions)

      log('Starting new container...')
      await newContainer.start()

      log('Update container started successfully. Waiting for handover...')

      // 4. Start listening for the handshake from the new container
      try {
        await this.#createUpdateListener()
      } catch (e) {
        log('Handover failed: %s. Rolling back...', e.message)
        await newContainer.stop().catch(() => {})
        await newContainer.remove().catch(() => {})
        throw e
      }

      // The rest of the logic (handover) is handled by the socket communication
      // initiated by the NEW container in its startup phase.
    } catch (e) {
      throw new Error(`Failed to execute update: ${e.message}`)
    }
  }

  async #createUpdateListener() {
    const net = require('net')
    const fs = require('fs')
    const socketPath = '/app/storage/run/update.sock'
    const socketDir = '/app/storage/run'

    if (!fs.existsSync(socketDir)) fs.mkdirSync(socketDir, {recursive: true})

    // Remove existing socket if any
    if (fs.existsSync(socketPath)) fs.unlinkSync(socketPath)

    return new Promise((resolve, reject) => {
      // Timeout for the entire handover process (e.g., 2 minutes)
      const timeout = setTimeout(() => {
        server.close()
        reject(new Error('Update handover timed out'))
      }, 120000)

      const server = net.createServer(socket => {
        log('New container connected to update socket')

        socket.on('data', async data => {
          const message = data.toString().trim()
          log('Received message from update socket: %s', message)

          if (message === 'HANDSHAKE_READY') {
            log('Handshake successful! New container is ready.')

            // Critical Point: The core switch logic
            try {
              await this.#performHandover()
              socket.write('HANDOVER_COMPLETE')
              resolve(true)
            } catch (e) {
              socket.write(`HANDOVER_FAILED:${e.message}`)
              reject(e)
            } finally {
              clearTimeout(timeout)
              // Give some time for the message to be sent before closing
              setTimeout(() => {
                socket.end()
                server.close()
                // Self-destruct sequence for the old container
                this.#selfDestruct()
              }, 1000)
            }
          }
        })
      })

      server.listen(socketPath, () => {
        fs.chmodSync(socketPath, 0o666)
        log('Listening on update socket: %s', socketPath)
      })

      server.on('error', e => {
        clearTimeout(timeout)
        reject(e)
      })
    })
  }

  async #performHandover() {
    log('Performing handover...')
    // Rename current container (backup)
    // Rename new container (primary)
    // Since we are inside the container, we can't easily rename ourselves via Docker API
    // if we don't know our own true ID or name perfectly.
    // And renaming might be blocked if container is running.

    // Instead of renaming, we rely on the fact that the NEW container
    // has already started binding ports (SO_REUSEPORT) or will take over.
    // The most important thing here is to STOP our services.

    log('Stopping internal services...')
    Odac.server('Server').stop() // Stops App, Web, Mail, etc.

    // Note: process.exit() will be called in #selfDestruct
  }

  async #selfDestruct() {
    log('Old container mission complete. Disabling restart policy and exiting.')

    // Disable restart policy to prevent Docker from restarting this container
    try {
      if (process.env.HOSTNAME) {
        const container = this.#docker.getContainer(process.env.HOSTNAME)
        await container.update({RestartPolicy: {Name: 'no'}})
        log('Restart policy disabled.')
      }
    } catch (e) {
      error('Failed to disable restart policy: %s', e.message)
    }

    process.exit(0)
  }

  async #performHandshake(socketPath) {
    const net = require('net')

    return new Promise((resolve, reject) => {
      const socket = net.createConnection(socketPath)

      const timeout = setTimeout(() => {
        socket.destroy()
        reject(new Error('Handshake timeout'))
      }, 5000)

      socket.on('connect', () => {
        log('Connected to update socket. Sending READY signal...')
        socket.write('HANDSHAKE_READY')
      })

      socket.on('data', data => {
        const message = data.toString().trim()
        log('Received handshake response: %s', message)

        if (message === 'HANDOVER_COMPLETE') {
          clearTimeout(timeout)
          log('Handover completed successfully. We are now the primary instance.')
          socket.end()
          // We are now in charge. The old process should have exited or is about to.
          // Clean up the socket file to ensure clean state for next restart/update
          try {
            require('fs').unlinkSync(socketPath)
          } catch {
            // Ignore
          }
          this.#triggerReady()
          resolve(true)
        } else if (message.startsWith('HANDOVER_FAILED')) {
          clearTimeout(timeout)
          socket.end()
          reject(new Error(message))
        }
      })

      socket.on('error', err => {
        clearTimeout(timeout)
        reject(err)
      })
    })
  }

  async #getLocalImageId() {
    try {
      // Find the ID of the currently running 'odac' container
      // This assumes the container name is 'odac'
      const {stdout} = await execAsync("docker inspect --format='{{.Image}}' odac")
      return stdout.trim()
    } catch (e) {
      log('Could not get local image ID: %s', e.message)
      return null
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
}

module.exports = new Updater()
