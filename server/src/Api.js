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
    update: (...args) => Odac.server('Updater').start(...args),
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
  #tcpServer = null
  #unixServer = null
  #started = false
  #connectionHandler = null
  #clientTokens = new Map() // Token -> Domain

  addToken(domain) {
    if (!domain) return
    const token = this.generateToken(domain)
    this.#clientTokens.set(token, domain)
  }

  removeToken(domain) {
    if (!domain) return
    const token = this.generateToken(domain)
    this.#clientTokens.delete(token)
  }

  generateToken(domain) {
    return nodeCrypto.createHmac('sha256', Odac.core('Config').config.api.auth).update(domain).digest('hex')
  }

  allow(ip) {
    this.#allowed.add(ip)
  }

  disallow(ip) {
    this.#allowed.delete(ip)
  }

  init() {
    if (!Odac.core('Config').config.api) Odac.core('Config').config.api = {}
    // Only generate auth token if missing
    if (!Odac.core('Config').config.api.auth) {
      Odac.core('Config').config.api.auth = nodeCrypto.randomBytes(32).toString('hex')
    }

    // Pre-load all existing website tokens for O(1) lookup
    this.reloadTokens()

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

        // Auth Logic: Root vs Client
        let isRoot = false
        let clientDomain = null

        if (auth === Odac.core('Config').config.api.auth) {
          isRoot = true
        } else if (this.#clientTokens.has(auth)) {
          clientDomain = this.#clientTokens.get(auth)
        } else {
          return socket.write(JSON.stringify({id, ...this.result(false, 'unauthorized')}))
        }

        if (!action || !this.#commands[action]) {
          return socket.write(JSON.stringify({id, ...this.result(false, 'unknown_action')}))
        }

        // RBAC: Client restrictions
        if (!isRoot) {
          // Allow only specific commands for containers
          const allowed = ['mail.send']
          if (!allowed.includes(action)) {
            Odac.core('Log').warn('Api', `Blocked unauthorized action '${action}' from domain '${clientDomain}'`)
            return socket.write(JSON.stringify({id, ...this.result(false, 'permission_denied')}))
          }

          // Security: Inject Domain Identity into Payload for audit/logic if needed
          // For now, we trust the Action handler to be safe, but we log the context.
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

    this.#connectionHandler = handleConnection // Save for start()
  }

  start() {
    if (this.#started) return
    this.#started = true

    // TCP Server for localhost/CLI only
    const startTcpServer = () => {
      // Remove previous listeners to avoid duplicates on retry
      if (this.#tcpServer) {
        this.#tcpServer.removeAllListeners()
      }

      this.#tcpServer = net.createServer(socket => this.#connectionHandler(socket, false))

      this.#tcpServer.on('error', e => {
        if (e.code === 'EADDRINUSE') {
          // If port is busy, it implies the old container is still running.
          // We wait and retry until it hands over the port (Zero Downtime Handover).
          Odac.core('Log').log('Api', 'Port 1453 in use. Waiting for release...')
          setTimeout(startTcpServer, 1000)
        } else {
          Odac.core('Log').error('Api', `TCP Server error: ${e.message}`)
        }
      })

      this.#tcpServer.listen(1453, '127.0.0.1')
    }

    startTcpServer()

    // Unix Socket Server for containers (bypasses network/firewall)
    const sockDir = this.#socketDir
    const sockPath = this.socketPath
    if (!fs.existsSync(sockDir)) {
      fs.mkdirSync(sockDir, {recursive: true})
    }
    if (fs.existsSync(sockPath)) {
      try {
        fs.unlinkSync(sockPath)
      } catch (e) {
        Odac.core('Log').error('Api', `Failed to remove old socket: ${e.message}`)
      }
    }
    this.#unixServer = net.createServer(socket => this.#connectionHandler(socket, true))
    this.#unixServer.listen(sockPath, () => {
      fs.chmodSync(sockPath, 0o666)
      Odac.core('Log').log('Api', `Unix socket listening at ${sockPath}`)
      // Grant privileges to newly created socket
      // (Optional: chown if needed, but chmod 666 is usually enough for group access)
    })
  }

  stop() {
    try {
      if (this.#tcpServer) {
        this.#tcpServer.close()
        this.#tcpServer = null
      }
      if (this.#unixServer) {
        this.#unixServer.close()
        this.#unixServer = null
        // Clean up socket file
        if (fs.existsSync(this.socketPath)) {
          fs.unlinkSync(this.socketPath)
        }
      }
      this.#started = false
    } catch (e) {
      Odac.core('Log').error('Api', `Error stopping API services: ${e.message}`)
    }
  }

  reloadTokens() {
    this.#clientTokens.clear()
    const websites = Odac.core('Config').config.websites || {}
    for (const domain in websites) {
      this.addToken(domain)
    }
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
