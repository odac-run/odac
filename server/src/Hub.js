const {log} = Candy.core('Log', false).init('Hub')

const axios = require('axios')

class Hub {
  auth(code) {
    log('CandyPack authenticating...')
    let data = {code: code}
    this.call('auth', data)
      .then(response => {
        let token = response.token
        let secret = response.secret
        Candy.core('Config').config.auth = {token: token, secret: secret}
        log('CandyPack authenticated!')
      })
      .catch(error => {
        log(error ? error : 'CandyPack authentication failed!')
      })
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
