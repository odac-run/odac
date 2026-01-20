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
    if (fs.existsSync(socketPath)) fs.unlinkSync(socketPath)

    return new Promise((resolve, reject) => {
      // Extended timeout for stability check (e.g. 5 minutes total)
      const globalTimeout = setTimeout(() => {
        server.close()
        reject(new Error('Update process timed out globally'))
      }, 300000)

      let handoverCompleted = false

      const server = net.createServer(socket => {
        log('New container connected. Starting stability monitoring...')

        // Monitoring: If socket closes before handover completion, Rollback!
        socket.on('close', async () => {
          if (!handoverCompleted) {
            log('CRITICAL: New container disconnected prematurely! Initiating ROLLBACK...')
            try {
              // 1. Remove the failed new container
              try {
                // Better: Iterate containers or just try to remove 'odac' (which is the new one now)
                const newOne = this.#docker.getContainer('odac')
                await newOne.remove({force: true})
                log('Failed new container removed.')
              } catch {
                // Ignore removal errors if already gone
                // It might be named 'odac-update' if rename failed
                try {
                  const updateOne = this.#docker.getContainer('odac-update')
                  await updateOne.remove({force: true})
                } catch {
                  /* Ignore */
                }
              }

              // 2. Restore my name from 'odac-backup' to 'odac'
              // We need to find our own container.
              // Assuming we renamed ourselves to 'odac-backup' during the TakeOver phase initiated by New
              // Wait, TakeOver is done by NEW instance via API.
              // So if NEW died, WE are named 'odac-backup'.

              const myName = 'odac-backup'
              const me = this.#docker.getContainer(myName)
              await me.rename({name: 'odac'})
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
              // Now we stop our services
              await this.#performHandover()
              socket.write('HANDOVER_COMPLETE')
              resolve(true)
            } catch (e) {
              socket.write(`HANDOVER_FAILED:${e.message}`)
              reject(e)
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

      server.listen(socketPath, () => {
        fs.chmodSync(socketPath, 0o666)
        log('Listening on update socket: %s', socketPath)
      })

      server.on('error', e => {
        clearTimeout(globalTimeout)
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

  async #takeOver() {
    log('Taking over container identity...')
    const targetName = 'odac'
    const backupName = 'odac-backup'

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
      if (process.env.HOSTNAME) {
        const me = this.#docker.getContainer(process.env.HOSTNAME)
        await me.rename({name: targetName})
        log('Renamed self to %s', targetName)
      }
    } catch (e) {
      error('Failed to rename self: %s', e.message)
    }
  }

  async #performHandshake(socketPath) {
    const net = require('net')

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
          socket.end()
          try {
            require('fs').unlinkSync(socketPath)
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
