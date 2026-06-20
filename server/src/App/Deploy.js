const {log, error} = Odac.core('Log', false).init('App.Deploy')
const fs = require('fs')
const http = require('http')
const os = require('os')
const path = require('path')

class Deploy {
  // Matches the green container name suffix produced by App's runtime id generator:
  // `<appName>-green-<13+digit-ts>_<8-hex>`. Used to detect orphaned log dirs
  // and Map entries left behind by Blue-Green deploys.
  static GREEN_SUFFIX_RE = /-green-\d+_[a-f0-9]{8}$/

  #api

  constructor(api) {
    this.#api = api
  }

  async performBlueGreenDeploy(app, greenContainerName, options = {}) {
    const {logCtrl = null, operation = 'Redeploy', runGreenContainer, setStarting = false} = options
    const api = this.#api

    if (typeof runGreenContainer !== 'function') {
      throw new Error('Blue-Green deploy requires a runGreenContainer function.')
    }

    if (!app.ports || app.ports.length === 0 || !app.ports[0].container) {
      log('Legacy App Fix: Assigning default port 3000 to app %s during %s', app.name, operation.toLowerCase())
      app.ports = [{container: 3000}]
      api.saveApps()
    }

    if (setStarting) {
      api.set(app.id, {status: 'starting'})
    }

    if (logCtrl) logCtrl.startPhase('start_new_container')
    await runGreenContainer()
    if (logCtrl) logCtrl.endPhase('start_new_container', true)

    api.set(app.id, {status: 'switching'})

    let expectedPort = 3000
    if (app.ports && app.ports.length > 0 && app.ports[0].container) {
      expectedPort = app.ports[0].container
    }

    let isReady = false
    let attempts = 0
    let greenIP = null
    let lastReadinessError = null
    while (attempts < 120) {
      try {
        const listeningPorts = await Odac.server('Container').getListeningPorts(greenContainerName)
        if (listeningPorts.includes(expectedPort)) {
          greenIP = await Odac.server('Container').getIP(greenContainerName)
          if (greenIP) {
            isReady = true
            break
          }
        }
      } catch (e) {
        lastReadinessError = e && e.message ? e.message : 'unknown readiness probe error'
      }

      await new Promise(r => setTimeout(r, 1000))
      attempts++
    }

    if (!isReady || !greenIP) {
      await Odac.server('Container').stop(greenContainerName)
      await Odac.server('Container').remove(greenContainerName)
      await this.cleanupGreenArtifacts(greenContainerName)
      const details = lastReadinessError ? ` Last readiness error: ${lastReadinessError}` : ''
      throw new Error(`New container failed readiness probe (port bind timeout). ${operation} aborted to maintain uptime.${details}`)
    }

    const httpReady = await this.#httpHealthCheck(greenIP, expectedPort)
    if (!httpReady) {
      await Odac.server('Container').stop(greenContainerName)
      await Odac.server('Container').remove(greenContainerName)
      await this.cleanupGreenArtifacts(greenContainerName)
      throw new Error(`New container failed HTTP readiness probe. ${operation} aborted to maintain uptime.`)
    }

    app.ip = greenIP
    api.set(app.id, {status: 'running', activeContainerId: greenContainerName})

    api.scanAndSaveHttpStatus(app).catch(e => error('HTTP scan failed for %s: %s', app.name, e.message))

    if (logCtrl) logCtrl.startPhase('proxy_propagation')
    await Odac.server('Proxy').syncConfig()
    Odac.server('Proxy').purgeCacheForApp(app.id)
    if (logCtrl) logCtrl.endPhase('proxy_propagation', true)

    if (logCtrl) logCtrl.startPhase('stop_old_container')
    if (process.env.NODE_ENV !== 'test') {
      await new Promise(r => setTimeout(r, 5000))
    }

    const oldRuntimeLog = api.logStreams.get(app.name)
    if (oldRuntimeLog && typeof oldRuntimeLog.end === 'function') oldRuntimeLog.end()
    api.logStreams.delete(app.name)

    await Odac.server('Container').stop(app.name)
    await Odac.server('Container').remove(app.name)

    const greenRuntimeLog = api.logStreams.get(greenContainerName)
    if (greenRuntimeLog && typeof greenRuntimeLog.end === 'function') greenRuntimeLog.end()
    api.logStreams.delete(greenContainerName)

    let renameSuccess = false
    for (let i = 0; i < 5; i++) {
      try {
        await Odac.server('Container').docker.getContainer(greenContainerName).rename({name: app.name})
        renameSuccess = true
        break
      } catch (e) {
        log('Docker rename failed, retrying in 2s (Attempt %d/5): %s', i + 1, e.message)
        await new Promise(r => setTimeout(r, 2000))
      }
    }

    if (renameSuccess) {
      api.set(app.id, {activeContainerId: null})
      // Docker container has been renamed away from greenContainerName, so the
      // on-disk log dir under that name is orphaned. Skip on rename failure —
      // there the green name is still the live container.
      await this.cleanupGreenArtifacts(greenContainerName)
    } else {
      error(
        'Failed to rename green container %s to %s after 5 attempts. ZDD will persist with activeContainerId.',
        greenContainerName,
        app.name
      )
    }

    await api.attachLogger(app)

    if (logCtrl) {
      await new Promise(r => setTimeout(r, 1000))
      logCtrl.endPhase('stop_old_container', true)
    }

    api.set(app.id, {started: Date.now()})
  }

