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
    const totalMem = os.totalmem()
    const freeMem = os.freemem()

    return {
      cpu: this.getCpuUsage(),
      memory: {
        used: totalMem - freeMem,
        total: totalMem
      },
      uptime: os.uptime(),
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      node: process.version
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

    const idle = totalIdle / cpus.length
    const total = totalTick / cpus.length
    const usage = 100 - ~~((100 * idle) / total)

    return usage
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

            if (response.data.data && response.data.data.authenticated === false) {
              log('Authentication failed, returning data for handling')
              return resolve(response.data.data)
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
