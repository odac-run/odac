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
        if (payload.message !== undefined || payload.data !== undefined) {
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
              if (payload.message) console.log(payload.message)
              if (Array.isArray(payload.data) && payload.data.length > 0) {
                this.#printTable(payload.data)
              } else if (typeof payload.data === 'object' && payload.data !== null) {
                console.dir(payload.data, {depth: null, colors: true})
              }
            } else {
              console.error(payload.message || 'Unknown error')
              if (payload.data) console.error(JSON.stringify(payload.data, null, 2))
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

  #printTable(data) {
    if (!data || data.length === 0) return

    // Pre-process data to format dates
    const formattedData = data.map(row => {
      const newRow = {...row}
      for (const key of Object.keys(newRow)) {
        const val = newRow[key]
        if (!val) continue

        const lowerKey = key.toLowerCase()
        const isDateKey = ['created', 'date', 'started', 'updated', 'time'].some(k => lowerKey.includes(k))

        // Check if explicit date key and looks like a timestamp (numeric)
        if (isDateKey && (typeof val === 'number' || (typeof val === 'string' && !isNaN(val))) && val > 0) {
          const ts = Number(val)
          // Heuristic: If < 1e11 (year 1973 in ms), assume seconds. JS Date.now() is > 1.7e12
          const date = new Date(ts > 1e11 ? ts : ts * 1000)

          if (!isNaN(date.getTime())) {
            const yyyy = date.getFullYear()
            const mm = String(date.getMonth() + 1).padStart(2, '0')
            const dd = String(date.getDate()).padStart(2, '0')
            const hh = String(date.getHours()).padStart(2, '0')
            const min = String(date.getMinutes()).padStart(2, '0')
            newRow[key] = `${yyyy}-${mm}-${dd} ${hh}:${min}`
          }
        }
      }
      return newRow
    })

    const headers = Object.keys(formattedData[0]).map(h => h.toUpperCase())
    const keys = Object.keys(formattedData[0])

    // 1. Calculate minimum required widths
    const minWidths = headers.map((h, i) => {
      const maxContent = Math.max(...formattedData.map(row => String(row[keys[i]] || '').length))
      return Math.max(h.length, maxContent) + 2 // +2 base padding
    })

    // 2. Calculate extra space to fill terminal width
    const termWidth = process.stdout.columns || 80
    const totalMinWidth = minWidths.reduce((a, b) => a + b, 0)

    let finalWidths = [...minWidths]
    if (termWidth > totalMinWidth) {
      const extraSpace = termWidth - totalMinWidth
      const perColumnExtra = Math.floor(extraSpace / headers.length)
      finalWidths = minWidths.map(w => w + perColumnExtra)
    }

    const buildRow = cells => cells.map((c, i) => String(c).padEnd(finalWidths[i])).join('')
    const separator = finalWidths.map(w => '-'.repeat(w)).join('')

    console.log(buildRow(headers))
    console.log(separator)
    formattedData.forEach(row => {
      console.log(
        buildRow(
          keys.map(k => {
            let val = row[k]
            if (Array.isArray(val)) val = val.join(', ')
            return val || '-'
          })
        )
      )
    })
    console.log('')
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
