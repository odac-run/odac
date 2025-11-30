const {log} = Candy.core('Log', false).init('Hub')

const axios = require('axios')
const os = require('os')
const fs = require('fs')

class Hub {
  getLinuxDistro() {
    if (os.platform() !== 'linux') return null

    try {
      const osRelease = fs.readFileSync('/etc/os-release', 'utf8')
      const lines = osRelease.split('\n')
      const distro = {}

      for (const line of lines) {
        const [key, value] = line.split('=')
        if (key && value) {
          distro[key] = value.replace(/"/g, '')
        }
      }

      return {
        name: distro.NAME || distro.ID || 'Unknown',
        version: distro.VERSION_ID || distro.VERSION || 'Unknown',
        id: distro.ID || 'unknown'
      }
    } catch {
      return null
    }
  }

  async auth(code) {
    log('CandyPack authenticating...')
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

    if (distro) {
      data.distro = distro
    }
    try {
      const response = await this.call('auth', data)
      let token = response.token
      let secret = response.secret
      Candy.core('Config').config.auth = {token: token, secret: secret}
      log('CandyPack authenticated!')
      return Candy.server('Api').result(true, __('Authentication successful'))
    } catch (error) {
      log(error ? error : 'CandyPack authentication failed!')
      return Candy.server('Api').result(false, error || __('Authentication failed'))
    }
  }

  call(action, data) {
    return new Promise((resolve, reject) => {
      axios
        .post('https://hub.candypack.dev/' + action, data)
        .then(response => {
          if (!response.data.result.success) return reject(response.data.result.message)
          resolve(response.data.data)
        })
        .catch(error => {
          log(error.response.data)
          reject(error.response.data)
        })
    })
  }
}

module.exports = new Hub()
