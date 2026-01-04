const {log} = Odac.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const fs = require('fs')

class Hub {
  constructor() {
    this.websocket = null
    this.websocketReconnectAttempts = 0
    this.maxReconnectAttempts = 5
    this.lastNetworkStats = null
    this.lastNetworkTime = null
    this.lastCpuStats = null
    this.checkCounter = 0
    this.nextReconnectTime = 0
    this.statsInterval = 60
    this.handshakeInterval = 60
  }

  isAuthenticated() {
    return !!(Odac.core('Config').config.hub && Odac.core('Config').config.hub.token)
  }

  check() {
    this.checkCounter = this.checkCounter + 1

    // Reset counter to avoid overflow, though highly unlikely with basic ints
    if (this.checkCounter > 3600) this.checkCounter = 1

    const hub = Odac.core('Config').config.hub
    if (!hub || !hub.token) {
      return
    }

    if (!this.websocket) {
      if (Date.now() >= this.nextReconnectTime) {
        this.nextReconnectTime = Date.now() + 5000 + Math.floor(Math.random() * 15000)
        this.connectWebSocket('wss://hub.odac.run/ws', hub.token)
      }
      return
    }

    // Dynamic stats interval
    if (this.checkCounter % this.statsInterval === 0) {
      log('Sending container stats (Interval: %ds)...', this.statsInterval)
      this.sendContainerStats()
    }

    // Dynamic handshake/full info interval
    if (this.checkCounter % this.handshakeInterval === 0) {
      log('Sending initial handshake (Interval: %ds)...', this.handshakeInterval)
      this.sendInitialHandshake()
    }
  }

  connectWebSocket(url, token) {
    if (this.websocket) {
      log('WebSocket already connected')
      return
    }

    try {
      const WebSocket = require('ws')

      log('Connecting to WebSocket: %s', url)
      this.websocket = new WebSocket(url, {
        rejectUnauthorized: true,
        headers: {
          Authorization: `Bearer ${token}`
        }
      })

      this.websocket.on('open', () => {
        log('WebSocket connected')
        this.websocketReconnectAttempts = 0
        this.sendInitialHandshake()
      })

      this.websocket.on('message', data => {
        this.handleWebSocketMessage(data)
      })

      this.websocket.on('close', () => {
        log('WebSocket disconnected')
        this.websocket = null
        // Reconnect after random delay (5-20s) to prevent thundering herd
        this.nextReconnectTime = Date.now() + 5000 + Math.floor(Math.random() * 15000)
      })

      this.websocket.on('error', error => {
        log('WebSocket error: %s', error.message)
      })
    } catch (error) {
      log('Failed to connect WebSocket: %s', error.message)
      this.websocket = null
    }
  }

  disconnectWebSocket() {
    if (this.websocket) {
      log('Disconnecting WebSocket')
      this.websocket.close()
      this.websocket = null
    }
  }

