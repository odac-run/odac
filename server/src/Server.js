class Server {
  constructor() {
    Candy.core('Config').config.server.pid = process.pid
    Candy.core('Config').config.server.started = Date.now()
    Candy.server('Service')
    Candy.server('DNS')
    Candy.server('Web')
    Candy.server('Mail')
    Candy.server('Api')
    Candy.server('Hub')
    setTimeout(function () {
      setInterval(function () {
        Candy.server('Service').check()
        Candy.server('SSL').check()
        Candy.server('Web').check()
        Candy.server('Mail').check()
      }, 1000)
    }, 1000)
  }

  stop() {
    Candy.server('Service').stopAll()
    Candy.server('Web').stopAll()
  }
}

module.exports = new Server()
