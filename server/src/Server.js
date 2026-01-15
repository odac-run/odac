class Server {
  constructor() {
    Odac.core('Config').config.server.pid = process.pid
    Odac.core('Config').config.server.started = Date.now()
    Odac.server('App')
    Odac.server('DNS')
    Odac.server('Web')
    Odac.server('Mail')
    Odac.server('Api')
    Odac.server('Hub')
    Odac.server('Container')
    setTimeout(function () {
      setInterval(function () {
        Odac.server('App').check()
        Odac.server('SSL').check()
        Odac.server('Web').check()
        Odac.server('Mail').check()
        Odac.server('Hub').check()
      }, 1000)
    }, 1000)
  }

  stop() {
    Odac.server('App').stopAll()
    Odac.server('Web').stopAll()
  }
}

module.exports = new Server()
