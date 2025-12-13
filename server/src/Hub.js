const {log} = Candy.core('Log', false).init('Hub')

const axios = require('axios')
const nodeCrypto = require('crypto')
const os = require('os')
const fs = require('fs')

class Hub {
  constructor() {
    this.websocket = null
    this.httpInterval = null
    this.websocketReconnectAttempts = 0
    this.maxReconnectAttempts = 5
    this.lastNetworkStats = null
    this.lastNetworkTime = null
    this.lastCpuStats = null

    this.startHttpPolling()
  }

  startHttpPolling() {
    if (this.httpInterval) {
      return
    }

    log('Starting HTTP polling (60s interval)')
    this.check()
    this.httpInterval = setInterval(() => {
      if (!this.websocket) {
        this.check()
      }
    }, 10000)
  }

  stopHttpPolling() {
    if (this.httpInterval) {
      log('Stopping HTTP polling')
      clearInterval(this.httpInterval)
      this.httpInterval = null
    }
  }

  async check() {
    const hub = Candy.core('Config').config.hub
    if (!hub || !hub.token) {
      return
    }

    try {
      const status = this.getSystemStatus()
      status.timestamp = Math.floor(Date.now() / 1000)

      const response = await this.call('status', status)

      if (!response.authenticated) {
        log('Server not authenticated: %s', response.reason || 'unknown')
        if (response.reason === 'token_invalid' || response.reason === 'signature_invalid') {
          log('Authentication credentials invalid, clearing config')
          delete Candy.core('Config').config.hub
        }
        return
      }

      if (response.websocket && !this.websocket) {
        log('WebSocket requested by cloud')
        this.connectWebSocket(response.websocketUrl, response.websocketToken)
      }
    } catch (error) {
      log('Failed to report status: %s', error)
    }
  }

  connectWebSocket(url, token) {
    if (this.websocket) {
      log('WebSocket already connected')
      return
    }

    try {
      const WebSocket = require('ws')
      const wsUrl = `${url}?token=${token}`

      log('Connecting to WebSocket: %s', url)
      this.websocket = new WebSocket(wsUrl, {
        rejectUnauthorized: true
      })

      this.websocket.on('open', () => {
        log('WebSocket connected')
        this.websocketReconnectAttempts = 0
        this.stopHttpPolling()
        this.sendWebSocketStatus()
      })

      this.websocket.on('message', data => {
        this.handleWebSocketMessage(data)
      })

      this.websocket.on('close', () => {
        log('WebSocket disconnected')
        this.websocket = null
        this.startHttpPolling()
      })

      this.websocket.on('error', error => {
        log('WebSocket error: %s', error.message)
      })
    } catch (error) {
      log('Failed to connect WebSocket: %s', error.message)
      this.websocket = null
      this.startHttpPolling()
    }
  }

  disconnectWebSocket() {
    if (this.websocket) {
      log('Disconnecting WebSocket')
      this.websocket.close()
      this.websocket = null
      this.startHttpPolling()
    }
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

  handleWebSocketMessage(data) {
    try {
      const message = JSON.parse(data.toString())

      if (message.type === 'disconnect') {
        log('Cloud requested disconnect: %s', message.reason || 'unknown')
        this.disconnectWebSocket()
        return
      }

      if (message.type === 'command') {
        if (this.verifyWebSocketMessage(message)) {
          this.processCommand(message.data)
        } else {
          log('WebSocket message verification failed')
        }
      }
    } catch (error) {
      log('Failed to handle WebSocket message: %s', error.message)
    }
  }

  signWebSocketMessage(message) {
    const hub = Candy.core('Config').config.hub
    if (!hub || !hub.secret) {
      return null
    }

    const payload = JSON.stringify({type: message.type, data: message.data, timestamp: message.timestamp})
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
    log('Processing command: %s', command.action)
  }

  getSystemStatus() {
    const memoryInfo = this.getMemoryUsage()
    const diskInfo = this.getDiskUsage()
    const networkInfo = this.getNetworkUsage()
    const servicesInfo = this.getServicesInfo()

    const serverStarted = Candy.core('Config').config.server.started
    const candypackUptime = serverStarted ? Math.floor((Date.now() - serverStarted) / 1000) : 0

    return {
      cpu: this.getCpuUsage(),
      memory: memoryInfo,
      disk: diskInfo,
      network: networkInfo,
      services: servicesInfo,
      uptime: candypackUptime,
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      node: process.version
    }
  }

  getServicesInfo() {
    try {
      const config = Candy.core('Config').config

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
    log('CandyPack authenticating...')
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
      let token = response.token
      let secret = response.secret
      log('Token received: %s...', token ? token.substring(0, 8) : 'none')
      Candy.core('Config').config.hub = {token: token, secret: secret}
      log('CandyPack authenticated!')
      return Candy.server('Api').result(true, __('Authentication successful'))
    } catch (error) {
      log('Authentication failed: %s', error ? error : 'Unknown error')
      return Candy.server('Api').result(false, error || __('Authentication failed'))
    }
  }

  signRequest(data) {
    const hub = Candy.core('Config').config.hub
    if (!hub || !hub.secret) {
      return null
    }

    const signature = nodeCrypto.createHmac('sha256', hub.secret).update(JSON.stringify(data)).digest('hex')

    return signature
  }

  call(action, data) {
    log('Hub API call: %s', action)
    return new Promise((resolve, reject) => {
      const url = 'https://hub.candypack.dev/' + action
      log('POST request to: %s', url)

      const headers = {}
      const hub = Candy.core('Config').config.hub
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
}

module.exports = new Hub()
