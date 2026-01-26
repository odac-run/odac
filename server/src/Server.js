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

  stop(exceptWeb = false) {
    // Stop check interval first to prevent services from restarting
    if (this.#checkInterval) {
      clearInterval(this.#checkInterval)
      this.#checkInterval = null
    }
    // Stop non-web services first (they don't support SO_REUSEPORT)
    Odac.server('Mail').stop()
    Odac.server('DNS').stop()
    Odac.server('Api').stop()
    Odac.server('Hub').stop()

    // Web is stopped last (or not at all if exceptWeb=true)
    // This allows new container's Web to start BEFORE old one stops (SO_REUSEPORT)
    if (!exceptWeb) {
      Odac.server('Web').stop()
    }
  }
}

module.exports = new Server()
