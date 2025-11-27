const net = require('net')
const nodeCrypto = require('crypto')

class Api {
  #commands = {
    auth: (...args) => Candy.server('Hub').auth(...args),
    'mail.create': (...args) => Candy.server('Mail').create(...args),
    'mail.delete': (...args) => Candy.server('Mail').delete(...args),
    'mail.list': (...args) => Candy.server('Mail').list(...args),
    'mail.password': (...args) => Candy.server('Mail').password(...args),
    'mail.send': (...args) => Candy.server('Mail').send(...args),
    'service.start': (...args) => Candy.server('Service').start(...args),
    'service.delete': (...args) => Candy.server('Service').delete(...args),
    'server.stop': () => Candy.server('Server').stop(),
    'ssl.renew': (...args) => Candy.server('SSL').renew(...args),
    'subdomain.create': (...args) => Candy.server('Subdomain').create(...args),
    'subdomain.delete': (...args) => Candy.server('Subdomain').delete(...args),
    'subdomain.list': (...args) => Candy.server('Subdomain').list(...args),
    'web.create': (...args) => Candy.server('Web').create(...args),
    'web.delete': (...args) => Candy.server('Web').delete(...args),
    'web.list': (...args) => Candy.server('Web').list(...args)
  }
  #connections = {}

  init() {
    if (!Candy.core('Config').config.api) Candy.core('Config').config.api = {}
    // Regenerate auth token every start
    Candy.core('Config').config.api.auth = nodeCrypto.randomBytes(32).toString('hex')

    const server = net.createServer()

    server.on('connection', socket => {
      // Only allow localhost
      if (socket.remoteAddress !== '::ffff:127.0.0.1' && socket.remoteAddress !== '127.0.0.1') {
        socket.destroy()
        return
      }

      let id = Math.random().toString(36).substring(7)

      this.#connections[id] = socket

      socket.on('data', async raw => {
        let payload
        try {
          payload = JSON.parse(raw.toString())
        } catch {
          return socket.write(JSON.stringify(this.result(false, 'invalid_json')))
        }

        const {auth, action, data} = payload || {}
        if (!auth || auth !== Candy.core('Config').config.api.auth) {
          return socket.write(JSON.stringify({id, ...this.result(false, 'unauthorized')}))
        }
        if (!action || !this.#commands[action]) {
          return socket.write(JSON.stringify({id, ...this.result(false, 'unknown_action')}))
        }
        try {
          const result = await this.#commands[action](...(data ?? []), (process, status, message) => {
            this.send(id, process, status, message)
          })
          socket.write(JSON.stringify({id, ...result}))
          socket.destroy()
        } catch (err) {
          socket.write(JSON.stringify({id, ...this.result(false, err.message || 'error')}))
          socket.destroy()
        }
      })

      socket.on('close', () => {
        delete this.#connections[id]
      })
    })

    server.listen(1453)
  }

  send(id, process, status, message) {
    if (!this.#connections[id]) return
    return this.#connections[id].write(JSON.stringify({process, status, message}) + '\r\n')
  }

  result(result, message) {
    return {result, message}
  }
}

module.exports = new Api()
