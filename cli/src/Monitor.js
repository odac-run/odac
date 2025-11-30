require('../../core/Candy.js')

const fs = require('fs')
const os = require('os')

class Monitor {
  #current = ''
  #domains = []
  #height
  #logs = {content: [], mtime: null, selected: null, watched: []}
  #logging = false
  #modules = ['api', 'client', 'config', 'dns', 'hub', 'mail', 'server', 'service', 'ssl', 'subdomain', 'web']
  #printing = false
  #selected = 0
  #services = []
  #watch = []
  #websites = {}
  #width

  constructor() {
    process.stdout.write(process.platform === 'win32' ? `title CandyPack Debug\n` : `\x1b]2;CandyPack Debug\x1b\x5c`)
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
    result += Candy.cli('Cli').color('┌', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(5), 'gray')
    let title = Candy.cli('Cli').color(__('Modules'), null)
    result += ' ' + Candy.cli('Cli').color(title) + ' '
    result += Candy.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
    result += Candy.cli('Cli').color('┬', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(5), 'gray')
    title = Candy.cli('Cli').color(__('Logs'), null)
    result += ' ' + Candy.cli('Cli').color(title) + ' '
    result += Candy.cli('Cli').color('─'.repeat(this.#width - c1 - title.length - 7), 'gray')
    result += Candy.cli('Cli').color('┐\n', 'gray')
    for (let i = 0; i < this.#height - 3; i++) {
      if (this.#modules[i]) {
        result += Candy.cli('Cli').color('│', 'gray')
        result += Candy.cli('Cli').color(
          '[' + (this.#watch.includes(i) ? 'X' : ' ') + '] ',
          i == this.#selected ? 'blue' : 'white',
          i == this.#selected ? 'white' : null,
          i == this.#selected ? 'bold' : null
        )
        result += Candy.cli('Cli').color(
          Candy.cli('Cli').spacing(this.#modules[i] ? this.#modules[i] : '', c1 - 4),
          i == this.#selected ? 'blue' : 'white',
          i == this.#selected ? 'white' : null,
          i == this.#selected ? 'bold' : null
        )
        result += Candy.cli('Cli').color('│', 'gray')
      } else {
        result += Candy.cli('Cli').color('│', 'gray')
        result += ' '.repeat(c1)
        result += Candy.cli('Cli').color('│', 'gray')
      }
      result += Candy.cli('Cli').spacing(this.#logs.content[i] ? this.#logs.content[i] : ' ', this.#width - c1)
      result += Candy.cli('Cli').color('│\n', 'gray')
    }
    result += Candy.cli('Cli').color('└', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Candy.cli('Cli').color('┴', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Candy.cli('Cli').color('┘\n', 'gray')
    let shortcuts = '↑/↓ ' + __('Navigate') + ' | ↵ ' + __('Select') + ' | Ctrl+C ' + __('Exit')
    result += Candy.cli('Cli').color(' CANDYPACK', 'magenta', 'bold')
    result += Candy.cli('Cli').color(Candy.cli('Cli').spacing(shortcuts, this.#width + 1 - 'CANDYPACK'.length, 'right'), 'gray')
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
    if (this.#selected < this.#domains.length) {
      file = os.homedir() + '/.candypack/logs/' + this.#domains[this.#selected] + '.log'
    } else if (this.#selected - this.#domains.length < this.#services.length) {
      file = os.homedir() + '/.candypack/logs/' + this.#services[this.#selected - this.#domains.length].name + '.log'
    } else {
      this.#logging = false
      return
    }
    let log = ''
    let mtime = null
    if (fs.existsSync(file)) {
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
          line = Candy.cli('Cli').color('[' + Candy.cli('Cli').formatDate(new Date(date)) + ']', 'green', 'bold') + line.substring(15)
        } else if ('[ERR]' == line.substring(0, 5)) {
          line = line.substring(5)
          let date = parseInt(line.substring(1, 14))
          line = Candy.cli('Cli').color('[' + Candy.cli('Cli').formatDate(new Date(date)) + ']', 'red', 'bold') + line.substring(15)
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

    const file = os.homedir() + '/.candypack/logs/.candypack.log'
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
            Candy.cli('Cli').color('[' + Candy.cli('Cli').formatDate(new Date(date)) + ']', dateColor, 'bold') +
            Candy.cli('Cli').color(`[${moduleName}]`, 'white', 'bold') +
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
          if (this.#selected + 1 < this.#domains.length + this.#services.length) {
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
              if (this.#domains[y - 2]) this.#selected = y - 2
              else if (this.#services[y - 2 - (this.#domains.length ? 1 : 0) - this.#domains.length])
                this.#selected = y - 2 - (this.#domains.length ? 1 : 0)
              let index = this.#watch.indexOf(this.#selected)
              if (index > -1) this.#watch.splice(index, 1)
              else this.#watch.push(this.#selected)
              this.#monitor()
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
        if (buffer[2] === 66 && this.#selected + 1 < this.#domains.length + this.#services.length) this.#selected++ // down
        this.#monitor()
      }
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
    })
  }

  #monitor() {
    if (this.#printing) return
    this.#printing = true
    this.#websites = Candy.core('Config').config.websites ?? []
    this.#services = Candy.core('Config').config.services ?? []
    this.#domains = Object.keys(this.#websites)
    this.#width = process.stdout.columns - 3
    this.#height = process.stdout.rows
    this.#load()
    let c1 = (this.#width / 12) * 3
    if (c1 % 1 != 0) c1 = Math.floor(c1)
    if (c1 > 50) c1 = 50
    let result = ''
    result += Candy.cli('Cli').color('┌', 'gray')
    let service = -1
    if (this.#domains.length) {
      result += Candy.cli('Cli').color('─'.repeat(5), 'gray')
      let title = Candy.cli('Cli').color(__('Websites'), null)
      result += ' ' + Candy.cli('Cli').color(title) + ' '
      result += Candy.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
    } else if (this.#services.length) {
      result += Candy.cli('Cli').color('─'.repeat(5), 'gray')
      let title = Candy.cli('Cli').color(__('Services'), null)
      result += ' ' + Candy.cli('Cli').color(title) + ' '
      result += Candy.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
      service++
    } else {
      result += Candy.cli('Cli').color('─'.repeat(c1), 'gray')
    }
    result += Candy.cli('Cli').color('┬', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Candy.cli('Cli').color('┐\n', 'gray')
    for (let i = 0; i < this.#height - 3; i++) {
      if (this.#domains[i]) {
        result += Candy.cli('Cli').color('│', 'gray')
        result += Candy.cli('Cli').icon(this.#websites[this.#domains[i]].status ?? null, i == this.#selected)
        result += Candy.cli('Cli').color(
          Candy.cli('Cli').spacing(this.#domains[i] ? this.#domains[i] : '', c1 - 3),
          i == this.#selected ? 'blue' : 'white',
          i == this.#selected ? 'white' : null,
          i == this.#selected ? 'bold' : null
        )
        result += Candy.cli('Cli').color('│', 'gray')
      } else if (this.#services.length && service == -1) {
        result += Candy.cli('Cli').color('├', 'gray')
        result += Candy.cli('Cli').color('─'.repeat(5), 'gray')
        let title = Candy.cli('Cli').color(__('Services'), null)
        result += ' ' + Candy.cli('Cli').color(title) + ' '
        result += Candy.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
        result += Candy.cli('Cli').color('┤', 'gray')
        service++
      } else if (service >= 0 && service < this.#services.length) {
        result += Candy.cli('Cli').color('│', 'gray')
        result += Candy.cli('Cli').icon(this.#services[service].status ?? null, i - 1 == this.#selected)
        result += Candy.cli('Cli').color(
          Candy.cli('Cli').spacing(this.#services[service].name, c1 - 3),
          i - 1 == this.#selected ? 'blue' : 'white',
          i - 1 == this.#selected ? 'white' : null,
          i - 1 == this.#selected ? 'bold' : null
        )
        result += Candy.cli('Cli').color('│', 'gray')
        service++
      } else {
        result += Candy.cli('Cli').color('│', 'gray')
        result += ' '.repeat(c1)
        result += Candy.cli('Cli').color('│', 'gray')
      }
      if (this.#logs.selected == this.#selected) {
        result += Candy.cli('Cli').spacing(this.#logs.content[i] ? this.#logs.content[i] : ' ', this.#width - c1)
      } else {
        result += ' '.repeat(this.#width - c1)
      }
      result += Candy.cli('Cli').color('│\n', 'gray')
    }
    result += Candy.cli('Cli').color('└', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Candy.cli('Cli').color('┴', 'gray')
    result += Candy.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Candy.cli('Cli').color('┘\n', 'gray')
    let shortcuts = '↑/↓ ' + __('Navigate') + ' | Ctrl+C ' + __('Exit')
    result += Candy.cli('Cli').color(' CANDYPACK', 'magenta', 'bold')
    result += Candy.cli('Cli').color(Candy.cli('Cli').spacing(shortcuts, this.#width + 1 - 'CANDYPACK'.length, 'right'), 'gray')
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
