const {log} = Odac.core('Log', false).init('Mail', 'Server')

const tls = require('tls')
const imap = require('./imap')

class server {
  clients
  options

  constructor(options) {
    this.options = options
  }

  listen(port) {
    if (!port) port = 993
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
      log('Server error: ' + err)
      if (this.options.onError) this.options.onError(err)
    })
    this.server.listen(port)
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
