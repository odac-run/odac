const {log, error} = Odac.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const https = require('https')
const packageJson = require('../../package.json')

const System = require('./Hub/System')
const {WebSocketClient, MessageSigner} = require('./Hub/WebSocket')

const HUB_URL = process.env.ODAC_HUB_URL || 'https://hub.odac.run'
const HUB_WS_URL = HUB_URL.replace(/^http/, 'ws') + '/ws'

class Hub {
  #active = false
  #logSubs = new Map()

  constructor() {
    this.ws = new WebSocketClient()

    // Commands and Tasks
    this.commands = {
      configure: {
        fn: payload => this.#handleConfigure(payload)
      },
      'app.create': {
        fn: payload => Odac.server('App').create(payload),
        triggers: ['app.list']
      },
      'app.build_stats': {
        fn: payload => Odac.server('App').getBuildStats(payload.name || payload.container || payload.id)
      },
      'app.delete': {
        fn: payload => Odac.server('App').delete(payload.id),
        triggers: ['app.list']
      },
      'app.env.get': {
        fn: payload => Odac.server('App').getEnv(payload.name || payload.id)
      },
      'app.env.delete': {
        fn: payload => Odac.server('App').deleteEnv(payload.name || payload.id, payload.keys),
        triggers: ['app.list']
      },
      'app.env.link': {
        fn: payload => Odac.server('App').linkEnv(payload.name || payload.id, payload.target),
        triggers: ['app.list']
      },
      'app.env.set': {
        fn: payload => Odac.server('App').setEnv(payload.name || payload.id, payload.env),
        triggers: ['app.list']
      },
      'app.env.unlink': {
        fn: payload => Odac.server('App').unlinkEnv(payload.name || payload.id, payload.target),
        triggers: ['app.list']
      },
      'app.list': {
        fn: () => Odac.server('App').list(true),
        interval: 30 * 60 * 1000,
        lastRun: 0
      },
      'app.redeploy': {
        fn: payload => Odac.server('App').redeploy(payload),
        triggers: ['app.list', 'app.stats']
      },
      'app.restart': {
        fn: payload => Odac.server('App').restart(payload.container),
        triggers: ['app.list', 'app.stats']
      },
      'app.stats': {
        fn: () => this.getAppStats(),
        interval: 60 * 1000,
        lastRun: 0
      },
      'domain.add': {
        fn: payload => Odac.server('Domain').add(payload.domain, payload.app),
        triggers: ['domain.list', 'system.info']
      },
      'domain.delete': {
        fn: payload => Odac.server('Domain').delete(payload.domain),
        triggers: ['domain.list', 'system.info']
      },
      'domain.list': {
        fn: () => Odac.server('Domain').list(),
        interval: 30 * 60 * 1000,
        lastRun: 0
      },
      'system.info': {
        fn: () => Odac.server('Api').result(true, System.getSystemInfo()),
        interval: 60 * 60 * 1000,
        lastRun: 0
      },
      'app.logs.on': {
        fn: payload => {
          log('[Hub] Subscription request for app: %s', payload.app)

          if (this.#logSubs.has(payload.app)) {
            return {success: true, message: 'Already subscribed'}
          }

          let buffer = []
          let timer = null

          const flush = () => {
            if (buffer.length === 0) return
            // Send as batch to reduce overhead
            this.#sendSignedMessage('log.stream', {
              app: payload.app,
              batch: buffer
            })
            buffer = []
            timer = null
          }

          const unsubscribe = Odac.server('App').subscribeToLogs(payload.app, logData => {
            buffer.push(logData)
            if (buffer.length >= 50) {
              // Max 50 logs per packet
              if (timer) clearTimeout(timer)
              flush()
            } else if (!timer) {
              timer = setTimeout(flush, 500) // Max 500ms delay
            }
          })

          if (unsubscribe) {
            log('[Hub] Successfully subscribed to %s', payload.app)
            // Wrap unsubscribe to clear timer
            const safeUnsubscribe = () => {
              if (timer) clearTimeout(timer)
              flush() // Send remaining
              unsubscribe()
            }
            this.#logSubs.set(payload.app, safeUnsubscribe)
            return {success: true, message: 'Subscribed to logs'}
          }

          return {success: false, message: 'App not running or logs unavailable'}
        }
      },
      'app.logs.off': {
        fn: payload => {
          const unsub = this.#logSubs.get(payload.app)
          if (unsub) {
            unsub()
            this.#logSubs.delete(payload.app)
          }
        }
      },
      'app.build_logs.on': {
        fn: async payload => {
          const key = payload.app + ':build'
          if (this.#logSubs.has(key)) return {success: true, message: 'Already subscribed'}

          let buffer = []
          let timer = null
          const flush = () => {
            if (buffer.length === 0) return
            this.#sendSignedMessage('build.log', {app: payload.app, batch: buffer})
            buffer = []
            timer = null
          }

          const unsubscribe = Odac.server('Container').subscribeToBuildLogs(payload.app, logData => {
            buffer.push(logData)
            if (buffer.length >= 50) {
              if (timer) clearTimeout(timer)
              flush()
            } else if (!timer) {
              timer = setTimeout(flush, 500)
            }
          })

          if (unsubscribe) {
            const safeUnsubscribe = () => {
              if (timer) clearTimeout(timer)
              flush()
              unsubscribe()
            }
            this.#logSubs.set(key, safeUnsubscribe)
            return {success: true, message: 'Subscribed to active build logs'}
          }

          // No active build -> Send last log
          const content = await Odac.server('Container').getLastBuildLog(payload.app)
          this.#sendSignedMessage('build.log', {
            app: payload.app,
            content: content,
            finished: true
          })

          return {success: true, message: 'Sent last build log'}
        }
      },
      'app.build_logs.off': {
        fn: payload => {
          const key = payload.app + ':build'
          const unsub = this.#logSubs.get(key)
          if (unsub) {
            unsub()
            this.#logSubs.delete(key)
          }
          return {success: true, message: 'Unsubscribed from build logs'}
        }
      },
      'updater.start': {
        fn: () => Odac.server('Updater').start()
      }
    }

    this.agent = new https.Agent({
      rejectUnauthorized: true,
      keepAlive: true,
      keepAliveMsecs: 1000,
      maxSockets: 25,
      timeout: 30000
    })

    this.ws.setHandlers({
      onConnect: () => {
        this.trigger('system.info')
        this.trigger('app.list')
        this.trigger('domain.list')
      },
      onMessage: data => this.#handleMessage(data),
      onDisconnect: () => {
        log('[Hub] Disconnected from Cloud. Cleaning up active log streams...')
        this.#unsubscribeAllLogs()
      }
    })
  }

  // Public API
  start() {
    this.#active = true
    log('Hub Service started')
  }

  stop() {
    this.#active = false
    this.ws.disconnect()
    log('Hub Service stopped')
  }

  isAuthenticated() {
    return !!this.#getHubConfig()?.token
  }

  check() {
    if (!this.#active) return

    const hub = this.#getHubConfig()
    if (!hub?.token) return

    if (!this.ws.connected) {
      if (this.ws.shouldReconnect()) this.ws.connect(HUB_WS_URL, hub.token)
      return
    }

    const now = Date.now()
    for (const [name, command] of Object.entries(this.commands)) {
      // Skip disabled tasks (interval is 0, false, null, undefined)
      if (!command.interval || command.interval <= 0) continue

      if (now - command.lastRun >= command.interval) {
        command.lastRun = now
        // Execute task without blocking the loop
        this.#executeTask(name, command)
      }
    }
  }

  // Trigger a specific task manually (e.g. after an event)
  async trigger(name) {
    const command = this.commands[name]
    if (command) {
      if (command.interval) command.lastRun = Date.now() // Reset timer if it's a task
      await this.#executeTask(name, command)
    }
  }

  async #executeTask(name, command) {
    if (!this.ws.connected) return
    try {
      const data = await command.fn()
      if (data !== undefined) {
        this.#sendSignedMessage(name, data)
      }
    } catch (e) {
      log('Task %s error: %s', name, e.message)
    }
  }

  getSystemStatus() {
    return System.getStatus()
  }

  getLinuxDistro() {
    return System.getLinuxDistro()
  }

  async getAppStats() {
    const res = await Odac.server('App').list(true)
    const apps = res.result ? res.data : []
    const statsData = {}

    if (Array.isArray(apps)) {
      for (const app of apps) {
        if (app.status === 'running') {
          const stats = await Odac.server('Container').getStats(app.name)
          if (stats) statsData[app.name] = stats
        }
      }
    }

    return Odac.server('Api').result(true, statsData)
  }

  // HTTP API
  async auth(code) {
    log('Odac authenticating...')
    log('Auth code received: %s', code ? code.substring(0, 8) + '...' : 'none')

    const distro = this.getLinuxDistro()

    const data = {
      code,
      os: os.platform(),
      arch: os.arch(),
      hostname: os.hostname(),
      version: packageJson.version,
      node: process.version
    }

    if (distro) data.distro = distro

    try {
      const response = await this.call('auth', data)

      if (!this.#validateSchema(response, {token: 'string', secret: 'string'})) {
        throw new Error(__('Invalid authentication response format'))
      }

      Odac.core('Config').config.hub = {
        token: response.token,
        secret: response.secret
      }

      log('Odac authenticated!')
      return Odac.server('Api').result(true, __('Authentication successful'))
    } catch (error) {
      log('Authentication failed: %s', error || 'Unknown error')
      return Odac.server('Api').result(false, error || __('Authentication failed'))
    }
  }

  async call(action, data, retries = 3) {
    log('Hub API call: %s', action)

    const url = `${HUB_URL}/${action}`
    const headers = this.#buildHeaders(action, data)

    for (let attempt = 1; attempt <= retries; attempt++) {
      try {
        const response = await axios.post(url, data, {
          headers,
          httpsAgent: this.agent,
          timeout: 30000
        })

        return this.#parseResponse(action, response)
      } catch (error) {
        const isRetryable = error.code === 'EAI_AGAIN' || error.code === 'ENOTFOUND' || error.code === 'ETIMEDOUT'

        if (isRetryable && attempt < retries) {
          log('Hub API call failed (attempt %d/%d): %s - Retrying...', attempt, retries, error.code)
          await new Promise(r => setTimeout(r, 1000 * attempt))
          continue
        }

        return this.#handleApiError(action, error)
      }
    }
  }

  async getApp(name) {
    return this.call('app', {name})
  }

  // Command Processing
  async processCommand(command) {
    const cmd = this.commands[command.action]
    if (!command?.action || !cmd) {
      log('Invalid or unknown command received: %s', command?.action)
      return
    }

    const {fn, interval, triggers} = cmd
    log('Processing command: %s', command.action)

    try {
      if (interval) cmd.lastRun = Date.now() // Reset timer if it's a task

      const result = await fn(command.payload)

      // Send response if requested
      if (command.requestId) {
        this.#sendCommandResponse(command.requestId, result || {result: true})
      }

      // Trigger related tasks
      if (triggers && Array.isArray(triggers)) {
        for (const task of triggers) {
          await this.trigger(task)
        }
      }
    } catch (e) {
      log('Command execution failed: %s', e.message)
      if (command.requestId) {
        this.#sendCommandResponse(command.requestId, {result: false, message: e.message})
      }
    }
  }

  #sendCommandResponse(requestId, result) {
    // Normalize: Api.result() returns {result, message}, but we need {success, message}
    const success = result.success !== undefined ? result.success : result.result
    this.#sendSignedMessage('command.response', {
      id: requestId,
      success,
      message: result.message,
      data: result.data
    })
  }

  // Private Helpers
  #unsubscribeAllLogs() {
    if (this.#logSubs.size === 0) return
    log('[Hub] Clearing %d active log subscriptions', this.#logSubs.size)
    for (const [app, unsub] of this.#logSubs.entries()) {
      try {
        unsub() // Clears timer and buffer
      } catch (e) {
        error('[Hub] Error unsubscribing from %s: %s', app, e.message)
      }
    }
    this.#logSubs.clear()
  }

  #getHubConfig() {
    return Odac.core('Config').config.hub
  }

  #sendSignedMessage(type, data) {
    const timestamp = Math.floor(Date.now() / 1000)
    const hub = this.#getHubConfig()

    const message = {
      type,
      data,
      timestamp,
      signature: MessageSigner.sign({type, data, timestamp}, hub?.secret)
    }

    this.ws.send(message)
  }

  #handleMessage(data) {
    try {
      const message = JSON.parse(data.toString())
      const hub = this.#getHubConfig()

      if (!MessageSigner.verify(message, hub?.secret)) {
        log('WebSocket message verification failed')
        return
      }

      if (message.type === 'disconnect') {
        log('Cloud requested disconnect: %s', message.reason || 'unknown')
        if (message.reason === 'token_invalid' || message.reason === 'signature_invalid') {
          log('Authentication credentials invalid, clearing config')
          delete Odac.core('Config').config.hub
        }
        this.ws.disconnect()
        return
      }

      if (message.type === 'command') {
        this.processCommand({
          ...message.data,
          requestId: message.id || message.requestId
        })
      }
    } catch (error) {
      log('Failed to handle WebSocket message: %s', error.message)
    }
  }

  #handleConfigure(payload) {
    if (!this.#validateSchema(payload, {intervals: 'object'})) {
      log('Invalid configure payload')
      return
    }

    const {intervals} = payload
    let updated = false

    for (const [key, value] of Object.entries(intervals)) {
      if (value === undefined) continue

      const command = this.commands[key]
      if (command && command.interval !== undefined) {
        // If 0, false or null is sent, it will disable the task (interval = 0)
        const newInterval = value ? value * 1000 : 0
        if (command.interval !== newInterval) {
          command.interval = newInterval
          updated = true
          log('Task interval updated: %s = %sms', key, newInterval)
        }
      }
    }

    if (updated) log('Configuration updated: intervals synced')
  }

  #validateSchema(data, schema) {
    if (!data || typeof data !== 'object') return false

    for (const [key, type] of Object.entries(schema)) {
      if (data[key] === undefined || typeof data[key] !== type) {
        return false
      }
    }
    return true
  }

  #buildHeaders(action, data) {
    const headers = {}
    const hub = this.#getHubConfig()

    if (hub?.token) {
      headers['Authorization'] = `Bearer ${hub.token}`
    }

    if (action !== 'auth' && data.timestamp && hub?.secret) {
      headers['X-Signature'] = nodeCrypto.createHmac('sha256', hub.secret).update(JSON.stringify(data)).digest('hex')
    }

    return headers
  }

  #parseResponse(action, response) {
    if (!response.data?.result) {
      throw new Error('Invalid response format')
    }

    if (!response.data.result.success) {
      if (response.data.result.authenticated === false) {
        return response.data.result
      }
      throw new Error(response.data.result.message)
    }

    log('API call successful: %s', action)
    return response.data.data
  }

  #handleApiError(action, error) {
    log('API call failed: %s - %s', action, error.message)

    if (error.response) {
      throw error.response.data
    } else if (error.request) {
      throw new Error('No response from server')
    } else {
      throw new Error(error.message)
    }
  }
}

module.exports = new Hub()
