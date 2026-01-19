const {log} = Odac.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const https = require('https')

const System = require('./Hub/System')
const {WebSocketClient, MessageSigner} = require('./Hub/WebSocket')

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
      onMessage: data => this.handleWebSocketMessage(data),
      onDisconnect: () => {}
    })
  }

  isAuthenticated() {
    return !!(Odac.core('Config').config.hub && Odac.core('Config').config.hub.token)
  }

  check() {
    this.checkCounter = this.checkCounter + 1
    if (this.checkCounter > 3600) this.checkCounter = 1

    const hub = Odac.core('Config').config.hub
    if (!hub || !hub.token) return

    if (!this.ws.connected) {
      if (this.ws.shouldReconnect()) {
        this.ws.connect('wss://hub.odac.run/ws', hub.token)
      }
      return
    }

    if (this.checkCounter % this.statsInterval === 0) {
      log('Sending container stats (Interval: %ds)...', this.statsInterval)
      this.sendContainerStats()
    }

    if (this.checkCounter % this.handshakeInterval === 0) {
      log('Sending initial handshake (Interval: %ds)...', this.handshakeInterval)
      this.sendInitialHandshake()
    }
  }

  async sendInitialHandshake() {
    if (!this.ws.connected) return

    let containers = []
    if (Odac.server('Container').available) {
      containers = await Odac.server('Container').list()
    }

    const websites = Odac.core('Config').config.websites || {}

    const apps = Odac.core('Config').config.apps || []

    const formattedContainers = containers
      .filter(c => {
        const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
        return websites[name] || apps.find(s => s.name === name)
      })
      .map(c => {
        const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
        const app = {
          type: 'app',
          framework: c.image || 'unknown'
        }

        // Check if it's a website
        if (websites[name]) {
          app.type = 'website'
          app.domain = name
          // Future: Check package.json for framework (next.js, nuxt, etc)
          app.framework = 'odac'
        }

        return {
          id: c.id,
          name: name,
          image: c.image,
          state: c.state,
          status: c.status,
          created: c.created,
          app: app,
          ports: (c.ports || []).map(p => ({
            private: p.PrivatePort,
            public: p.PublicPort,
            type: p.Type
          }))
        }
      })

    const system = System.getSystemInfo()

    const timestamp = Math.floor(Date.now() / 1000)
    const payload = {
      type: 'connect_info',
      data: {
        system: system,
        containers: formattedContainers
      },
      timestamp: timestamp
    }

    payload.signature = this.signWebSocketMessage({
      type: payload.type,
      data: payload.data,
      timestamp: timestamp
    })

    this.ws.send(payload)
  }

  sendWebSocketStatus() {
    if (!this.ws.connected) return

    const status = this.getSystemStatus()
    const timestamp = Math.floor(Date.now() / 1000)

    this.ws.send({
      type: 'status',
      data: status,
      timestamp: timestamp,
      signature: this.signWebSocketMessage({type: 'status', data: status, timestamp})
    })
  }

  async sendContainerStats() {
    if (!this.ws.connected) return
    if (!Odac.server('Container').available) return

    const containers = await Odac.server('Container').list()
    const websites = Odac.core('Config').config.websites || {}
    const apps = Odac.core('Config').config.apps || []

    // Filter relevant containers
    const relevantContainers = containers.filter(c => {
      const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
      return websites[name] || apps.find(s => s.name === name)
    })

    if (relevantContainers.length === 0) return

    const statsData = {}

    for (const c of relevantContainers) {
      const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
      const stats = await Odac.server('Container').getStats(c.id)
      if (stats) {
        statsData[name] = stats
      }
    }

    const timestamp = Math.floor(Date.now() / 1000)
    const payload = {
      type: 'container_stats',
      data: statsData,
      timestamp: timestamp
    }

    payload.signature = this.signWebSocketMessage({
      type: payload.type,
      data: payload.data,
      timestamp: timestamp
    })

    this.ws.send(payload)
  }

  handleWebSocketMessage(data) {
    try {
      const message = JSON.parse(data.toString())

      if (!this.verifyWebSocketMessage(message)) {
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

  signWebSocketMessage(message) {
    const hub = Odac.core('Config').config.hub
    return MessageSigner.sign(message, hub?.secret)
  }

  verifyWebSocketMessage(message) {
    const hub = Odac.core('Config').config.hub
    return MessageSigner.verify(message, hub?.secret)
  }

  processCommand(command) {
    if (!command || !command.action) {
      log('Invalid command structure received')
      return
    }

    log('Processing command: %s', command.action)

    switch (command.action) {
      case 'configure':
        this.#handleConfigure(command.payload)
        break
      default:
        log('Unknown command action: %s', command.action)
    }
  }

  #handleConfigure(payload) {
    if (!this.validateSchema(payload, {intervals: 'object'})) {
      log('Invalid configure payload')
      return
    }

    const {intervals} = payload
    if (intervals) {
      if (intervals.stats) this.statsInterval = intervals.stats
      if (intervals.handshake) this.handshakeInterval = intervals.handshake
      log('Configuration updated: stats=%ds, handshake=%ds', this.statsInterval, this.handshakeInterval)
    }
  }

  /**
   * Validates data against a simple schema
   * @param {Object} data - Data to validate
   * @param {Object} schema - Key-Type mapping (e.g. {token: 'string', count: 'number'})
   * @returns {boolean}
   */
  validateSchema(data, schema) {
    if (!data || typeof data !== 'object') return false

    for (const [key, type] of Object.entries(schema)) {
      if (data[key] === undefined) {
        log('Validation failed: Missing key %s', key)
        return false
      }
      if (typeof data[key] !== type) {
        log('Validation failed: Key %s expected %s, got %s', key, type, typeof data[key])
        return false
      }
    }
    return true
  }

  getSystemStatus() {
    return System.getStatus()
  }

  getLinuxDistro() {
    return System.getLinuxDistro()
  }

  async auth(code) {
    log('Odac authenticating...')
    log('Auth code received: %s', code ? code.substring(0, 8) + '...' : 'none')
    const packageJson = require('../../package.json')
    const distro = this.getLinuxDistro()

    let data = {
      code: code,
      os: os.platform(),
      arch: os.arch(),
      hostname: os.hostname(),
      version: packageJson.version,
      node: process.version
    }

    log('Auth data prepared: os=%s, arch=%s, hostname=%s, version=%s', data.os, data.arch, data.hostname, data.version)

    if (distro) {
      data.distro = distro
      log('Distro info added to auth data')
    }
    try {
      log('Calling hub API for authentication...')
      const response = await this.call('auth', data)

      if (!this.validateSchema(response, {token: 'string', secret: 'string'})) {
        throw new Error(__('Invalid authentication response format'))
      }

      let token = response.token
      let secret = response.secret
      log('Token received: %s...', token ? token.substring(0, 8) : 'none')
      Odac.core('Config').config.hub = {token: token, secret: secret}
      log('Odac authenticated!')
      return Odac.server('Api').result(true, __('Authentication successful'))
    } catch (error) {
      log('Authentication failed: %s', error ? error : 'Unknown error')
      return Odac.server('Api').result(false, error || __('Authentication failed'))
    }
  }

  signRequest(data) {
    const hub = Odac.core('Config').config.hub
    if (!hub || !hub.secret) {
      return null
    }

    const signature = nodeCrypto.createHmac('sha256', hub.secret).update(JSON.stringify(data)).digest('hex')

    return signature
  }

  call(action, data) {
    log('Hub API call: %s', action)
    return new Promise((resolve, reject) => {
      const url = 'https://hub.odac.run/' + action
      log('POST request to: %s', url)

      const headers = {}
      const hub = Odac.core('Config').config.hub
      if (hub && hub.token) {
        headers['Authorization'] = `Bearer ${hub.token}`
      }

      if (action !== 'auth' && data.timestamp) {
        const signature = this.signRequest(data)
        if (signature) {
          headers['X-Signature'] = signature
        }
      }

      axios
        .post(url, data, {
          headers,
          httpsAgent: this.agent
        })
        .then(response => {
          log('Raw response received for %s', action)
          log('Response structure: %j', {
            hasData: !!response.data,
            hasResult: !!(response.data && response.data.result),
            dataKeys: response.data ? Object.keys(response.data) : []
          })

          if (!response.data) {
            log('Response has no data')
            return reject('Invalid response: no data')
          }

          if (!response.data.result) {
            log('Response has no result field')
            return reject('Invalid response: no result field')
          }

          if (!response.data.result.success) {
            log('API returned error: %s', response.data.result.message)

            if (response.data.result.authenticated === false) {
              log('Authentication failed, returning result for handling')
              return resolve(response.data.result)
            }

            return reject(response.data.result.message)
          }

          log('API call successful: %s', action)
          resolve(response.data.data)
        })
        .catch(error => {
          log('API call failed: %s - %s', action, error.message)
          if (error.response) {
            log('Error response status: %s', error.response.status)
            log('Error response data: %j', error.response.data)
            reject(error.response.data)
          } else if (error.request) {
            log('No response received, request was made')
            reject('No response from server')
          } else {
            log('Request setup error: %s', error.message)
            reject(error.message)
          }
        })
    })
  }
  async getApp(name) {
    return this.call('app', {name: name})
  }
}

module.exports = new Hub()
