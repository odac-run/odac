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

// Logs are appended incrementally; once a file passes MAX_LINES it is rewritten
// down to the last TRIM_LINES, then appending resumes.
const MAX_LINES = 2000
const TRIM_LINES = 1000

class Watchdog {
  // buf: in-memory content, flushed: bytes already on disk (-1 forces a full
  // rewrite, e.g. first flush or after a log-name switch), dirty: has new data.
  #log = {buf: '', flushed: -1, dirty: false}
  #err = {buf: '', flushed: -1, dirty: false}
  #restartCount = 0
  #lastRestartTimestamp = 0
  #isSaving = false
  #savePromise = null

  init() {
    setInterval(() => this.#saveLogs(), SAVE_INTERVAL_MS)
    this.#startServer()
  }

  #saveLogs() {
    // Return the in-flight save so callers (e.g. shutdown) can await it.
    if (this.#isSaving) return this.#savePromise
    if (!this.#log.dirty && !this.#err.dirty) return Promise.resolve()
    this.#isSaving = true
    this.#savePromise = this.#runSave()
    return this.#savePromise
  }

  async #runSave() {
    try {
      await fs.mkdir(LOG_DIR, {recursive: true})
      const logName = process.env.ODAC_LOG_NAME || '.odac'

      // Flush independently: a failure on the standard log must not skip the
      // error log, which carries the crash reason. Each #flush keeps its own
      // buffer dirty on failure, so the skipped one retries next interval.
      try {
        await this.#flush(this.#log, path.join(LOG_DIR, `${logName}.log`))
      } catch (e) {
        console.error('Failed to save standard log:', e)
      }
      try {
        await this.#flush(this.#err, path.join(LOG_DIR, `${logName}_err.log`))
      } catch (e) {
        console.error('Failed to save error log:', e)
      }
    } catch (error) {
      console.error('Failed to save logs:', error)
    } finally {
      this.#isSaving = false
    }
  }

  /**
   * Flushes any pending logs without interrupting an in-flight write, then exits.
   */
  async #shutdown(code) {
    try {
      if (this.#savePromise) await this.#savePromise
      await this.#saveLogs()
    } catch {
      /* best effort — exit regardless */
    }
    process.exit(code)
  }

  /**
   * Appends new buffer content to its file, rewriting down to TRIM_LINES once
   * the buffer grows past MAX_LINES.
   */
  async #flush(state, file) {
    if (!state.dirty) return

    const prevFlushed = state.flushed
    const lines = state.buf.split('\n')
    const rewrite = state.flushed < 0 || lines.length > MAX_LINES

    let payload
    if (rewrite) {
      if (lines.length > TRIM_LINES) state.buf = lines.slice(-TRIM_LINES).join('\n')
      payload = state.buf
    } else {
      payload = state.buf.slice(state.flushed)
    }

    // Advance the offset and clear the flag before awaiting so data arriving
    // mid-write is picked up by the next flush instead of being dropped.
    state.flushed = state.buf.length
    state.dirty = false

    try {
      if (rewrite) await fs.writeFile(file, payload, 'utf8')
      else if (payload) await fs.appendFile(file, payload, 'utf8')
    } catch (err) {
      // Retry next cycle: re-append the same bytes, or force a fresh rewrite.
      state.dirty = true
      state.flushed = rewrite ? -1 : prevFlushed
      throw err
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

      for (const app of Odac.core('Config').config.apps ?? []) if (app.pid) await Odac.core('Process').stop(app.pid)

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
      const str = data.toString()
      if (str.includes('ODAC_CMD:SWITCH_LOGS')) {
        console.log('Watchdog: Switching to standard logs (.odac.log)...')
        process.env.ODAC_LOG_NAME = '.odac'
        // New file name: force a full rewrite for both streams.
        this.#log.flushed = -1
        this.#err.flushed = -1
        this.#err.dirty = true
      }
      this.#log.buf += `[LOG][${new Date().toISOString()}] ${str}`
      this.#log.dirty = true
    })

    child.stderr.on('data', data => {
      const line = `[ERR][${new Date().toISOString()}] ${data.toString()}`
      this.#log.buf += line
      this.#err.buf += line
      this.#log.dirty = true
      this.#err.dirty = true
    })

    child.on('close', code => {
      // If server exited intentionally (code 0), shutdown watchdog too
      if (code === 0) {
        console.log('Server process exited normally (code 0). Watchdog shutting down.')
        return this.#shutdown(0)
      }

      Odac.core('Config').reload()
      this.#err.buf += `[ERR][${new Date().toISOString()}] Process closed with code ${code}\n`
      this.#err.dirty = true

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
        this.#shutdown(1)
      }
    })
  }
}

module.exports = new Watchdog()
