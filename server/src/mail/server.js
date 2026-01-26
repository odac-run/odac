const {log} = Odac.core('Log', false).init('Mail', 'Server')

const tls = require('tls')
const imap = require('./imap')

class server {
  clients
  options

  constructor(options) {
    this.options = options
  }

  /**
   * Start listening on the specified port with retry mechanism for EADDRINUSE.
   * Used during zero-downtime updates when the old container hasn't released the port yet.
   * @param {number} port - Port number to listen on (default: 993)
   * @param {number} maxRetries - Maximum retry attempts (default: 15)
   * @param {number} retryDelayMs - Delay between retries in milliseconds (default: 1000)
   */
  listen(port, maxRetries = 15, retryDelayMs = 1000) {
    if (!port) port = 993
    this.#port = port
    this.#maxRetries = maxRetries
    this.#retryDelayMs = retryDelayMs
    this.#attemptListen()
  }

  #port = 993
  #maxRetries = 15
  #retryDelayMs = 1000
  #retryCount = 0

  #attemptListen() {
    this.server = tls.createServer(this.options)
    this.server.on('connection', socket => {
      socket.on('error', err => {
        if (err.code !== 'ECONNRESET') log('Socket error: ' + err)
      })
    })
    this.server.on('secureConnection', socket => {
      log('New connection from ' + socket.remoteAddress)
      socket.id = Math.random().toString(36).substring(7)
      socket.write('* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] IMAP4rev1 Server Ready\r\n')
      let conn = new imap(socket, this)
      conn.listen()
    })
    this.server.on('error', err => {
      if (err.code === 'EADDRINUSE' && this.#retryCount < this.#maxRetries) {
        this.#retryCount++
        log(`IMAP port ${this.#port} in use. Retrying (${this.#retryCount}/${this.#maxRetries})...`)
        setTimeout(() => this.#attemptListen(), this.#retryDelayMs)
        return
      }

      if (err.code === 'EADDRINUSE') {
        log(`IMAP failed to bind port ${this.#port} after ${this.#maxRetries} retries`)
      } else {
        log('Server error: ' + err)
      }
      if (this.options.onError) this.options.onError(err)
    })
    this.server.listen(this.#port)
  }

  stop(cb) {
    if (this.server) {
      this.server.close(cb)
    } else if (cb) {
      cb()
    }
  }
}

module.exports = server
