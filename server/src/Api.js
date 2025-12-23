const net = require('net')
const nodeCrypto = require('crypto')

class Api {
  #commands = {
    auth: (...args) => Odac.server('Hub').auth(...args),
    'mail.create': (...args) => Odac.server('Mail').create(...args),
    'mail.delete': (...args) => Odac.server('Mail').delete(...args),
    'mail.list': (...args) => Odac.server('Mail').list(...args),
    'mail.password': (...args) => Odac.server('Mail').password(...args),
    'mail.send': (...args) => Odac.server('Mail').send(...args),
    'service.start': (...args) => Odac.server('Service').start(...args),
    'service.delete': (...args) => Odac.server('Service').delete(...args),
    'server.stop': () => Odac.server('Server').stop(),
    'ssl.renew': (...args) => Odac.server('SSL').renew(...args),
    'subdomain.create': (...args) => Odac.server('Subdomain').create(...args),
    'subdomain.delete': (...args) => Odac.server('Subdomain').delete(...args),
    'subdomain.list': (...args) => Odac.server('Subdomain').list(...args),
    'web.create': (...args) => Odac.server('Web').create(...args),
    'web.delete': (...args) => Odac.server('Web').delete(...args),
    'web.list': (...args) => Odac.server('Web').list(...args),
    'app.install': (...args) => Odac.server('App').install(...args),
    'app.delete': (...args) => Odac.server('App').delete(...args),
    'app.list': (...args) => Odac.server('App').list(...args)
  }
  #connections = {}

  init() {
    if (!Odac.core('Config').config.api) Odac.core('Config').config.api = {}
    // Regenerate auth token every start
    Odac.core('Config').config.api.auth = nodeCrypto.randomBytes(32).toString('hex')

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
        if (!auth || auth !== Odac.core('Config').config.api.auth) {
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