  async sendInitialHandshake() {
    if (!this.websocket || this.websocket.readyState !== 1) {
      return
    }

    let containers = []
    if (Odac.server('Container').available) {
      containers = await Odac.server('Container').list()
    }

    const websites = Odac.core('Config').config.websites || {}

    const services = Odac.core('Config').config.services || []

    const formattedContainers = containers
      .filter(c => {
        const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
        return websites[name] || services.find(s => s.name === name)
      })
      .map(c => {
        const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
        const app = {
          type: 'service',
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

    const cpus = os.cpus()
    const system = {
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      release: os.release(),
      uptime: os.uptime(),
      load: os.loadavg(),
      memory: {
        total: Math.floor(os.totalmem() / 1024),
        free: Math.floor(os.freemem() / 1024)
      },
      cpu: {
        count: cpus.length,
        model: cpus.length > 0 ? cpus[0].model : 'unknown'
      },
      container_engine: Odac.server('Container').available
    }

    if (os.platform() === 'linux') {
      const distro = this.getLinuxDistro()
      if (distro && distro.name) {
        system.release = `${distro.name} ${distro.version}`
      }
    }

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

    this.websocket.send(JSON.stringify(payload))
  }

  sendWebSocketStatus() {
    if (!this.websocket || this.websocket.readyState !== 1) {
      return
    }

    const status = this.getSystemStatus()
    const timestamp = Math.floor(Date.now() / 1000)

    const message = {
      type: 'status',
      data: status,
      timestamp: timestamp,
      signature: this.signWebSocketMessage({type: 'status', data: status, timestamp})
    }

    this.websocket.send(JSON.stringify(message))
  }

  async sendContainerStats() {
    if (!this.websocket || this.websocket.readyState !== 1) {
      return
    }

    if (!Odac.server('Container').available) {
      return
    }

    const containers = await Odac.server('Container').list()
    const websites = Odac.core('Config').config.websites || {}
    const services = Odac.core('Config').config.services || []

    // Filter relevant containers
    const relevantContainers = containers.filter(c => {
      const name = c.names && c.names.length > 0 ? c.names[0].replace(/^\//, '') : 'unknown'
      return websites[name] || services.find(s => s.name === name)
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

    if (this.websocket && this.websocket.readyState === 1) {
      this.websocket.send(JSON.stringify(payload))
    }
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
        this.disconnectWebSocket()
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
    if (!hub || !hub.secret) {
      return null
    }

    const payload = JSON.stringify({type: message.type, data: message.data, timestamp: message.timestamp})
    // DEBUG: Log the payload being signed to debug HMAC issues
    // log('Signing payload: %s', payload)
    return nodeCrypto.createHmac('sha256', hub.secret).update(payload).digest('hex')
  }

  verifyWebSocketMessage(message) {
    const {type, data, timestamp, signature} = message

    if (!signature || !timestamp) {
      log('Missing signature or timestamp in WebSocket message')
      return false
    }

    const now = Math.floor(Date.now() / 1000)
    if (Math.abs(now - timestamp) > 300) {
      log('WebSocket message timestamp too old or in future')
      return false
    }

    const expectedSignature = this.signWebSocketMessage({type, data, timestamp})
    if (signature !== expectedSignature) {
      log('Invalid WebSocket message signature')
      return false
    }

    return true
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
    const memoryInfo = this.getMemoryUsage()
    const diskInfo = this.getDiskUsage()
    const networkInfo = this.getNetworkUsage()
    const servicesInfo = this.getServicesInfo()

    const serverStarted = Odac.core('Config').config.server.started
    const odacUptime = serverStarted ? Math.floor((Date.now() - serverStarted) / 1000) : 0

    return {
      cpu: this.getCpuUsage(),
      memory: memoryInfo,
      disk: diskInfo,
      network: networkInfo,
      services: servicesInfo,
      uptime: odacUptime,
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      node: process.version
    }
  }

  getServicesInfo() {
    try {
      const config = Odac.core('Config').config

      const websites = config.websites ? Object.keys(config.websites).length : 0
      const services = config.services ? config.services.length : 0
      const mailAccounts = config.mail && config.mail.accounts ? Object.keys(config.mail.accounts).length : 0

      return {
        websites: websites,
        services: services,
        mail: mailAccounts
      }
    } catch (error) {
      log('Failed to get services info: %s', error.message)
      return {
        websites: 0,
        services: 0,
        mail: 0
      }
    }
  }

  getMemoryUsage() {
    const totalMem = os.totalmem()

    if (os.platform() === 'darwin') {
      try {
        const {execSync} = require('child_process')
        const output = execSync('vm_stat', {encoding: 'utf8'})
        const lines = output.split('\n')

        let pageSize = 4096
        let pagesActive = 0
        let pagesWired = 0
        let pagesCompressed = 0

        for (const line of lines) {
          if (line.includes('page size of')) {
            pageSize = parseInt(line.match(/(\d+)/)[1])
          } else if (line.includes('Pages active')) {
            pagesActive = parseInt(line.match(/:\s*(\d+)/)[1])
          } else if (line.includes('Pages wired down')) {
            pagesWired = parseInt(line.match(/:\s*(\d+)/)[1])
          } else if (line.includes('Pages occupied by compressor')) {
            pagesCompressed = parseInt(line.match(/:\s*(\d+)/)[1])
          }
        }

        const usedMem = (pagesActive + pagesWired + pagesCompressed) * pageSize

        return {
          used: usedMem,
          total: totalMem
        }
      } catch (error) {
        log('Failed to get macOS memory usage: %s', error.message)
      }
    }

    const freeMem = os.freemem()
    return {
      used: totalMem - freeMem,
      total: totalMem
    }
  }

  getDiskUsage() {
    try {
      const {execSync} = require('child_process')
      let command

      if (os.platform() === 'win32') {
        command = 'wmic logicaldisk get size,freespace,caption'
      } else {
        command = "df -k / | tail -1 | awk '{print $2,$3}'"
      }

      const output = execSync(command, {encoding: 'utf8'})

      if (os.platform() === 'win32') {
        const lines = output.trim().split('\n')
        if (lines.length > 1) {
          const parts = lines[1].trim().split(/\s+/)
          const free = parseInt(parts[1]) || 0
          const total = parseInt(parts[2]) || 0
          return {
            used: total - free,
            total: total
          }
        }
      } else {
        const parts = output.trim().split(/\s+/)
        const total = parseInt(parts[0]) * 1024
        const used = parseInt(parts[1]) * 1024
        return {
          used: used,
          total: total
        }
      }
    } catch (error) {
      log('Failed to get disk usage: %s', error.message)
    }

    return {
      used: 0,
      total: 0
    }
  }

  getNetworkUsage() {
    try {
      const {execSync} = require('child_process')
      let command

      if (os.platform() === 'win32') {
        command = 'netstat -e'
      } else if (os.platform() === 'darwin') {
        command = "netstat -ib | grep -e 'en0' | head -1 | awk '{print $7,$10}'"
      } else {
        command = "cat /proc/net/dev | grep -E 'eth0|ens|enp' | head -1 | awk '{print $2,$10}'"
      }

      const output = execSync(command, {encoding: 'utf8', timeout: 5000})
      let currentStats = {received: 0, sent: 0}

      if (os.platform() === 'win32') {
        const lines = output.split('\n')
        for (const line of lines) {
          if (line.includes('Bytes')) {
            const parts = line.trim().split(/\s+/)
            currentStats.received = parseInt(parts[1]) || 0
            currentStats.sent = parseInt(parts[2]) || 0
            break
          }
        }
      } else {
        const parts = output.trim().split(/\s+/)
        currentStats.received = parseInt(parts[0]) || 0
        currentStats.sent = parseInt(parts[1]) || 0
      }

      const now = Date.now()

      if (this.lastNetworkStats && this.lastNetworkTime) {
        const timeDiff = (now - this.lastNetworkTime) / 1000
        const receivedDiff = currentStats.received - this.lastNetworkStats.received
        const sentDiff = currentStats.sent - this.lastNetworkStats.sent

        if (receivedDiff < 0 || sentDiff < 0 || timeDiff <= 0) {
          this.lastNetworkStats = currentStats
          this.lastNetworkTime = now
          return {download: 0, upload: 0}
        }

        const bandwidth = {
          download: Math.max(0, Math.round(receivedDiff / timeDiff)),
          upload: Math.max(0, Math.round(sentDiff / timeDiff))
        }

        this.lastNetworkStats = currentStats
        this.lastNetworkTime = now

        return bandwidth
      }

      this.lastNetworkStats = currentStats
      this.lastNetworkTime = now

      return {
        download: 0,
        upload: 0
      }
    } catch (error) {
      log('Failed to get network usage: %s', error.message)
    }

    return {
      download: 0,
      upload: 0
    }
  }

  getCpuUsage() {
    const cpus = os.cpus()
    let totalIdle = 0
    let totalTick = 0

    for (const cpu of cpus) {
      for (const type in cpu.times) {
        totalTick += cpu.times[type]
      }
      totalIdle += cpu.times.idle
    }

    const currentStats = {
      idle: totalIdle,
      total: totalTick
    }

    if (!this.lastCpuStats) {
      this.lastCpuStats = currentStats
      return 0
    }

    const idleDiff = currentStats.idle - this.lastCpuStats.idle
    const totalDiff = currentStats.total - this.lastCpuStats.total

    if (idleDiff < 0 || totalDiff <= 0) {
      this.lastCpuStats = currentStats
      return 0
    }

    const usage = 100 - ~~((100 * idleDiff) / totalDiff)

    this.lastCpuStats = currentStats

    return Math.max(0, Math.min(100, usage))
  }

  getLinuxDistro() {
    log('Getting Linux distro info...')
    if (os.platform() !== 'linux') {
      log('Platform is not Linux: %s', os.platform())
      return null
    }

    try {
      log('Reading /etc/os-release...')
      const osRelease = fs.readFileSync('/etc/os-release', 'utf8')
      const lines = osRelease.split('\n')
      const distro = {}

      for (const line of lines) {
        const [key, value] = line.split('=')
        if (key && value) {
          distro[key] = value.replace(/"/g, '')
        }
      }

      const result = {
        name: distro.NAME || distro.ID || 'Unknown',
        version: distro.VERSION_ID || distro.VERSION || 'Unknown',
        id: distro.ID || 'unknown'
      }
      log('Distro detected: %s %s', result.name, result.version)
      return result
    } catch (err) {
      log('Failed to read distro info: %s', err.message)
      return null
    }
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
          httpsAgent: new (require('https').Agent)({
            rejectUnauthorized: true
          })
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
