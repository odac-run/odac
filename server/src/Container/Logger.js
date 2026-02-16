const fs = require('fs')
const path = require('path')
const os = require('os')
const {Transform} = require('stream')

// Standard Logger initialization
const {error} = Odac.core('Log').init('Container', 'Logger')

class Logger {
  #appName
  #logsDir
  #buildsDir
  #runtimeDir

  constructor(appName) {
    this.#appName = appName
    this.#logsDir = path.join(os.homedir(), '.odac', 'logs', appName)
    this.#buildsDir = path.join(this.#logsDir, 'builds')
    this.#runtimeDir = path.join(this.#logsDir, 'runtime')
  }

  /**
   * Initializes the logger directories properly
   */
  async init() {
    try {
      await fs.promises.mkdir(this.#buildsDir, {recursive: true})
      await fs.promises.mkdir(this.#runtimeDir, {recursive: true})
    } catch (e) {
      error('Failed to initialize logger directories for %s: %s', this.#appName, e.message)
    }
  }

  /**
   * Creates a write stream for a new build and analyzes it on the fly
   * @param {string} buildId - Unique build identifier
   * @param {Object} metadata - Initial metadata (e.g. { trigger: 'git', branch: 'main' })
   * @returns {Object} { stream, analyzer } - Stream to pipe logs into, and the analyzer object
   */
  createBuildStream(buildId, metadata = {}) {
    const logFile = path.join(this.#buildsDir, `${buildId}.log`)
    const summaryFile = path.join(this.#buildsDir, `${buildId}.json`)

    const startTime = Date.now()
    const stats = {
      id: buildId,
      timestamp: startTime,
      duration: 0,
      status: 'pending', // pending, success, failed
      errors: 0,
      warnings: 0,
      phases: [], // Array of { name, start, end, duration, status }
      metadata
    }

    // Real-time analysis stream
    const analyzer = new Transform({
      transform(chunk, encoding, callback) {
        const line = chunk.toString()

        // Simple heuristics for analysis
        const isError = /error/i.test(line) && !/node_modules/i.test(line)
        const isWarning = /warning/i.test(line) && !/npm warn/i.test(line)

        if (isError) {
          stats.errors++
          // Increment for active phases
          for (const p of stats.phases) {
            if (!p.end) p.errors = (p.errors || 0) + 1
          }
        }

        if (isWarning) {
          stats.warnings++
          // Increment for active phases
          for (const p of stats.phases) {
            if (!p.end) p.warnings = (p.warnings || 0) + 1
          }
        }

        this.push(chunk) // Pass through
        callback()
      }
    })

    const fileStream = fs.createWriteStream(logFile, {flags: 'a'})
    analyzer.pipe(fileStream)

    // Return control object
    return {
      stream: analyzer, // Pipe source to this
      path: logFile,

      // Call this when a specific phase starts
      startPhase: phaseName => {
        stats.phases.push({
          name: phaseName,
          start: Date.now(),
          status: 'running',
          errors: 0,
          warnings: 0
        })
      },

      // Call this when a phase ends
      endPhase: (phaseName, success = true) => {
        // Find last matching phase that is still running (no end time)
        for (let i = stats.phases.length - 1; i >= 0; i--) {
          const phase = stats.phases[i]
          if (phase.name === phaseName && !phase.end) {
            phase.end = Date.now()
            phase.duration = (phase.end - phase.start) / 1000
            phase.status = success ? 'success' : 'failed'
            break
          }
        }
      },

      // Call this to finalize the log
      finalize: async (success = true) => {
        // Auto-close any pending phases
        const now = Date.now()
        for (const phase of stats.phases) {
          if (!phase.end) {
            phase.end = now
            phase.duration = (phase.end - phase.start) / 1000
            phase.status = success ? 'success' : 'failed'
          }
        }

        stats.status = success ? 'success' : 'failed'
        stats.duration = (now - startTime) / 1000

        // Write summary
        try {
          await fs.promises.writeFile(summaryFile, JSON.stringify(stats, null, 2))
        } catch (e) {
          error('Failed to write build summary: %s', e.message)
        }

        this.#rotateLogs() // Trigger cleanup in background
      }
    }
  }

  /**
   * Creates a write stream for runtime logs with daily rotation
   * @returns {Object} { stream, path }
   */
  createRuntimeStream() {
    const today = new Date().toISOString().split('T')[0] // YYYY-MM-DD
    const logFile = path.join(this.#runtimeDir, `${today}.log`)

    // For now, simple append stream
    const fileStream = fs.createWriteStream(logFile, {flags: 'a'})

    // Auto-rotate check (simple approach: check on creation)
    this.#rotateRuntimeLogs()

    return {
      stream: fileStream,
      path: logFile
    }
  }

  /**
   * Returns the most recent build summary
   */
  async getLastBuild() {
    try {
      const files = await fs.promises.readdir(this.#buildsDir)
      const jsonFiles = files.filter(f => f.endsWith('.json'))
      if (jsonFiles.length === 0) return null

      // Sort by filename (timestamp based) desc
      jsonFiles.sort().reverse()

      const content = await fs.promises.readFile(path.join(this.#buildsDir, jsonFiles[0]), 'utf8')
      const data = JSON.parse(content)
      const phases = data.phases || []

      // Sort phases to show linear progression (completed first)
      phases.sort((a, b) => {
        if (a.end && b.end) return a.end - b.end
        if (a.end) return -1
        if (b.end) return 1
        return a.start - b.start
      })

      return {
        id: data.id,
        status: data.status,
        time: data.timestamp,
        duration: data.duration,
        errors: data.errors || 0,
        warnings: data.warnings || 0,
        phases,
        metadata: data.metadata || {}
      }
    } catch {
      return null
    }
  }

  /**
   * Returns summary stats for the last 24 hours
   */
  async getDailySummary() {
    const oneDayAgo = Date.now() - 24 * 60 * 60 * 1000
    const summary = {
      total: 0,
      success: 0,
      failed: 0,
      totalDuration: 0,
      avgDuration: 0,
      builds: []
    }

    try {
      const files = await fs.promises.readdir(this.#buildsDir)
      const jsonFiles = files.filter(f => f.endsWith('.json'))

      for (const file of jsonFiles) {
        try {
          const content = await fs.promises.readFile(path.join(this.#buildsDir, file), 'utf8')
          const data = JSON.parse(content)

          if (data.timestamp > oneDayAgo) {
            summary.total++
            if (data.status === 'success') summary.success++
            else summary.failed++

            summary.totalDuration += data.duration || 0
            summary.builds.push({
              id: data.id,
              status: data.status,
              time: data.timestamp,
              duration: data.duration,
              errors: data.errors,
              phases: data.phases || [],
              metadata: data.metadata || {}
            })
          }
        } catch {
          // Ignore corrupted files
        }
      }

      if (summary.total > 0) {
        summary.avgDuration = parseFloat((summary.totalDuration / summary.total).toFixed(2))
      }

      // Sort by newest first
      summary.builds.sort((a, b) => b.time - a.time)
    } catch (e) {
      if (e.code !== 'ENOENT') {
        error('Failed to get daily summary: %s', e.message)
      }
    }

    return summary
  }

  /**
   * Private: Rotates logs (Retention Policy)
   * Keeps last 10 builds to save space
   */
  async #rotateLogs() {
    try {
      const files = await fs.promises.readdir(this.#buildsDir)
      const jsonFiles = files.filter(f => f.endsWith('.json'))

      if (jsonFiles.length <= 10) return

      const fileStats = await Promise.all(
        jsonFiles.map(async f => {
          const stat = await fs.promises.stat(path.join(this.#buildsDir, f))
          return {name: f, time: stat.mtimeMs}
        })
      )

      fileStats.sort((a, b) => b.time - a.time) // Newest first

      const toDelete = fileStats.slice(10) // Keep top 10

      for (const item of toDelete) {
        const id = item.name.replace('.json', '')
        await fs.promises.unlink(path.join(this.#buildsDir, item.name)).catch(() => {}) // JSON
        await fs.promises.unlink(path.join(this.#buildsDir, `${id}.log`)).catch(() => {}) // LOG
      }
    } catch (e) {
      error('Log rotation failed: %s', e.message)
    }
  }

  async #rotateRuntimeLogs() {
    try {
      const files = await fs.promises.readdir(this.#runtimeDir)
      // Delete logs older than 7 days
      const daysToKeep = 7
      const now = Date.now()

      for (const file of files) {
        if (!file.endsWith('.log')) continue

        try {
          const filePath = path.join(this.#runtimeDir, file)
          const stats = await fs.promises.stat(filePath)
          const diffDays = (now - stats.mtimeMs) / (1000 * 60 * 60 * 24)

          if (diffDays > daysToKeep) {
            await fs.promises.unlink(filePath).catch(() => {})
          }
        } catch {
          // Ignore
        }
      }
    } catch (e) {
      error('Runtime log rotation failed: %s', e.message)
    }
  }
}

module.exports = Logger
