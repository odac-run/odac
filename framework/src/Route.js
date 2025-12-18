const fs = require('fs')

const Cron = require('./Route/Cron.js')
const Internal = require('./Route/Internal.js')
const MiddlewareChain = require('./Route/Middleware.js')
const {WebSocketServer} = require('./WebSocket.js')

var routes2 = {}
const mime = {
  html: 'text/html',
  css: 'text/css',
  js: 'text/javascript',
  json: 'application/json',
  png: 'image/png',
  jpg: 'image/jpg',
  jpeg: 'image/jpeg',
  svg: 'image/svg+xml',
  ico: 'image/x-icon',
  mp3: 'audio/mpeg',
  mp4: 'video/mp4',
  webm: 'video/webm',
  woff: 'font/woff',
  woff2: 'font/woff2',
  ttf: 'font/ttf',
  otf: 'font/otf',
  eot: 'font/eot',
  pdf: 'application/pdf',
  zip: 'application/zip',
  tar: 'application/x-tar',
  gz: 'application/gzip',
  rar: 'application/x-rar-compressed',
  '7z': 'application/x-7z-compressed',
  txt: 'text/plain',
  log: 'text/plain',
  csv: 'text/csv',
  xml: 'text/xml',
  rss: 'application/rss+xml',
  atom: 'application/atom+xml',
  yaml: 'application/x-yaml',
  sh: 'application/x-sh',
  bat: 'application/x-bat',
  exe: 'application/x-exe',
  bin: 'application/x-binary',
  doc: 'application/msword',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  xls: 'application/vnd.ms-excel',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  ppt: 'application/vnd.ms-powerpoint',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  avi: 'video/x-msvideo',
  wmv: 'video/x-ms-wmv',
  flv: 'video/x-flv',
  webp: 'image/webp',
  gif: 'image/gif',
  bmp: 'image/bmp',
  tiff: 'image/tiff',
  tif: 'image/tiff',
  weba: 'audio/webm',
  wav: 'audio/wav',
  ogg: 'audio/ogg',
  flac: 'audio/flac',
  aac: 'audio/aac',
  midi: 'audio/midi'
}

class Route {
  loading = false
  routes = {}
  middlewares = {}
  _pendingMiddlewares = []
  #wsServer = new WebSocketServer()
  auth = {
    page: (path, authFile, file) => this.authPage(path, authFile, file),
    post: (path, authFile, file) => this.authPost(path, authFile, file),
    get: (path, authFile, file) => this.authGet(path, authFile, file),
    ws: (path, handler, options) => this.authWs(path, handler, options),
    use: (...middlewares) => new MiddlewareChain(this, [...middlewares.flat()])
  }

  async #runMiddlewares(Odac, middlewares) {
    if (!middlewares || middlewares.length === 0) return

