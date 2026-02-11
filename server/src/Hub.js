const {log} = Odac.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const https = require('https')

const System = require('./Hub/System')
const {WebSocketClient, MessageSigner} = require('./Hub/WebSocket')

const HUB_URL = process.env.ODAC_HUB_URL || 'https://hub.odac.run'
const HUB_WS_URL = HUB_URL.replace(/^http/, 'ws') + '/ws'

class Hub {
  #active = false

  constructor() {
    this.ws = new WebSocketClient()

    // Task Definitions
    this.tasks = [
      {
        name: 'system.info',
        fn: () => System.getSystemInfo(),
        interval: 60 * 60 * 1000,
        lastRun: 0
      },
      {
        name: 'app.list',
        fn: () => Odac.server('App').list(true),
        interval: 30 * 60 * 1000,
        lastRun: 0
      },
      {
        name: 'domain.list',
        fn: () => Odac.server('Domain').list(),
        interval: 30 * 60 * 1000,
        lastRun: 0
      },
      {
        name: 'app.stats',
        fn: () => this.getAppStats(),
        interval: 60 * 1000,
        lastRun: 0
      }
    ]

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
      onDisconnect: () => {}
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
    for (const task of this.tasks) {
      // Skip disabled tasks (interval is 0, false, null, undefined)
      if (!task.interval || task.interval <= 0) continue

      if (now - task.lastRun >= task.interval) {
        task.lastRun = now
        // Execute task without blocking the loop
        this.#executeTask(task)
      }
    }
  }

  // Trigger a specific task manually (e.g. after an event)
  async trigger(name) {
    const task = this.tasks.find(t => t.name === name)
    if (task) {
      task.lastRun = Date.now() // Reset timer
      await this.#executeTask(task)
    }
  }

  async #executeTask(task) {
    if (!this.ws.connected) return
    try {
      const data = await task.fn()
      if (data !== undefined) {
        this.#sendSignedMessage(task.name, data)
      }
    } catch (e) {
      log(`Task ${task.name} error: ${e.message}`)
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

    return statsData
  }

  // HTTP API
  async auth(code) {
    log('Odac authenticating...')
    log('Auth code received: %s', code ? code.substring(0, 8) + '...' : 'none')

    const packageJson = require('../../package.json')
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
  processCommand(command) {
    if (!command?.action) {
      log('Invalid command structure received')
      return
    }

    log('Processing command: %s', command.action)

    switch (command.action) {
      case 'configure':
        this.#handleConfigure(command.payload)
        break
      case 'app.create':
        this.#handleAppCreate(command)
        break
      case 'app.restart':
        this.#handleAppRestart(command)
        break
      case 'domain.add':
        this.#handleDomainAdd(command)
        break
      case 'updater.start':
        Odac.server('Updater').start()
        break
      default:
        log('Unknown command action: %s', command.action)
    }
  }

  async #handleAppCreate(command) {
    const payload = command.payload

    if (!payload) {
      log('app.create: Missing payload')
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: 'Missing payload'
      })
      return
    }

    try {
      log('Creating app: %j', payload)
      const result = await Odac.server('App').create(payload)
      this.#sendCommandResponse(command.requestId, result)

      // Immediately trigger update
      await this.trigger('app.list')
    } catch (e) {
      log('app.create failed: %s', e.message)
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: e.message
      })
    }
  }

  async #handleAppRestart(command) {
    const payload = command.payload

    if (!payload?.container) {
      log('app.restart: Missing payload (container)')
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: 'Missing payload (container)'
      })
      return
    }

    const target = payload.container
    try {
      log('Restarting app: %s', target)
      const result = await Odac.server('App').restart(target)
      this.#sendCommandResponse(command.requestId, result)

      // Update Hub stats & info
      await this.trigger('app.stats')
      await this.trigger('app.list')
    } catch (e) {
      log('app.restart failed: %s', e.message)
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: e.message
      })
    }
  }

  async #handleDomainAdd(command) {
    const payload = command.payload

    if (!payload?.domain || !payload?.appId) {
      log('domain.add: Missing payload (domain or appId)')
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: 'Missing payload (domain or appId)'
      })
      return
    }

    try {
      log('Adding domain: %s for app: %s', payload.domain, payload.appId)
      const result = await Odac.server('Domain').add(payload.domain, payload.appId)
      this.#sendCommandResponse(command.requestId, result)

      // Immediately trigger update
      await this.trigger('domain.list')
      await this.trigger('system.info')
    } catch (e) {
      log('domain.add failed: %s', e.message)
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: e.message
      })
    }
  }

  #sendCommandResponse(requestId, result) {
    // Normalize: Api.result() returns {result, message}, but we need {success, message}
    const success = result.success !== undefined ? result.success : result.result
    this.#sendSignedMessage('command.response', {
      requestId,
      success,
      message: result.message,
      data: result.data
    })
  }

  // Private Helpers
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
        this.processCommand(message.data)
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

      const task = this.tasks.find(t => t.name === key)
      if (task) {
        // If 0, false or null is sent, it will disable the task (interval = 0)
        const newInterval = value ? value * 1000 : 0
        if (task.interval !== newInterval) {
          task.interval = newInterval
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
