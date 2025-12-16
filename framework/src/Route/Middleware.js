class MiddlewareChain {
  constructor(route, middlewares) {
    this._route = route
    this._middlewares = middlewares
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
}

module.exports = MiddlewareChain