  /**
   * Performs an HTTP-level health check against a container.
   * TCP port listening does NOT guarantee the app can serve HTTP traffic.
   * This method sends actual HTTP requests to verify L7 readiness,
   * eliminating the brief 502 window during Blue-Green ZDD switches.
   */
  async #httpHealthCheck(ip, port, maxAttempts = 30) {
    for (let i = 0; i < maxAttempts; i++) {
      try {
        await new Promise((resolve, reject) => {
          const req = http.get({hostname: ip, port, path: '/', timeout: 2000}, res => {
            res.resume()
            resolve()
          })
          req.on('error', reject)
          req.on('timeout', () => {
            req.destroy()
            reject(new Error('timeout'))
          })
        })
        log('HTTP health check passed for %s:%d (attempt %d)', ip, port, i + 1)
        return true
      } catch {
        await new Promise(r => setTimeout(r, 1000))
      }
    }
    return false
  }

  // Removes the on-disk log dir + in-memory bookkeeping for a green container
  // that is no longer live (either successfully renamed away or aborted).
  // Safe to call with a non-green name (no-op if pattern doesn't match).
  async cleanupGreenArtifacts(greenContainerName) {
    if (!greenContainerName || !Deploy.GREEN_SUFFIX_RE.test(greenContainerName)) return
    const logDir = path.join(os.homedir(), '.odac', 'logs', greenContainerName)
    try {
      await fs.promises.rm(logDir, {recursive: true, force: true})
    } catch (e) {
      log('Failed to remove stale green log dir for %s: %s', greenContainerName, e.message)
    }
    this.#api.loggers.delete(greenContainerName)
    this.#api.logStreams.delete(greenContainerName)
    try {
      Odac.server('Container').unregisterBuildLogger(greenContainerName)
    } catch {
      /* ignore */
    }
  }

  // Startup sweep for green log dirs that survived past Blue-Green deploys
  // before the prevention fix was in place (or from a future bug that lets
  // one leak again). Names matching `*-green-<ts>_<hex>` are short-lived by
  // contract — they only exist while a green container is mid-flight, so any
  // such dir found at startup is orphaned.
  async cleanupStaleGreenLogs() {
    const logsRoot = path.join(os.homedir(), '.odac', 'logs')
    let entries
    try {
      entries = await fs.promises.readdir(logsRoot, {withFileTypes: true})
    } catch {
      return
    }
    let removed = 0
    for (const ent of entries) {
      if (!ent.isDirectory()) continue
      if (!Deploy.GREEN_SUFFIX_RE.test(ent.name)) continue
      try {
        await fs.promises.rm(path.join(logsRoot, ent.name), {recursive: true, force: true})
        removed++
      } catch (e) {
        log('Failed to remove stale green log dir %s: %s', ent.name, e.message)
      }
    }
    if (removed) log('Cleaned up %d stale green log dir(s)', removed)
  }

  async sweepGreenContainersFor(appName, activeContainerId) {
    const container = Odac.server('Container')
    if (!container.available) return

    const candidates = new Set()
    if (activeContainerId && activeContainerId !== appName) candidates.add(activeContainerId)

    const greenPrefix = `${appName}-green-`
    try {
      const all = await container.list()
      for (const c of all) {
        for (const rawName of c.names || []) {
          const name = rawName.replace(/^\//, '')
          if (name.startsWith(greenPrefix) && Deploy.GREEN_SUFFIX_RE.test(name)) {
            candidates.add(name)
          }
        }
      }
    } catch (e) {
      log('Delete[%s]: green sweep listing failed: %s', appName, e.message)
    }

    for (const name of candidates) {
      log('Delete[%s]: sweeping green companion %s', appName, name)
      try {
        await container.stop(name)
      } catch {
        /* already stopped or gone */
      }
      try {
        await container.remove(name)
      } catch {
        /* already removed */
      }
      await this.cleanupGreenArtifacts(name)
    }
  }
}

module.exports = Deploy
