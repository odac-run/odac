class MiddlewareChain {
  constructor(route, middlewares) {
    this._route = route
    this._middlewares = middlewares
    this.auth = {
      page: (path, authFile, file) => this.authPage(path, authFile, file),
      post: (path, authFile, file) => this.authPost(path, authFile, file),
      get: (path, authFile, file) => this.authGet(path, authFile, file),
      ws: (path, handler, options) => this.authWs(path, handler, options)
    }
  }

  use(...middlewares) {
    this._middlewares.push(...middlewares.flat())
    return this
  }

  page(path, file) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.page(path, file)
    this._route._pendingMiddlewares = []
    return this
  }

  post(path, file, options) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.post(path, file, options)
    this._route._pendingMiddlewares = []
    return this
  }

  get(path, file, options) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.get(path, file, options)
    this._route._pendingMiddlewares = []
    return this
  }

  ws(path, handler, options) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.ws(path, handler, options)
    this._route._pendingMiddlewares = []
    return this
  }

  authPage(path, authFile, file) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.authPage(path, authFile, file)
    this._route._pendingMiddlewares = []
    return this
  }

  authPost(path, authFile, file) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.authPost(path, authFile, file)
    this._route._pendingMiddlewares = []
    return this
  }

  authGet(path, authFile, file) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.authGet(path, authFile, file)
    this._route._pendingMiddlewares = []
    return this
  }

  authWs(path, handler, options) {
    this._route._pendingMiddlewares = [...this._middlewares]
    this._route.authWs(path, handler, options)
    this._route._pendingMiddlewares = []
    return this
  }
}

module.exports = MiddlewareChain
