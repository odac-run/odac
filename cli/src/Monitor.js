require('../../core/Odac.js')

const fs = require('fs')
const os = require('os')

class Monitor {
  #current = ''
  #domains = []
  #height
  #logs = {content: [], mtime: null, selected: null, watched: []}
  #logging = false
  #modules = ['api', 'config', 'dns', 'hub', 'mail', 'server', 'service', 'ssl', 'subdomain', 'web', 'app']
  #printing = false
  #selected = 0
  #services = []
  #activeApps = []
  #watch = []
  #websites = {}
  #apps = {}
  #width

  constructor() {
    process.stdout.write(process.platform === 'win32' ? `title Odac Debug\n` : `\x1b]2;Odac Debug\x1b\x5c`)
  }

  async debug() {
    await this.#debug()
    setInterval(() => this.#debug(), 250)

    process.stdout.write('\x1b[?25l')
    process.stdout.write('\x1b[?1000h')
    process.stdin.setRawMode(true)
    process.stdin.setEncoding('utf8')
    process.stdin.on('data', chunk => {
      const buffer = Buffer.from(chunk)
      if (buffer.length >= 6 && buffer[0] === 0x1b && buffer[1] === 0x5b && buffer[2] === 0x4d) {
        // Mouse wheel up
        if (buffer[3] === 96) {
          if (this.#selected > 0) {
            this.#selected--
            this.#debug()
          }
        }
        // Mouse wheel down
        if (buffer[3] === 97) {
          if (this.#selected + 1 < this.#modules.length) {
            this.#selected++
            this.#debug()
          }
        }

        // Mouse click
        if (buffer[3] === 32) {
          const btn = buffer[3] - 32
          if (btn === 0 || btn === 1) {
            const x = buffer[4] - 32
            const y = buffer[5] - 32
            let c1 = (this.#width / 12) * 3
            if (c1 % 1 != 0) c1 = Math.floor(c1)
            if (c1 > 50) c1 = 50
            if (x > 1 && x < c1 && y < this.#height - 4) {
              if (this.#modules[y - 2]) {
                this.#selected = y - 2
                let index = this.#watch.indexOf(this.#selected)
                if (index > -1) this.#watch.splice(index, 1)
                else this.#watch.push(this.#selected)
                this.#debug()
              }
            }
          }
        }
      }

      // Ctrl+C
      if (buffer.length === 1 && buffer[0] === 3) {
        process.stdout.write('\x1b[?25h')
        process.stdout.write('\x1b[?1000l')
        process.stdout.write('\x1Bc')
        process.exit(0)
      }
      // Enter
      if (buffer.length === 1 && buffer[0] === 13) {
        let index = this.#watch.indexOf(this.#selected)
        if (index > -1) this.#watch.splice(index, 1)
        else this.#watch.push(this.#selected)
        this.#debug()
      }
      // Up/Down arrow keys
      if (buffer.length === 3 && buffer[0] === 27 && buffer[1] === 91) {
        if (buffer[2] === 65 && this.#selected > 0) this.#selected-- // up
        if (buffer[2] === 66 && this.#selected + 1 < this.#modules.length) this.#selected++ // down
        this.#debug()
      }
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
    })
  }

  #debug() {
    if (this.#printing) return
    this.#printing = true
    this.#width = process.stdout.columns - 3
    this.#height = process.stdout.rows
    this.#loadModuleLogs()
    let c1 = (this.#width / 12) * 3
    if (c1 % 1 != 0) c1 = Math.floor(c1)
    if (c1 > 50) c1 = 50
    let result = ''
    result += Odac.cli('Cli').color('┌', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
    let title = Odac.cli('Cli').color(__('Modules'), null)
    result += ' ' + Odac.cli('Cli').color(title) + ' '
    result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
    result += Odac.cli('Cli').color('┬', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
    title = Odac.cli('Cli').color(__('Logs'), null)
    result += ' ' + Odac.cli('Cli').color(title) + ' '
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1 - title.length - 7), 'gray')
    result += Odac.cli('Cli').color('┐\n', 'gray')
    for (let i = 0; i < this.#height - 3; i++) {
      if (this.#modules[i]) {
        result += Odac.cli('Cli').color('│', 'gray')
        result += Odac.cli('Cli').color(
          '[' + (this.#watch.includes(i) ? 'X' : ' ') + '] ',
          i == this.#selected ? 'blue' : 'white',
          i == this.#selected ? 'white' : null,
          i == this.#selected ? 'bold' : null
        )
        result += Odac.cli('Cli').color(
          Odac.cli('Cli').spacing(this.#modules[i] ? this.#modules[i] : '', c1 - 4),
          i == this.#selected ? 'blue' : 'white',
          i == this.#selected ? 'white' : null,
          i == this.#selected ? 'bold' : null
        )
        result += Odac.cli('Cli').color('│', 'gray')
      } else {
        result += Odac.cli('Cli').color('│', 'gray')
        result += ' '.repeat(c1)
        result += Odac.cli('Cli').color('│', 'gray')
      }
      result += Odac.cli('Cli').spacing(this.#logs.content[i] ? this.#logs.content[i] : ' ', this.#width - c1)
      result += Odac.cli('Cli').color('│\n', 'gray')
    }
    result += Odac.cli('Cli').color('└', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Odac.cli('Cli').color('┴', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┘\n', 'gray')
    let shortcuts = '↑/↓ ' + __('Navigate') + ' | ↵ ' + __('Select') + ' | Ctrl+C ' + __('Exit')
    result += Odac.cli('Cli').color(' ODAC', 'magenta', 'bold')
    result += Odac.cli('Cli').color(Odac.cli('Cli').spacing(shortcuts, this.#width + 1 - 'ODAC'.length, 'right'), 'gray')
    if (result !== this.#current) {
      this.#current = result
      process.stdout.write('\x1Bc')
      process.stdout.write(result)
    }
    this.#printing = false
  }

  async #load() {
    if (this.#logging) return
    this.#logging = true
    this.#logs.selected = this.#selected
    let file = null

    const domainsCount = this.#domains.length
    const servicesCount = this.#services.length

    if (this.#selected < domainsCount) {
      file = os.homedir() + '/.odac/logs/' + this.#domains[this.#selected] + '.log'
    } else if (this.#selected < domainsCount + servicesCount) {
      file = os.homedir() + '/.odac/logs/' + this.#services[this.#selected - domainsCount].name + '.log'
    } else {
      this.#logging = false
      return
    }

    let log = ''
    let mtime = null
    if (file && fs.existsSync(file)) {
      mtime = fs.statSync(file).mtime
      if (this.#selected == this.#logs.selected && mtime == this.#logs.mtime) return
      log = fs.readFileSync(file, 'utf8')
    }
    this.#logs.content = log
      .trim()
      .replace(/\r\n/g, '\n')
      .split('\n')
      .map(line => {
        if ('[LOG]' == line.substring(0, 5)) {
          line = line.substring(5)
          let date = parseInt(line.substring(1, 14))
          line = Odac.cli('Cli').color('[' + Odac.cli('Cli').formatDate(new Date(date)) + ']', 'green', 'bold') + line.substring(15)
        } else if ('[ERR]' == line.substring(0, 5)) {
          line = line.substring(5)
          let date = parseInt(line.substring(1, 14))
          line = Odac.cli('Cli').color('[' + Odac.cli('Cli').formatDate(new Date(date)) + ']', 'red', 'bold') + line.substring(15)
        }
        return line
      })
      .slice(-this.#height + 4)
    this.#logs.mtime = mtime
    this.#logging = false
  }

  async #loadModuleLogs() {
    if (this.#logging) return
    this.#logging = true

    if (this.#watch.length === 0) {
      this.#logs.content = []
      this.#logging = false
      return
    }

    const file = os.homedir() + '/.odac/logs/.odac.log'
    let log = ''
    let mtime = null

    if (fs.existsSync(file)) {
      mtime = fs.statSync(file).mtime
      if (JSON.stringify(this.#watch) === JSON.stringify(this.#logs.watched) && mtime == this.#logs.mtime) {
        this.#logging = false
        return
      }
      log = fs.readFileSync(file, 'utf8')
    }

    const selectedModules = this.#watch.map(index => this.#modules[index])
    this.#logs.content = log
      .trim()
      .replace(/\r\n/g, '\n')
      .split('\n')
      .map(line => {
        const lowerCaseLine = line.toLowerCase()
        const moduleName = selectedModules.find(name => lowerCaseLine.includes(`[${name}]`.toLowerCase()))
        return {line, moduleName}
      })
      .filter(item => item.moduleName)
      .map(item => {
        let {line, moduleName} = item
        if ('[LOG]' == line.substr(0, 5) || '[ERR]' == line.substr(0, 5)) {
          const isError = '[ERR]' == line.substr(0, 5)
          const date = line.substr(6, 24)
          const originalMessage = line.slice(34 + moduleName.length)
          const cleanedMessage = originalMessage.trim()
          const dateColor = isError ? 'red' : 'green'

          line =
            Odac.cli('Cli').color('[' + Odac.cli('Cli').formatDate(new Date(date)) + ']', dateColor, 'bold') +
            Odac.cli('Cli').color(`[${moduleName}]`, 'white', 'bold') +
            ' ' +
            cleanedMessage
        }
        return line
      })
      .slice(-this.#height + 4)

    this.#logs.mtime = mtime
    this.#logs.watched = [...this.#watch]
    this.#logging = false
  }

  monit() {
    this.#monitor()
    setInterval(() => this.#monitor(), 250)

    // Mouse event handler
    process.stdout.write('\x1b[?25l')
    process.stdout.write('\x1b[?1000h')
    process.stdin.setRawMode(true)
    process.stdin.setEncoding('utf8')
    process.stdin.on('data', chunk => {
      const buffer = Buffer.from(chunk)

      const totalItems = this.#domains.length + this.#services.length + this.#activeApps.length

      if (buffer.length >= 6 && buffer[0] === 0x1b && buffer[1] === 0x5b && buffer[2] === 0x4d) {
        // Mouse wheel up
        if (buffer[3] === 96) {
          if (this.#selected > 0) {
            this.#selected--
            this.#monitor()
          }
        }
        // Mouse wheel down
        if (buffer[3] === 97) {
          if (this.#selected + 1 < totalItems) {
            this.#selected++
            this.#monitor()
          }
        }

        // Mouse click
        if (buffer[3] === 32) {
          const btn = buffer[3] - 32
          if (btn === 0 || btn === 1) {
            const x = buffer[4] - 32
            const y = buffer[5] - 32
            let c1 = (this.#width / 12) * 3
            if (c1 % 1 != 0) c1 = Math.floor(c1)
            if (c1 > 50) c1 = 50
            if (x > 1 && x < c1 && y < this.#height - 4) {
              const clickedIndex = y - 2
              let targetIndex = -1
              let currentLine = 0

              if (this.#domains.length > 0) {
                if (clickedIndex >= currentLine && clickedIndex < currentLine + this.#domains.length) {
                  targetIndex = clickedIndex - currentLine
                }
                currentLine += this.#domains.length
                if (this.#services.length > 0 || this.#activeApps.length > 0) currentLine++
              }

              if (targetIndex === -1 && this.#services.length > 0) {
                if (clickedIndex >= currentLine && clickedIndex < currentLine + this.#services.length) {
                  targetIndex = this.#domains.length + (clickedIndex - currentLine)
                }
                currentLine += this.#services.length
                if (this.#activeApps.length > 0) currentLine++
              }

              if (targetIndex === -1 && this.#activeApps.length > 0) {
                if (clickedIndex >= currentLine && clickedIndex < currentLine + this.#activeApps.length) {
                  targetIndex = this.#domains.length + this.#services.length + (clickedIndex - currentLine)
                }
              }

              if (targetIndex !== -1) {
                this.#selected = targetIndex
                this.#monitor()
              }
            }
          }
        }
      }

      // Ctrl+C
      if (buffer.length === 1 && buffer[0] === 3) {
        process.stdout.write('\x1b[?25h')
        process.stdout.write('\x1b[?1000l')
        process.stdout.write('\x1Bc')
        process.exit(0)
      }
      // Up/Down arrow keys
      if (buffer.length === 3 && buffer[0] === 27 && buffer[1] === 91) {
        if (buffer[2] === 65 && this.#selected > 0) this.#selected-- // up
        if (buffer[2] === 66 && this.#selected + 1 < totalItems) this.#selected++ // down
        this.#monitor()
      }
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
    })
  }

  #monitor() {
    if (this.#printing) return
    this.#printing = true
    this.#websites = Odac.core('Config').config.websites ?? {}
    this.#services = Odac.core('Config').config.services ?? []
    this.#apps = Odac.core('Config').config.app ?? {}

    this.#domains = Object.keys(this.#websites)
    this.#activeApps = Object.keys(this.#apps)

    this.#width = process.stdout.columns - 3
    this.#height = process.stdout.rows
    this.#load()
    let c1 = (this.#width / 12) * 3
    if (c1 % 1 != 0) c1 = Math.floor(c1)
    if (c1 > 50) c1 = 50

    let result = ''
    result += Odac.cli('Cli').color('┌', 'gray')

    if (this.#domains.length) {
      result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
      let title = Odac.cli('Cli').color(__('Websites'), null)
      result += ' ' + Odac.cli('Cli').color(title) + ' '
      result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
    } else if (this.#services.length) {
      result += Odac.cli('Cli').color('─'.repeat(this.#services.length ? 5 : c1), 'gray')
      if (this.#services.length) {
        let title = Odac.cli('Cli').color(__('Services'), null)
        result += ' ' + Odac.cli('Cli').color(title) + ' '
        result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
      }
    } else if (this.#activeApps.length) {
      result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
      let title = Odac.cli('Cli').color(__('Apps'), null)
      result += ' ' + Odac.cli('Cli').color(title) + ' '
      result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
    } else {
      result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    }

    result += Odac.cli('Cli').color('┬', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┐\n', 'gray')

    let renderedLines = 0
    let globalIndex = 0

    // WEBSITES
    for (let i = 0; i < this.#domains.length; i++) {
      if (renderedLines >= this.#height - 3) break
      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(this.#websites[this.#domains[i]].status ?? null, globalIndex == this.#selected)
      result += Odac.cli('Cli').color(
        Odac.cli('Cli').spacing(this.#domains[i] ? this.#domains[i] : '', c1 - 3),
        globalIndex == this.#selected ? 'blue' : 'white',
        globalIndex == this.#selected ? 'white' : null,
        globalIndex == this.#selected ? 'bold' : null
      )
      result += Odac.cli('Cli').color('│', 'gray')

      if (this.#logs.selected == globalIndex) {
        result += Odac.cli('Cli').spacing(this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' ', this.#width - c1)
      } else {
        result += ' '.repeat(this.#width - c1)
      }
      result += Odac.cli('Cli').color('│\n', 'gray')

      globalIndex++
      renderedLines++
    }

    // SERVICES SEPARATOR
    if (this.#services.length > 0 && (this.#domains.length > 0 || this.#activeApps.length > 0)) {
      if (renderedLines < this.#height - 3) {
        result += Odac.cli('Cli').color(this.#domains.length > 0 ? '├' : '│', 'gray')
        result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
        let title = Odac.cli('Cli').color(__('Services'), null)
        result += ' ' + Odac.cli('Cli').color(title) + ' '
        result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
        result += Odac.cli('Cli').color(this.#domains.length > 0 ? '┤' : '│', 'gray')
        result += ' '.repeat(this.#width - c1)
        result += Odac.cli('Cli').color('│\n', 'gray')
        renderedLines++
      }
    }

    // SERVICES
    for (let i = 0; i < this.#services.length; i++) {
      if (renderedLines >= this.#height - 3) break
      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(this.#services[i].status ?? null, globalIndex == this.#selected)
      result += Odac.cli('Cli').color(
        Odac.cli('Cli').spacing(this.#services[i].name, c1 - 3),
        globalIndex == this.#selected ? 'blue' : 'white',
        globalIndex == this.#selected ? 'white' : null,
        globalIndex == this.#selected ? 'bold' : null
      )
      result += Odac.cli('Cli').color('│', 'gray')

      if (this.#logs.selected == globalIndex) {
        result += Odac.cli('Cli').spacing(this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' ', this.#width - c1)
      } else {
        result += ' '.repeat(this.#width - c1)
      }
      result += Odac.cli('Cli').color('│\n', 'gray')
      globalIndex++
      renderedLines++
    }

    // APPS SEPARATOR
    if (this.#activeApps.length > 0) {
      if (renderedLines < this.#height - 3) {
        const hasPrev = this.#domains.length > 0 || this.#services.length > 0
        result += Odac.cli('Cli').color(hasPrev ? '├' : '│', 'gray')
        result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
        let title = Odac.cli('Cli').color(__('Apps'), null)
        result += ' ' + Odac.cli('Cli').color(title) + ' '
        result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
        if (hasPrev) {
          result += Odac.cli('Cli').color('┤', 'gray')
        } else {
          result += Odac.cli('Cli').color('│', 'gray')
        }
        result += ' '.repeat(this.#width - c1)
        result += Odac.cli('Cli').color('│\n', 'gray')
        renderedLines++
      }
    }

    // APPS
    for (let i = 0; i < this.#activeApps.length; i++) {
      if (renderedLines >= this.#height - 3) break
      const appName = this.#activeApps[i]
      const app = this.#apps[appName]

      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(app.status ?? null, globalIndex == this.#selected)
      result += Odac.cli('Cli').color(
        Odac.cli('Cli').spacing(appName, c1 - 3),
        globalIndex == this.#selected ? 'blue' : 'white',
        globalIndex == this.#selected ? 'white' : null,
        globalIndex == this.#selected ? 'bold' : null
      )
      result += Odac.cli('Cli').color('│', 'gray')

      if (this.#logs.selected == globalIndex) {
        result += Odac.cli('Cli').spacing(this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' ', this.#width - c1)
      } else {
        result += ' '.repeat(this.#width - c1)
      }
      result += Odac.cli('Cli').color('│\n', 'gray')
      globalIndex++
      renderedLines++
    }

    // FILL EMPTY LINES
    while (renderedLines < this.#height - 3) {
      result += Odac.cli('Cli').color('│', 'gray')
      result += ' '.repeat(c1)
      result += Odac.cli('Cli').color('│', 'gray')
      result += ' '.repeat(this.#width - c1)
      result += Odac.cli('Cli').color('│\n', 'gray')
      renderedLines++
    }

    result += Odac.cli('Cli').color('└', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Odac.cli('Cli').color('┴', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┘\n', 'gray')
    let shortcuts = '↑/↓ ' + __('Navigate') + ' | ↵ ' + __('Select') + ' | Ctrl+C ' + __('Exit')
    result += Odac.cli('Cli').color(' ODAC', 'magenta', 'bold')
    result += Odac.cli('Cli').color(Odac.cli('Cli').spacing(shortcuts, this.#width + 1 - 'ODAC'.length, 'right'), 'gray')
    if (result !== this.#current) {
      this.#current = result
      process.stdout.clearLine(0)
      process.stdout.write('\x1Bc')
      process.stdout.write(result)
    }
    this.#printing = false
  }
}

module.exports = Monitor
