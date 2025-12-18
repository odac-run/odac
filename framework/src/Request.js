const nodeCrypto = require('crypto')

class OdacRequest {
  #odac
  #complete = false
  #cookies = {received: [], sent: []}
  data = {post: {}, get: {}, url: {}}
  #event = {data: [], end: []}
  #headers = {Server: 'Odac'}
  #status = 200
  #timeout = null
  #earlyHints = null
  variables = {}
  isAjaxLoad = false
  ajaxLoad = null
  clientSkeleton = null
  page = null

  constructor(id, req, res, odac) {
    this.id = id
    this.req = req
    this.res = res
    this.#odac = odac
    this.method = req.method.toLowerCase()
    this.url = req.url
    this.host = req.headers.host
    this.ssl = this.header('x-odac-connection-ssl') === 'true'
    this.ip = (this.header('x-odac-connection-remoteaddress') ?? req.connection.remoteAddress).replace('::ffff:', '')
    delete this.req.headers['x-odac-connection-ssl']
    delete this.req.headers['x-odac-connection-remoteaddress']
    let route = req.headers.host.split('.')[0]
    if (!Odac.Route.routes[route]) route = 'www'
    this.route = route
    if (this.res) {
      this.#timeout = setTimeout(() => !this.res.finished && this.abort(408), Odac.Config.request.timeout)
    }
    this.#data()
    if (!Odac.Request) Odac.Request = {}
    if (!this.cookie('odac_client') || !this.session('_client') || this.session('_client') !== this.cookie('odac_client')) {
      let client = nodeCrypto
        .createHash('md5')
        .update(this.ip + this.id + Date.now().toString() + Math.random().toString())
        .digest('hex')
      this.cookie('odac_client', client, {expires: null, httpOnly: false})
      this.session('_client', client)
    }
  }

