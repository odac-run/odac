const {log} = Odac.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const https = require('https')

const System = require('./Hub/System')
const {WebSocketClient, MessageSigner} = require('./Hub/WebSocket')

const HUB_URL = 'https://hub.odac.run'
const HUB_WS_URL = 'wss://hub.odac.run/ws'
const CHECK_COUNTER_MAX = 3600

class Hub {
  constructor() {
    this.ws = new WebSocketClient()
    this.checkCounter = 0
    this.statsInterval = 60
    this.handshakeInterval = 60

    this.agent = new https.Agent({
      rejectUnauthorized: true,
      keepAlive: true,
      keepAliveMsecs: 1000,
      maxSockets: 25,
      timeout: 30000
    })

    this.ws.setHandlers({
      onConnect: () => this.sendInitialHandshake(),
      onMessage: data => this.#handleMessage(data),
      onDisconnect: () => {}
    })
  }

  // Public API
  isAuthenticated() {
    return !!this.#getHubConfig()?.token
  }

  check() {
    this.checkCounter++
    if (this.checkCounter > CHECK_COUNTER_MAX) this.checkCounter = 1

    const hub = this.#getHubConfig()
    if (!hub?.token) return

    if (!this.ws.connected) {
      if (this.ws.shouldReconnect()) this.ws.connect(HUB_WS_URL, hub.token)
      return
    }

    if (this.checkCounter % this.statsInterval === 0) this.sendContainerStats()
    if (this.checkCounter % this.handshakeInterval === 0) this.sendInitialHandshake()
  }

  getSystemStatus() {
    return System.getStatus()
  }

  getLinuxDistro() {
    return System.getLinuxDistro()
  }

  // WebSocket Messages
  async sendInitialHandshake() {
    if (!this.ws.connected) return

    const containers = await this.#getFormattedContainers()
    const system = System.getSystemInfo()

    this.#sendSignedMessage('connect_info', {system, containers})
  }

  sendWebSocketStatus() {
    if (!this.ws.connected) return

    const status = this.getSystemStatus()
    this.#sendSignedMessage('status', status)
  }

  async sendContainerStats() {
    if (!this.ws.connected) return
    if (!Odac.server('Container').available) return

    const containers = await Odac.server('Container').list()
    const relevantContainers = this.#filterRelevantContainers(containers)

    if (relevantContainers.length === 0) return

    const statsData = {}
    for (const c of relevantContainers) {
      const name = this.#getContainerName(c)
      const stats = await Odac.server('Container').getStats(c.id)
      if (stats) statsData[name] = stats
    }

    this.#sendSignedMessage('container_stats', statsData)
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
      case 'updater.start':
        try {
          Odac.server('Updater').start(command)
        } catch (e) {
          log('Updater module not found or failed: %s', e.message)
        }
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

      // Immediately update Hub with new container list
      await this.sendInitialHandshake()
    } catch (e) {
      log('app.create failed: %s', e.message)
      this.#sendCommandResponse(command.requestId, {
        success: false,
        message: e.message
      })
    }
  }

  #sendCommandResponse(requestId, result) {
    this.#sendSignedMessage('command_response', {
      requestId,
      success: result.success,
      message: result.message,
      data: result.data
    })
  }

  // Private Helpers
  #getHubConfig() {
    return Odac.core('Config').config.hub
  }

  #getContainerName(container) {
    return container.names?.[0]?.replace(/^\//, '') || 'unknown'
  }

  #filterRelevantContainers(containers) {
    const websites = Odac.core('Config').config.websites || {}
    const apps = Odac.core('Config').config.apps || []

    return containers.filter(c => {
      const name = this.#getContainerName(c)
      return websites[name] || apps.find(s => s.name === name)
    })
  }

  async #getFormattedContainers() {
    if (!Odac.server('Container').available) return []

    const containers = await Odac.server('Container').list()
    const websites = Odac.core('Config').config.websites || {}
    const apps = Odac.core('Config').config.apps || []

    return containers
      .filter(c => {
        const name = this.#getContainerName(c)
        return websites[name] || apps.find(s => s.name === name)
      })
      .map(c => this.#formatContainer(c, websites))
  }

  #formatContainer(c, websites) {
    const name = this.#getContainerName(c)
    const app = {
      type: websites[name] ? 'website' : 'app',
      framework: websites[name] ? 'odac' : c.image || 'unknown'
    }

    if (websites[name]) app.domain = name

    return {
      id: c.id,
      name,
      image: c.image,
      state: c.state,
      status: c.status,
      created: c.created,
      app,
      ports: (c.ports || []).map(p => ({
        private: p.PrivatePort,
        public: p.PublicPort,
        type: p.Type
      }))
    }
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
    if (intervals?.stats) this.statsInterval = intervals.stats
    if (intervals?.handshake) this.handshakeInterval = intervals.handshake

    log('Configuration updated: stats=%ds, handshake=%ds', this.statsInterval, this.handshakeInterval)
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
