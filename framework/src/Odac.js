module.exports = {
  init: async function () {
    global.Odac = this.instance()
    await global.Odac.Env.init()
    await global.Odac.Config.init()
    await global.Odac.Mysql.init()
    await global.Odac.Route.init()
    await global.Odac.Server.init()
    global.Odac.instance = this.instance
    global.__ = value => {
      return value
    }
  },

  instance(id, req, res) {
    let _odac = {}

    _odac.Config = require('./Config.js')
    _odac.Env = require('./Env.js')
    _odac.Mail = (...args) => new (require('./Mail.js'))(...args)
    _odac.Mysql = require('./Mysql.js')
    _odac.Route = global.Odac?.Route ?? new (require('./Route.js'))()
    _odac.Server = require('./Server.js')
    _odac.Var = (...args) => new (require('./Var.js'))(...args)

    if (req) {
      _odac.Request = new (require('./Request.js'))(id, req, res, _odac)
      _odac.Auth = new (require('./Auth.js'))(_odac.Request)
      _odac.Token = new (require('./Token.js'))(_odac.Request)
      _odac.Lang = new (require('./Lang.js'))(_odac)
      if (res) {
        _odac.View = new (require('./View.js'))(_odac)
      }

      _odac._intervals = []
      _odac._timeouts = []
      _odac.setInterval = function (callback, delay, ...args) {
        const id = setInterval(callback, delay, ...args)
        _odac._intervals.push(id)
        return id
      }
      _odac.setTimeout = function (callback, delay, ...args) {
        const id = setTimeout(callback, delay, ...args)
        _odac._timeouts.push(id)
        return id
      }
      _odac.clearInterval = function (id) {
        const index = _odac._intervals.indexOf(id)
        if (index > -1) _odac._intervals.splice(index, 1)
        clearInterval(id)
      }
      _odac.clearTimeout = function (id) {
        const index = _odac._timeouts.indexOf(id)
        if (index > -1) _odac._timeouts.splice(index, 1)
        clearTimeout(id)
      }
      _odac.cleanup = function () {
        for (const id of _odac._intervals) clearInterval(id)
        for (const id of _odac._timeouts) clearTimeout(id)
        _odac._intervals = []
        _odac._timeouts = []
      }

      if (global.Odac?.Route?.class) {
        for (const name in global.Odac.Route.class) {
          const Module = global.Odac.Route.class[name].module
          _odac[name] = typeof Module === 'function' ? new Module(_odac) : Module
        }
      }

      _odac.__ = function (...args) {
        return _odac.Lang.get(...args)
      }
      _odac.abort = function (code) {
        return _odac.Request.abort(code)
      }
      _odac.cookie = function (key, value, options) {
        return _odac.Request.cookie(key, value, options)
      }
      _odac.direct = function (url) {
        return _odac.Request.redirect(url)
      }
      _odac.env = function (key, defaultValue) {
        return _odac.Env.get(key, defaultValue)
      }
      _odac.return = function (data) {
        return _odac.Request.end(data)
      }
      _odac.request = function (key) {
        return _odac.Request.request(key)
      }
      _odac.set = function (key, value) {
        return _odac.Request.set(key, value)
      }
      _odac.token = function (hash) {
        return hash ? _odac.Token.check(hash) : _odac.Token.generate()
      }
      _odac.validator = function () {
        return new (require('./Validator.js'))(_odac.Request)
      }
      _odac.write = function (value) {
        return _odac.Request.write(value)
      }
      _odac.stream = function (input) {
        _odac.Request.clearTimeout()
        return new (require('./Stream'))(_odac.Request.req, _odac.Request.res, input, _odac)
      }
    }

    return _odac
  }
}