  // - ABORT REQUEST
  async abort(code) {
    this.status(code)
    let result = {401: 'Unauthorized', 404: 'Not Found', 408: 'Request Timeout'}[code] ?? null
    if (Odac.Route.routes[this.route].error && Odac.Route.routes[this.route].error[code])
      result = await Odac.Route.routes[this.route].error[code].cache(this.#odac)
    this.end(result)
  }

  // - SET COOKIE
  cookie(key, value, options = {}) {
    if (value === undefined) {
      if (this.#cookies.sent[key]) return this.#cookies.sent[key]
      if (this.#cookies.received[key]) return this.#cookies.received[key]
      value =
        this.req.headers.cookie
          ?.split('; ')
          .find(c => c.startsWith(key + '='))
          ?.split('=')[1] ?? null
      if (value && value.startsWith('{') && value.endsWith('}')) value = JSON.parse(value)
      this.#cookies.received[key] = value
      return value
    }
    if (options.path === undefined) options.path = '/'
    if (options.expires === undefined) options.expires = new Date(Date.now() + 1000 * 60 * 60 * 24 * 365).toUTCString()
    if (options.secure === undefined) options.secure = true
    if (options.httpOnly === undefined) options.httpOnly = true
    if (options.sameSite === undefined) options.sameSite = 'Strict'
    if (typeof value === 'object') value = JSON.stringify(value)
    let cookie = `${key}=${value}`
    for (const option of Object.keys(options)) if (options[option]) cookie += `; ${option}=${options[option]}`
    this.#cookies.sent.push(cookie)
  }

  #data() {
    let split = this.url.split('?')
    if (split[1]) {
      let data = split[1].split('&')
      for (let i = 0; i < data.length; i++) {
        if (data[i].indexOf('=') === -1) continue
        let key = data[i].split('=')[0]
        let val = data[i].split('=')[1]
        this.data.get[key] = val
      }
    }
    let body = ''
    this.req.on('data', chunk => {
      body += chunk.toString()
      if (body.length > 1e6) {
        body = ''
        this.status(413)
        this.end()
      } else {
        if (body.length > 0 && body.indexOf('Content-Disposition') === -1) return
        if (body.indexOf('Content-Disposition') > -1) {
          let boundary = body.split('\r\n')[0].split('; ')[1].split('=')[1]
          let data = body.split(boundary)
          for (let i = 0; i < data.length; i++) {
            if (data[i].indexOf('Content-Disposition') === -1) continue
            let key = data[i].split('name="')[1].split('"')[0]
            let val = data[i].split('\r\n\r\n')[1].split('\r\n')[0]
            this.data.post[key] = val
          }
        } else {
          let data = body.split('&')
          for (let i = 0; i < data.length; i++) {
            if (data[i].indexOf('=') === -1) continue
            let key = decodeURIComponent(data[i].split('=')[0])
            let val = decodeURIComponent(data[i].split('=')[1] || '')
            this.data.post[key] = val
          }
        }
      }
      for (const event of this.#event.data) {
        event.callback(event.active ? chunk : body)
        event.active = true
      }
    })
    this.req.on('end', () => {
      if (!body) return (this.#complete = true)
      if (body.startsWith('{') && body.endsWith('}')) {
        this.data.post = JSON.parse(body)
      } else {
        let data = body.split('&')
        for (let i = 0; i < data.length; i++) {
          if (data[i].indexOf('=') === -1) continue
          let key = decodeURIComponent(data[i].split('=')[0])
          let val = decodeURIComponent(data[i].split('=')[1] || '')
          this.data.post[key] = val
        }
      }
      this.#complete = true
      for (const event of this.#event.end) event.callback()
    })
  }

  // - RETURN REQUEST
  end(data) {
    if (data instanceof Promise) return data.then(result => this.end(result))
    if (this.res.finished) return
    if (typeof data === 'object' && data !== null && data.type !== 'Buffer') {
      let json = JSON.stringify(data)
      if (json.length > 0 && JSON.parse(json).type !== 'Buffer') {
        data = json
        this.header('Content-Type', 'application/json')
      }
    }
    clearTimeout(this.#timeout)
    this.print()
    this.res.end(data)
    this.req.connection.destroy()
  }

  // - GET
  get(key) {
    return this.variables[key] ? this.variables[key].value : null
  }

  // - SET HEADER
  header(key, value) {
    if (value === null) delete this.#headers[key]
    else if (value !== undefined) this.#headers[key] = value
    else for (const header of Object.keys(this.req.headers)) if (header.toLowerCase() === key.toLowerCase()) return this.req.headers[header]
  }

  // - ON EVENT
  on(event, callback) {
    if (this.#event[event]) this.#event[event].push({callback: callback, active: false})
    else return false
  }

  // - PRINT HEADERS
  print() {
    if (this.res.headersSent) return

    if (this.#earlyHints && this.#earlyHints.length > 0 && global.Odac?.View?.EarlyHints) {
      global.Odac.View.EarlyHints.send(this.res, this.#earlyHints)
    }

    this.#headers['Set-Cookie'] = this.#cookies.sent
    this.res.writeHead(this.#status, this.#headers)
  }

  // - SET EARLY HINTS
  setEarlyHints(hints) {
    this.#earlyHints = hints
  }

  // - HAS EARLY HINTS
  hasEarlyHints() {
    return this.#earlyHints !== null && this.#earlyHints.length > 0
  }

  // - REDIRECT
  redirect(url) {
    this.header('Location', url)
    this.status(302)
  }

  // - GET REQUEST
  async request(key, method) {
    if (method) method = method.toUpperCase()
    if ((!method || method === 'post') && (this.data.post[key] ?? null)) return this.data.post[key]
    if ((!method || method === 'get') && (this.data.get[key] ?? null)) return this.data.get[key]
    if ((!method || method === 'url') && (this.data.url[key] ?? null)) return this.data.url[key]
    return new Promise(resolve => {
      let interval = setInterval(() => {
        if (this.data.post[key] !== undefined || this.data.get[key] !== undefined || this.#complete) {
          clearInterval(interval)
          if (!key && !method) resolve(this.data.post)
          else if (!key && method) resolve(this.data[method.toLowerCase()])
          else if (this.data.post[key] !== undefined && method !== 'GET') resolve(this.data.post[key])
          else if (this.data.get[key] !== undefined && method !== 'GET') resolve(this.data.get[key])
          else resolve()
        }
      }, 10)
    })
  }

  // - SESSION
  session(key, value) {
    if (!Odac.Request.session) Odac.Request.session = {}
    if (!Odac.Request.sessionLocks) Odac.Request.sessionLocks = {}

    let pri = nodeCrypto
      .createHash('md5')
      .update(this.req.headers['user-agent'] ?? '.')
      .digest('hex')
    let pub = this.cookie('candy_session')

    if (!pub || !Odac.Request.session[pub + '-' + pri]) {
      const lockKey = `${this.ip}-${pri}`
      const now = Date.now()

      if (Odac.Request.sessionLocks[lockKey]) {
        const lock = Odac.Request.sessionLocks[lockKey]
        if (now - lock.timestamp < 5000 && Odac.Request.session[`${lock.sessionId}-${pri}`]) {
          pub = lock.sessionId
        } else {
          delete Odac.Request.sessionLocks[lockKey]
        }
      }

      if (!pub) {
        const sessionLockValues = Object.values(Odac.Request.sessionLocks)
        const activeSessions = new Set(sessionLockValues.map(l => l.sessionId))
        do {
          pub = nodeCrypto
            .createHash('md5')
            .update(this.ip + this.id + Date.now().toString() + Math.random().toString())
            .digest('hex')
        } while (Odac.Request.session[`${pub}-${pri}`] || activeSessions.has(pub))

        Odac.Request.sessionLocks[lockKey] = {sessionId: pub, timestamp: now}
        Odac.Request.session[`${pub}-${pri}`] = {}
        this.cookie('candy_session', `${pub}`)

        setTimeout(() => {
          if (Odac.Request.sessionLocks[lockKey]?.timestamp === now) {
            delete Odac.Request.sessionLocks[lockKey]
          }
        }, 5000)
      }
    }

    if (!Odac.Request.session[pub + '-' + pri]) Odac.Request.session[pub + '-' + pri] = {}
    if (value === undefined) return Odac.Request.session[pub + '-' + pri][key] ?? null
    else if (value === null) delete Odac.Request.session[pub + '-' + pri][key]
    else Odac.Request.session[pub + '-' + pri][key] = value
  }

  // - SET
  set(key, value, ajax = false) {
    if (typeof key === 'object') for (const k in key) this.variables[k] = {value: key[k], ajax: ajax}
    else this.variables[key] = {value: value, ajax: ajax}
  }

  // - HTTP CODE
  status(code) {
    this.#status = code
  }

  // - CLEAR TIMEOUT (for long-running connections like SSE)
  clearTimeout() {
    clearTimeout(this.#timeout)
  }

  // - WRITE DATA
  write(data) {
    if (this.res.finished) return
    this.res.write(data)
  }
}

module.exports = OdacRequest
