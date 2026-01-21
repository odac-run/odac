const {spawn} = require('child_process')
const fs = require('fs').promises
const os = require('os')
const path = require('path')

// --- Constants ---
const ODAC_HOME = path.join(os.homedir(), '.odac')
const LOG_DIR = path.join(ODAC_HOME, 'logs')
const SERVER_SCRIPT_PATH = path.join(__dirname, '..', '..', 'server', 'index.js')

const MAX_RESTARTS_IN_WINDOW = 100
const RESTART_WINDOW_MS = 1000 * 60 * 5 // 5 minutes
const SAVE_INTERVAL_MS = 1000 // 1 second

class Watchdog {
  #logBuffer = ''
  #errorBuffer = ''
  #restartCount = 0
  #lastRestartTimestamp = 0
  #isSaving = false

  init() {
    // Set up periodic log saving. This is done only once.
    setInterval(() => this.#saveLogs(), SAVE_INTERVAL_MS)

    // Start the server for the first time.
    this.#startServer()
  }

  /**
   * Saves the buffered logs to their respective files.
   * This function is designed to be called periodically.
   * Keeps only the last 1000 lines in each log file.
   */
  async #saveLogs() {
    if (this.#isSaving) return
    this.#isSaving = true

    try {
      // Ensure log directory exists before attempting to write files
      await fs.mkdir(LOG_DIR, {recursive: true})
      const logFile = path.join(LOG_DIR, '.odac.log')
      const errFile = path.join(LOG_DIR, '.odac_err.log')

      // Limit log buffer to last 1000 lines
      const logLines = this.#logBuffer.split('\n')
      if (logLines.length > 1000) {
        this.#logBuffer = logLines.slice(-1000).join('\n')
      }

      // Limit error buffer to last 1000 lines
      const errLines = this.#errorBuffer.split('\n')
      if (errLines.length > 1000) {
        this.#errorBuffer = errLines.slice(-1000).join('\n')
      }

      await fs.writeFile(logFile, this.#logBuffer, 'utf8')
      await fs.writeFile(errFile, this.#errorBuffer, 'utf8')
    } catch (error) {
      console.error('Failed to save logs:', error)
    } finally {
      this.#isSaving = false
    }
  }

  /**
   * Performs startup checks to ensure a clean environment.
   * It kills any old watchdog or server processes that might still be running.
   * It also creates the necessary configuration files and directories if they don't exist.
   * @returns {Promise<boolean>} A promise that resolves to true if the checks pass.
   */
  async #performStartupChecks() {
    try {
      // Kill previous watchdog process if it exists and is different from the current one
      if (Odac.core('Config').config.server.watchdog && Odac.core('Config').config.server.watchdog !== process.pid)
        await Odac.core('Process').stop(Odac.core('Config').config.server.watchdog)

      // Kill previous server process if it exists
      if (Odac.core('Config').config.server.pid) await Odac.core('Process').stop(Odac.core('Config').config.server.pid)

      for (const domain of Object.keys(Odac.core('Config').config?.websites ?? []))
        if (Odac.core('Config').config.websites[domain].pid)
          await Odac.core('Process').stop(Odac.core('Config').config.websites[domain].pid)

      for (const service of Odac.core('Config').config.services ?? []) if (service.pid) await Odac.core('Process').stop(service.pid)

      // Update config with current watchdog's info
      Odac.core('Config').config.server.watchdog = process.pid
      Odac.core('Config').config.server.started = Date.now()
      Odac.core('Config').force()

      return new Promise(resolve => setTimeout(() => resolve(true), 1000))
    } catch (error) {
      console.error('Error during startup checks:', error)
      return false
    }
  }

  /**
   * Starts the server process and sets up monitoring.
   */
  async #startServer() {
    const checksPassed = await this.#performStartupChecks()
    if (!checksPassed) {
      console.error('Startup checks failed. Aborting.')
      process.exit(1)
    }

    // Ensure log directory exists before starting
    await fs.mkdir(LOG_DIR, {recursive: true})

    const child = spawn('node', [SERVER_SCRIPT_PATH])

    process.on('exit', () => child.kill())

    Odac.core('Config').config.server.pid = child.pid

    console.log(`Watchdog process started with PID: ${process.pid}`)
    console.log(`Server process started with PID: ${child.pid}`)

    child.stdout.on('data', data => {
      this.#logBuffer += `[LOG][${new Date().toISOString()}] ${data.toString()}`
    })

    child.stderr.on('data', data => {
      this.#logBuffer += `[ERR][${new Date().toISOString()}] ${data.toString()}`
      this.#errorBuffer += `[ERR][${new Date().toISOString()}] ${data.toString()}`
    })

    child.on('close', code => {
      // If server exited intentionally (code 0), shutdown watchdog too
      if (code === 0) {
        console.log('Server process exited normally (code 0). Watchdog shutting down.')
        return process.exit(0)
      }

      Odac.core('Config').reload()
      this.#errorBuffer += `[ERR][${new Date().toISOString()}] Process closed with code ${code}\n`

      // Reset restart count if the last restart was a while ago
      if (Date.now() - this.#lastRestartTimestamp > RESTART_WINDOW_MS) {
        this.#restartCount = 0
      }

      this.#restartCount++
      this.#lastRestartTimestamp = Date.now()

      // If restart limit is not exceeded, restart the server
      if (this.#restartCount < MAX_RESTARTS_IN_WINDOW) {
        console.log('Server process closed. Restarting...')
        // Relaunch the server process without setting up new intervals
        this.#startServer()
      } else {
        console.error('Server has crashed too many times. Not restarting.')
        // Final attempt to save logs before exiting
        this.#saveLogs().then(() => process.exit(1))
      }
    })
  }
}

module.exports = new Watchdog()
