module.exports = {
  init: async function () {
    global.Candy = this.instance()
    await global.Candy.Env.init()
    await global.Candy.Config.init()
    await global.Candy.Mysql.init()
    await global.Candy.Route.init()
    await global.Candy.Server.init()
    global.Candy.instance = this.instance
    global.__ = value => {
      return value
    }
  },

  instance(id, req, res) {
    let _candy = {}

    _candy.Config = require('./Config.js')
    _candy.Env = require('./Env.js')
    _candy.Mail = (...args) => new (require('./Mail.js'))(...args)
    _candy.Mysql = require('./Mysql.js')
    _candy.Route = global.Candy?.Route ?? new (require('./Route.js'))()
    _candy.Server = require('./Server.js')
    _candy.Var = (...args) => new (require('./Var.js'))(...args)

    if (req) {
      _candy.Request = new (require('./Request.js'))(id, req, res, _candy)
      _candy.Auth = new (require('./Auth.js'))(_candy.Request)
      _candy.Token = new (require('./Token.js'))(_candy.Request)
      _candy.Lang = new (require('./Lang.js'))(_candy)
      if (res) {
        _candy.View = new (require('./View.js'))(_candy)
      }

      _candy._intervals = []
      _candy._timeouts = []
      _candy.setInterval = function (callback, delay, ...args) {
        const id = setInterval(callback, delay, ...args)
        _candy._intervals.push(id)
        return id
      }
      _candy.setTimeout = function (callback, delay, ...args) {
        const id = setTimeout(callback, delay, ...args)
        _candy._timeouts.push(id)
        return id
      }
      _candy.clearInterval = function (id) {
        const index = _candy._intervals.indexOf(id)
        if (index > -1) _candy._intervals.splice(index, 1)
        clearInterval(id)
      }
      _candy.clearTimeout = function (id) {
        const index = _candy._timeouts.indexOf(id)
        if (index > -1) _candy._timeouts.splice(index, 1)
        clearTimeout(id)
      }
      _candy.cleanup = function () {
        for (const id of _candy._intervals) clearInterval(id)
        for (const id of _candy._timeouts) clearTimeout(id)
        _candy._intervals = []
        _candy._timeouts = []
      }

      if (global.Candy?.Route?.class) {
        for (const name in global.Candy.Route.class) {
          const Module = global.Candy.Route.class[name].module
          _candy[name] = typeof Module === 'function' ? new Module(_candy) : Module
        }
      }

      _candy.__ = function (...args) {
        return _candy.Lang.get(...args)
      }
      _candy.abort = function (code) {
        return _candy.Request.abort(code)
      }
      _candy.cookie = function (key, value, options) {
        return _candy.Request.cookie(key, value, options)
      }
      _candy.direct = function (url) {
        return _candy.Request.redirect(url)
      }
      _candy.env = function (key, defaultValue) {
        return _candy.Env.get(key, defaultValue)
      }
      _candy.return = function (data) {
        return _candy.Request.end(data)
      }
      _candy.request = function (key) {
        return _candy.Request.request(key)
      }
      _candy.set = function (key, value) {
        return _candy.Request.set(key, value)
      }
      _candy.token = function (hash) {
        return hash ? _candy.Token.check(hash) : _candy.Token.generate()
      }
      _candy.validator = function () {
        return new (require('./Validator.js'))(_candy.Request)
      }
      _candy.write = function (value) {
        return _candy.Request.write(value)
      }
      _candy.stream = function (input) {
        _candy.Request.clearTimeout()
        return new (require('./Stream'))(_candy.Request.req, _candy.Request.res, input, _candy)
      }
    }

    return _candy
  }
}
