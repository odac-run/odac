const nodeCrypto = require('crypto')
const fs = require('fs')
const os = require('os')

module.exports = {
  auth: {
    key: 'id',
    token: 'candy_auth'
  },
  request: {
    timeout: 10000
  },
  encrypt: {
    key: 'odac'
  },
  earlyHints: {
    enabled: true,
    auto: true,
    maxResources: 5
  },

  init: function () {
    try {
      this.system = JSON.parse(fs.readFileSync(os.homedir() + '/.odac/config.json'))
    } catch {
      this.system = {}
    }

    if (fs.existsSync(__dir + '/config.json')) {
      let config = {}
      try {
        config = JSON.parse(fs.readFileSync(__dir + '/config.json'))
        config = this._interpolate(config)
      } catch (err) {
        console.error('Error reading config file:', __dir + '/config.json', err.message)
      }
      this._deepMerge(this, config)
    }
    this.encrypt.key = nodeCrypto.createHash('md5').update(this.encrypt.key).digest('hex')
  },

  _interpolate: function (obj) {
    if (typeof obj === 'string') {
      return obj.replace(/\$\{(\w+)\}/g, (_, key) => {
        // Special variables
        if (key === 'odac') {
          return __dirname.replace(/\/framework\/src$/, '')
        }
        // Environment variables
        return process.env[key] || ''
      })
    }
    if (Array.isArray(obj)) {
      return obj.map(item => this._interpolate(item))
    }
    if (obj && typeof obj === 'object') {
      const result = {}
      for (const key of Object.keys(obj)) {
        result[key] = this._interpolate(obj[key])
      }
      return result
    }
    return obj
  },

  _deepMerge: function (target, source) {
    for (const key of Object.keys(source)) {
      if (source[key] && typeof source[key] === 'object' && !Array.isArray(source[key])) {
        // Ensure target[key] is also an object before recursive merge
        if (!target[key] || typeof target[key] !== 'object' || Array.isArray(target[key])) {
          target[key] = {}
        }
        this._deepMerge(target[key], source[key])
      } else {
        target[key] = source[key]
      }
    }
  }
}