    for (const mw of middlewares) {
      const middleware = typeof mw === 'function' ? mw : this.middlewares[mw]?.handler

      if (!middleware) {
        console.error(`Middleware not found: ${mw}`)
        return Odac.Request.abort(500)
      }

      const result = await middleware(Odac)

      if (result === false) {
        return Odac.Request.abort(403)
      }

      if (result !== undefined && result !== true) {
        return result
      }
    }
  }

  async #executeController(Odac, controller) {
    const middlewareResult = await this.#runMiddlewares(Odac, controller.middlewares)
    if (middlewareResult !== undefined) return middlewareResult

    if (controller.params) {
      for (let key in controller.params) {
        Odac.Request.data.url[key] = controller.params[key]
      }
    }

    if (typeof controller.cache === 'function') {
      return controller.cache(Odac)
    }
  }

  async check(Odac) {
    let url = Odac.Request.url.split('?')[0]
    if (url.endsWith('/')) url = url.slice(0, -1)

    if (url.startsWith('/_odac/')) {
      Odac.Request.route = '_odac_internal'
    }

    if (['post', 'put', 'patch', 'delete'].includes(Odac.Request.method)) {
      const formToken = await Odac.request('_odac_form_token')
      if (formToken) {
        await Internal.processForm(Odac)
      }
    }
    if (
      Odac.Request.url === '/' &&
      Odac.Request.method === 'get' &&
      Odac.Request.header('X-Odac') === 'token' &&
      Odac.Request.header('Referer').startsWith((Odac.Request.ssl ? 'https://' : 'http://') + Odac.Request.host + '/') &&
      Odac.Request.header('X-Odac-Client') === Odac.Request.cookie('odac_client')
    ) {
      Odac.Request.header('Access-Control-Allow-Origin', (Odac.Request.ssl ? 'https://' : 'http://') + Odac.Request.host)
      Odac.Request.header('Cache-Control', 'no-store, no-cache, must-revalidate, max-age=0')
      return {
        token: Odac.token()
      }
    }

    // Handle AJAX page load requests
    if (Odac.Request.method === 'get' && Odac.Request.header('X-Odac') === 'ajaxload') {
      let loadElements = Odac.Request.header('X-Odac-Load')
      if (loadElements) {
        Odac.Request.ajaxLoad = loadElements.split(',')
      }
      Odac.Request.isAjaxLoad = true
      Odac.Request.clientSkeleton = Odac.Request.header('X-Odac-Skeleton')
    }
    if (Odac.Config && Odac.Config.route && Odac.Config.route[url]) {
      Odac.Config.route[url] = Odac.Config.route[url].replace('${odac}', `${__dir}/node_modules/odac`)
      if (fs.existsSync(Odac.Config.route[url])) {
        let stat = fs.lstatSync(Odac.Config.route[url])
        if (stat.isFile()) {
          let type = 'text/html'
          if (Odac.Config.route[url].includes('.')) {
            let arr = Odac.Config.route[url].split('.')
            type = mime[arr[arr.length - 1]]
          }
          Odac.Request.header('Content-Type', type)
          Odac.Request.header('Cache-Control', 'public, max-age=31536000')
          Odac.Request.header('Content-Length', stat.size)
          return fs.readFileSync(Odac.Config.route[url])
        }
      }
    }
    for (let method of ['#' + Odac.Request.method, Odac.Request.method]) {
      let controller = this.#controller(Odac.Request.route, method, url)
      if (controller) {
        if (!method.startsWith('#') || (await Odac.Auth.check())) {
          Odac.Request.header('Cache-Control', 'no-store, no-cache, must-revalidate, max-age=0')
          if (
            ['post', 'get'].includes(Odac.Request.method) &&
            controller.token &&
            (!(await Odac.request('_token')) || !Odac.token(await Odac.Request.request('_token')))
          )
            return Odac.Request.abort(401)

          return await this.#executeController(Odac, controller)
        }
      }
    }
    let authPageController = this.#controller(Odac.Request.route, '#page', url)
    if (authPageController && (await Odac.Auth.check())) {
      Odac.Request.page = authPageController.cache?.file || authPageController.file
      return await this.#executeController(Odac, authPageController)
    }
    let pageController = this.#controller(Odac.Request.route, 'page', url)
    if (pageController) {
      Odac.Request.page = pageController.cache?.file || pageController.file
      return await this.#executeController(Odac, pageController)
    }
    if (url && !url.includes('/../') && fs.existsSync(`${__dir}/public${url}`)) {
      let stat = fs.lstatSync(`${__dir}/public${url}`)
      if (stat.isFile()) {
        let type = 'text/html'
        if (url.includes('.')) {
          let arr = url.split('.')
          type = mime[arr[arr.length - 1]]
        }
        Odac.Request.header('Content-Type', type)
        Odac.Request.header('Cache-Control', 'public, max-age=31536000')
        Odac.Request.header('Content-Length', stat.size)
        return fs.readFileSync(`${__dir}/public${url}`)
      }
    }
    return Odac.Request.abort(404)
  }

  #controller(route, method, url) {
    if (!this.routes[route] || !this.routes[route][method]) return false
    if (this.routes[route][method][url]) return this.routes[route][method][url]
    let arr = url.split('/')
    for (let key in this.routes[route][method]) {
      if (!key.includes('{') || !key.includes('}')) continue
      let route_arr = key.split('/')
      if (route_arr.length !== arr.length) continue
      let params = {}
      let next = false
      for (let i = 0; i < route_arr.length; i++) {
        if (route_arr[i].includes('{') && route_arr[i].includes('}')) {
          params[route_arr[i].replace('{', '').replace('}', '')] = arr[i]
          arr[i] = route_arr[i]
        } else if (route_arr[i] !== arr[i]) {
          next = true
          break
        }
      }
      if (next) continue
      if (arr.join('/') === key)
        return {
          params: params,
          cache: this.routes[route][method][key].cache,
          token: this.routes[route][method][key].token,
          middlewares: this.routes[route][method][key].middlewares
        }
    }
    return false
  }

  #loadMiddlewares() {
    const middlewareDir = `${__dir}/middleware/`
    if (!fs.existsSync(middlewareDir)) return

    for (const file of fs.readdirSync(middlewareDir)) {
      if (!file.endsWith('.js')) continue
      const name = file.replace('.js', '')
      const path = `${middlewareDir}${file}`
      const mtime = fs.statSync(path).mtimeMs

      if (this.middlewares[name] && this.middlewares[name].mtime >= mtime - 1000) continue

      delete require.cache[require.resolve(path)]
      this.middlewares[name] = {
        path,
        mtime,
        handler: require(path)
      }
    }
  }

  #init() {
    if (this.loading) return
    this.loading = true
    this.#loadMiddlewares()
    for (const file of fs.readdirSync(`${__dir}/controller/`)) {
      if (!file.endsWith('.js')) continue
      let name = file.replace('.js', '')
      if (!Odac.Route.class) Odac.Route.class = {}
      if (Odac.Route.class[name]) {
        if (Odac.Route.class[name].mtime >= fs.statSync(Odac.Route.class[name].path).mtimeMs + 1000) continue
        delete require.cache[require.resolve(Odac.Route.class[name].path)]
      }
      Odac.Route.class[name] = {
        path: `${__dir}/controller/${file}`,
        mtime: fs.statSync(`${__dir}/controller/${file}`).mtimeMs,
        module: require(`${__dir}/controller/${file}`)
      }
    }
    let dir = fs.readdirSync(`${__dir}/route/`)
    for (const file of dir) {
      if (!file.endsWith('.js')) continue
      let mtime = fs.statSync(`${__dir}/route/${file}`).mtimeMs
      Odac.Route.buff = file.replace('.js', '')
      if (!routes2[Odac.Route.buff] || routes2[Odac.Route.buff] < mtime - 1000) {
        delete require.cache[require.resolve(`${__dir}/route/${file}`)]
        routes2[Odac.Route.buff] = mtime
        require(`${__dir}/route/${file}`)
      }
      for (const type of ['page', '#page', 'post', '#post', 'get', '#get', 'error']) {
        if (!this.routes[Odac.Route.buff]) continue
        if (!this.routes[Odac.Route.buff][type]) continue
        for (const route in this.routes[Odac.Route.buff][type]) {
          if (routes2[Odac.Route.buff] > this.routes[Odac.Route.buff][type][route].loaded) {
            delete require.cache[require.resolve(this.routes[Odac.Route.buff][type][route].path)]
            delete this.routes[Odac.Route.buff][type][route]
          } else if (this.routes[Odac.Route.buff][type][route]) {
            if (typeof this.routes[Odac.Route.buff][type][route].type === 'function') continue
            if (this.routes[Odac.Route.buff][type][route].mtime < fs.statSync(this.routes[Odac.Route.buff][type][route].path).mtimeMs) {
              delete require.cache[require.resolve(this.routes[Odac.Route.buff][type][route].path)]
              this.routes[Odac.Route.buff][type][route].cache = require(this.routes[Odac.Route.buff][type][route].path)
              this.routes[Odac.Route.buff][type][route].mtime = fs.statSync(this.routes[Odac.Route.buff][type][route].path).mtimeMs
            }
          }
        }
      }
      delete Odac.Route.buff
    }
    Cron.init()
    this.loading = false
  }

  init() {
    this.#init()
    this.#registerInternalRoutes()
    setInterval(() => {
      this.#init()
    }, 5000)
  }

  #registerInternalRoutes() {
    if (!Odac.Route) Odac.Route = {}
    Odac.Route.buff = '_odac_internal'

    this.set(
      'POST',
      '/_odac/register',
      async Odac => {
        const csrfToken = await Odac.request('_token')
        if (!csrfToken || !Odac.token(csrfToken)) {
          return Odac.Request.abort(401)
        }
        return await Internal.register(Odac)
      },
      {token: true}
    )

    this.set(
      'POST',
      '/_odac/login',
      async Odac => {
        const csrfToken = await Odac.request('_token')
        if (!csrfToken || !Odac.token(csrfToken)) {
          return Odac.Request.abort(401)
        }
        return await Internal.login(Odac)
      },
      {token: true}
    )

    this.set(
      ['POST', 'GET', 'PUT', 'PATCH', 'DELETE'],
      '/_odac/form',
      async Odac => {
        const csrfToken = await Odac.request('_token')
        if (!csrfToken || !Odac.token(csrfToken)) {
          return Odac.Request.abort(401)
        }
        const result = await Internal.customForm(Odac)
        if (result !== null) return result

        return Odac.return({
          result: {
            success: false,
            message: 'No handler defined for this form'
          },
          errors: {_odac_form: 'Form action not configured'}
        })
      },
      {token: true}
    )

    delete Odac.Route.buff
  }

  async request(req, res) {
    let id = `${Date.now()}${Math.random().toString(36).substr(2, 9)}`
    let param = Odac.instance(id, req, res)
    if (!this.routes[param.Request.route]) return param.Request.end()
    try {
      let result = this.check(param)
      if (result instanceof Promise) result = await result
      const Stream = require('./Stream.js')
      if (result instanceof Stream) return
      if (param.Request.res.finished || param.Request.res.writableEnded) {
        param.cleanup()
        return
      }
      if (result) param.Request.end(result)
      await param.View.print(param)
      param.Request.print(param)
      param.cleanup()
    } catch (e) {
      console.error(e)
      param.Request.abort(500)
      param.cleanup()
      return param.Request.end()
    }
  }

  use(...middlewares) {
    return new MiddlewareChain(this, [...middlewares.flat()])
  }

  set(type, url, file, options = {}) {
    if (Array.isArray(type)) {
      type = type.map(t => t.toLowerCase())
      for (const t of type) {
        this.set(t, url, file, options)
      }
      return this
    }

    if (!options) options = {}
    if (typeof url !== 'string') url = String(url)
    if (url.length && url.endsWith('/')) url = url.slice(0, -1)

    type = type.toLowerCase()

    const isFunction = typeof file === 'function'
    let path = `${__dir}/route/${Odac.Route.buff}.js`

    if (!isFunction && file) {
      path = `${__dir}/controller/${type.replace('#', '')}/${file}.js`
      if (typeof file === 'string' && file.includes('.')) {
        let arr = file.split('.')
        path = `${__dir}/controller/${arr[0]}/${type.replace('#', '')}/${arr.slice(1).join('.')}.js`
      }
    }

    if (!this.routes[Odac.Route.buff]) this.routes[Odac.Route.buff] = {}
    if (!this.routes[Odac.Route.buff][type]) this.routes[Odac.Route.buff][type] = {}

    if (this.routes[Odac.Route.buff][type][url]) {
      this.routes[Odac.Route.buff][type][url].loaded = routes2[Odac.Route.buff]
      if (!isFunction && this.routes[Odac.Route.buff][type][url].mtime < fs.statSync(path).mtimeMs) {
        delete this.routes[Odac.Route.buff][type][url]
        delete require.cache[require.resolve(path)]
      } else return this
    }

    if (isFunction || fs.existsSync(path)) {
      if (!this.routes[Odac.Route.buff][type][url]) this.routes[Odac.Route.buff][type][url] = {}
      this.routes[Odac.Route.buff][type][url].cache = isFunction ? file : require(path)
      this.routes[Odac.Route.buff][type][url].type = isFunction ? 'function' : 'controller'
      this.routes[Odac.Route.buff][type][url].file = file
      this.routes[Odac.Route.buff][type][url].mtime = isFunction ? Date.now() : fs.statSync(path).mtimeMs
      this.routes[Odac.Route.buff][type][url].path = path
      this.routes[Odac.Route.buff][type][url].loaded = routes2[Odac.Route.buff]
      this.routes[Odac.Route.buff][type][url].token = options.token ?? true
      this.routes[Odac.Route.buff][type][url].middlewares = this._pendingMiddlewares.length > 0 ? [...this._pendingMiddlewares] : undefined
    }

    return this
  }

  page(path, file) {
    if (typeof file === 'object' && !Array.isArray(file)) {
      this.set('page', path, _odac => {
        _odac.View.set(file)
        return
      })
      return this
    }
    if (file) this.set('page', path, file)
    return this
  }

  post(path, file, options) {
    this.set('post', path, file, options)
    return this
  }

  get(path, file, options) {
    this.set('get', path, file, options)
    return this
  }

  authPage(path, authFile, file) {
    if (typeof authFile === 'object' && !Array.isArray(authFile)) {
      this.set('#page', path, _odac => {
        _odac.View.set(authFile)
        return
      })
      if (typeof file === 'object' && !Array.isArray(file)) {
        this.set('page', path, _odac => {
          _odac.View.set(file)
          return
        })
      }
      return this
    }
    if (authFile) this.set('#page', path, authFile)
    if (file) {
      if (typeof file === 'object' && !Array.isArray(file)) {
        this.set('page', path, _odac => {
          _odac.View.set(file)
          return
        })
      } else {
        this.set('page', path, file)
      }
    }
    return this
  }

  authPost(path, authFile, file) {
    if (authFile) this.set('#post', path, authFile)
    if (file) this.post(path, file)
    return this
  }

  authGet(path, authFile, file) {
    if (authFile) this.set('#get', path, authFile)
    if (file) this.get(path, file)
    return this
  }

  error(code, file) {
    this.set('error', code, file)
  }

  cron(controller) {
    return Cron.job(controller)
  }

  ws(path, handler, options = {}) {
    this.setWs('ws', path, handler, options)
    return this
  }

  authWs(path, handler, options = {}) {
    this.setWs('#ws', path, handler, options)
    return this
  }

  setWs(type, path, handler, options = {}) {
    const middlewares = this._pendingMiddlewares.length > 0 ? [...this._pendingMiddlewares] : undefined
    this._pendingMiddlewares = []

    const {token = true} = options
    const requireAuth = type === '#ws'

    const wrappedHandler = async (ws, Odac) => {
      Odac.ws = ws

      ws.on('close', () => {
        if (Odac.cleanup && typeof Odac.cleanup === 'function') {
          Odac.cleanup()
        }
      })

      if (requireAuth) {
        const isAuthenticated = await Odac.Auth.check()
        if (!isAuthenticated) {
          ws.close(4001, 'Unauthorized')
          return
        }
      }

      if (token) {
        const wsToken = Odac.Request._wsHeaders ? Odac.Request._wsHeaders['sec-websocket-protocol'] : null
        const tokens = wsToken ? wsToken.split(', ') : []
        const odacToken = tokens.find(t => t.startsWith('odac-token-'))

        if (!odacToken) {
          ws.close(4002, 'Missing token')
          return
        }

        const tokenValue = odacToken.replace('odac-token-', '')
        if (!Odac.token(tokenValue)) {
          ws.close(4002, 'Invalid token')
          return
        }
      }

      if (middlewares) {
        for (const mw of middlewares) {
          const middleware = typeof mw === 'function' ? mw : this.middlewares[mw]?.handler

          if (!middleware) {
            console.error(`Middleware not found: ${mw}`)
            ws.close(4000, 'Internal error')
            return
          }

          const result = await middleware(Odac)

          if (result === false) {
            ws.close(4003, 'Forbidden')
            return
          }

          if (result !== undefined && result !== true) {
            ws.close(4000, 'Middleware rejected')
            return
          }
        }
      }
      return handler(Odac)
    }

    this.#wsServer.route(path, wrappedHandler)
  }

  handleWebSocketUpgrade(req, socket, head, Odac) {
    this.#wsServer.handleUpgrade(req, socket, head, Odac)
  }

  get wsServer() {
    return this.#wsServer
  }
}

module.exports = Route
