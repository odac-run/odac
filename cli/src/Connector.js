const findProcess = require('find-process').default
const net = require('net')

class Connector {
  constructor() {
    this.socket = null
    this.connected = false
    this.connecting = false
    this.manualClose = false
  }

  #connect() {
    if (this.connected || this.connecting) return
    this.connecting = true
    this.socket = net.createConnection({port: 1453, host: '127.0.0.1'}, () => {
      this.connected = true
      this.connecting = false
    })

    this.socket.on('data', raw => {
      raw = raw.toString()
      if (raw.includes('\r\n')) {
        raw = raw.split('\r\n')
      } else {
        raw = [raw]
      }
      for (let payload of raw) {
        try {
          payload = JSON.parse(payload)
        } catch {
          continue
        }
        if (payload.message) {
          if (payload.status) {
            if (this.lastProcess == payload.process) {
              process.stdout.clearLine(0)
              process.stdout.cursorTo(0)
            } else {
              this.lastProcess = payload.process
              process.stdout.write('\n')
            }
            process.stdout.write(Odac.cli('Cli').icon(payload.status) + payload.message + '\r')
          } else {
            if (this.lastProcess) process.stdout.write('\n')
            if (payload.result) {
              console.log(payload.message)
            } else {
              console.error(payload.message)
            }
            if (!this.manualClose) this.socket.end()
            this.connected = false
            this.connecting = false
            this.lastProcess = null
          }
        }
      }
    })

    this.socket.on('error', err => {
      console.error('Socket error:', err.message)
    })
  }

  call(command) {
    if (!command) return
    this.manualClose = false
    this.#connect()
    this.socket.write(
      JSON.stringify({
        auth: Odac.core('Config').config.api.auth,
        action: command.action,
        data: command.data
      })
    )
  }

  check() {
    return new Promise(resolve => {
      // Try port check first (works in Docker)
      const socket = net.createConnection({port: 1453, host: '127.0.0.1'}, () => {
        socket.end()
        return resolve(true)
      })

      socket.on('error', () => {
        // Fallback to PID check (works on bare metal)
        if (!Odac.core('Config').config.server.watchdog) return resolve(false)
        findProcess('pid', Odac.core('Config').config.server.watchdog)
          .then(list => {
            if (list.length > 0 && list[0].name == 'node') return resolve(true)
            return resolve(false)
          })
          .catch(err => {
            console.error('Error checking process:', err)
            return resolve(false)
          })
      })

      // Timeout after 2 seconds
      setTimeout(() => {
        socket.destroy()
        resolve(false)
      }, 2000)
    })
  }
}

module.exports = new Connector()
