class Server {
  #checkInterval = null

  async init() {
    Odac.core('Config').config.server.pid = process.pid
    Odac.core('Config').config.server.started = Date.now()
    Odac.server('App')
    Odac.server('DNS')
    Odac.server('Web')
    Odac.server('Mail')
    Odac.server('Api')
    Odac.server('Hub')
    Odac.server('Container')

    Odac.server('Updater').onReady(() => {
      Odac.server('Web').start()
      Odac.server('DNS').start()
      Odac.server('Hub').start()
      setTimeout(() => {
        Odac.server('Mail').start()
        Odac.server('Api').start()
      }, 1000)
    })

    setTimeout(() => {
      this.#checkInterval = setInterval(() => {
        Odac.server('App').check()
        Odac.server('SSL').check()
        Odac.server('Web').check()
        Odac.server('Mail').check()
        Odac.server('Hub').check()
      }, 1000)
    }, 1000)
  }

  stop() {
    // Stop check interval first to prevent services from restarting
    if (this.#checkInterval) {
      clearInterval(this.#checkInterval)
      this.#checkInterval = null
    }
    Odac.server('Web').stop()
    Odac.server('Mail').stop()
    Odac.server('DNS').stop()
    Odac.server('Api').stop()
    Odac.server('Hub').stop()
  }
}

module.exports = new Server()
