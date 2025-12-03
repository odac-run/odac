const {log} = Candy.core('Log', false).init('Hub')

const axios = require('axios')
const os = require('os')
const fs = require('fs')

class Hub {
  constructor() {
    setTimeout(() => {
      setInterval(() => {
        this.check()
      }, 60000)
    }, 1000)
  }

  async check() {
    const auth = Candy.core('Config').config.auth
    if (!auth || !auth.token) {
      return
    }

    try {
      const status = this.getSystemStatus()
      const response = await this.call('status', status)

      if (response.commands && response.commands.length > 0) {
        this.processCommands(response.commands)
      }
    } catch (error) {
      log('Failed to report status: %s', error)
    }
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

  processCommands(commands) {
    log('Processing %d commands', commands.length)
    for (const cmd of commands) {
      log('Command: %s', cmd.action)
    }
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
      Candy.core('Config').config.auth = {token: token, secret: secret}
      log('CandyPack authenticated!')
      return Candy.server('Api').result(true, __('Authentication successful'))
    } catch (error) {
      log('Authentication failed: %s', error ? error : 'Unknown error')
      return Candy.server('Api').result(false, error || __('Authentication failed'))
    }
  }

  call(action, data) {
    log('Hub API call: %s', action)
    return new Promise((resolve, reject) => {
      const url = 'https://hub.candypack.dev/' + action
      log('POST request to: %s', url)

      const headers = {}
      const auth = Candy.core('Config').config.auth
      if (auth && auth.token) {
        headers['Authorization'] = `Bearer ${auth.token}`
        headers['X-Secret'] = auth.secret
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
