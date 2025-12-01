const {log} = Candy.core('Log', false).init('Hub')

const axios = require('axios')
const os = require('os')
const fs = require('fs')

class Hub {
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
      axios
        .post(url, data)
        .then(response => {
          log('Response received for %s: success=%s', action, response.data.result.success)
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
            log('Error response data: %j', error.response.data)
            reject(error.response.data)
          } else {
            reject(error.message)
          }
        })
    })
  }
}

module.exports = new Hub()
