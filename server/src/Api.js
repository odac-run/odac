const net = require('net')
const nodeCrypto = require('crypto')
const fs = require('fs')
const os = require('os')
const path = require('path')

class Api {
  // Socket path inside this process (container or host)
  get #socketDir() {
    return path.join(os.homedir(), '.odac', 'run')
  }

  get socketPath() {
    return path.join(this.#socketDir, 'api.sock')
  }

  // Host path is same as internal path (resolved later by Container.js if needed)
  get hostSocketDir() {
    return this.#socketDir
  }
  #commands = {
    auth: (...args) => Odac.server('Hub').auth(...args),
    'app.start': (...args) => Odac.server('App').start(...args),
    'app.delete': (...args) => Odac.server('App').delete(...args),
    'app.list': (...args) => Odac.server('App').list(...args),
    'app.create': (...args) => Odac.server('App').create(...args),
    'mail.create': (...args) => Odac.server('Mail').create(...args),
    'mail.delete': (...args) => Odac.server('Mail').delete(...args),
    'mail.list': (...args) => Odac.server('Mail').list(...args),
    'mail.password': (...args) => Odac.server('Mail').password(...args),
    'mail.send': (...args) => Odac.server('Mail').send(...args),

    'server.stop': () => Odac.server('Server').stop(),
    'ssl.renew': (...args) => Odac.server('SSL').renew(...args),
    'subdomain.create': (...args) => Odac.server('Subdomain').create(...args),
    'subdomain.delete': (...args) => Odac.server('Subdomain').delete(...args),
    'subdomain.list': (...args) => Odac.server('Subdomain').list(...args),
    'web.create': (...args) => Odac.server('Web').create(...args),
    'web.delete': (...args) => Odac.server('Web').delete(...args),
    'web.list': (...args) => Odac.server('Web').list(...args)
  }
  #connections = {}
  #allowed = new Set()

  allow(ip) {
    this.#allowed.add(ip)
  }

  disallow(ip) {
    this.#allowed.delete(ip)
  }

  init() {
    if (!Odac.core('Config').config.api) Odac.core('Config').config.api = {}
    // Regenerate auth token every start
    Odac.core('Config').config.api.auth = nodeCrypto.randomBytes(32).toString('hex')

    const handleConnection = (socket, skipIpCheck = false) => {
      // IP check for TCP connections only
      if (!skipIpCheck && socket.remoteAddress) {
        const ip = socket.remoteAddress.replace(/^.*:/, '')
        Odac.core('Log').log('Api', `Incoming TCP connection from: ${ip}`)
        const isLocal = ip === '127.0.0.1' || ip === '::1'
        if (!isLocal && !this.#allowed.has(ip)) {
          Odac.core('Log').log('Api', `Blocking connection from unauthorized IP: ${ip}`)
          socket.destroy()
          return
        }
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

      socket.on('error', error => {
        if (error.code !== 'ECONNRESET') {
          Odac.core('Log').log('Api', `Socket error: ${error.message}`)
        }
        delete this.#connections[id]
      })

      socket.on('close', () => {
        delete this.#connections[id]
      })
    }

    // TCP Server for localhost/CLI only
    const tcpServer = net.createServer(socket => handleConnection(socket, false))
    tcpServer.listen(1453, '127.0.0.1')

    // Unix Socket Server for containers (bypasses network/firewall)
    const sockDir = this.#socketDir
    const sockPath = this.socketPath
    if (!fs.existsSync(sockDir)) {
      fs.mkdirSync(sockDir, {recursive: true})
    }
    if (fs.existsSync(sockPath)) {
      fs.unlinkSync(sockPath)
    }
    const socketServer = net.createServer(socket => handleConnection(socket, true))
    socketServer.listen(sockPath, () => {
      fs.chmodSync(sockPath, 0o666)
      Odac.core('Log').log('Api', `Unix socket listening at ${sockPath}`)
    })
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
