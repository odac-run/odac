require('../../core/Odac.js')

const fs = require('fs')
const os = require('os')
const {exec} = require('child_process')

class Monitor {
  #current = ''
  #domains = []
  #height
  #logs = {content: [], mtime: null, selected: null, watched: [], lastFetch: 0}
  #logging = false
  #modules = ['api', 'config', 'dns', 'hub', 'mail', 'server', 'service', 'ssl', 'subdomain', 'web']
  #printing = false
  #selected = 0
  #services = []
  #maxStatsLen = {cpu: 0, mem: 0}
  #watch = []
  #websites = {}
  #width
  #stats = {}

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
    let shortcuts = 'Mouse | ↑/↓ ' + __('Navigate') + ' | ↵ ' + __('Select') + ' | R ' + __('Restart') + ' | Ctrl+C ' + __('Exit')
    result += Odac.cli('Cli').color(' ODAC', 'magenta', 'bold')
    result += Odac.cli('Cli').color(Odac.cli('Cli').spacing(shortcuts, this.#width + 1 - 'ODAC'.length, 'right'), 'gray')
    if (result !== this.#current) {
      this.#current = result
      process.stdout.write('\x1Bc')
      process.stdout.write(result)
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
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
      const domain = this.#domains[this.#selected]
      if (this.#websites[domain]?.container) {
        this.#fetchDockerLogs(this.#websites[domain].container)
        return
      }
      file = os.homedir() + '/.odac/logs/' + domain + '.log'
    } else if (this.#selected < domainsCount + servicesCount) {
      // Services are now all containers
      const service = this.#services[this.#selected - domainsCount]
      if (service && service.name) {
        this.#fetchDockerLogs(service.name)
        return
      }
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
      .slice(-(this.#height - 4))
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
      .slice(-(this.#height - 4))

    this.#logs.mtime = mtime
    this.#logs.watched = [...this.#watch]
    this.#logging = false
  }

  #fetchDockerLogs(containerName) {
    // Check if we are restarting this container
    if (this.#logs.restarting && this.#logs.restarting[containerName]) {
      this.#logs.content = [this.#logs.restarting[containerName]]
      this.#logging = false
      return
    }

    // Throttle fetching docker logs to avoid flicker and heavy load
    const now = Date.now()
    if (this.#logs.selected === this.#selected && now - this.#logs.lastFetch < 1000) {
      // Use cached content if less than 1sec
      this.#logging = false
      return
    }

    const cmd = `docker logs -t --tail ${this.#height} ${containerName} 2>&1`
    exec(cmd, (error, stdout) => {
      if (error) {
        this.#logs.content = [Odac.cli('Cli').color('Error fetching logs: ' + error.message, 'red')]
      } else {
        this.#logs.content = stdout
          .replace(/\r\n/g, '\n')
          .trim()
          .split('\n')
          .filter(l => l)
          .map(line => {
            // Docker -t output format: 2024-12-24T11:22:33.444444444Z Content...
            const firstSpace = line.indexOf(' ')
            if (firstSpace === -1) return line

            const rawDate = line.substring(0, firstSpace)
            let content = line.substring(firstSpace + 1)

            // Try parse date
            const date = new Date(rawDate)
            let formattedDate = ''
            if (!isNaN(date.getTime())) {
              formattedDate = Odac.cli('Cli').formatDate(date)
            } else {
              // If not a valid date, maybe it wasn't a timestamped line (shouldn't happen with -t)
              return line
            }

            let color = 'green'

            // Heuristic to detect error logs if not explicitly marked
            if (content.includes('[ERR]') || content.toLowerCase().includes('error')) {
              color = 'red'
            }

            return Odac.cli('Cli').color(`[${formattedDate}]`, color, 'bold') + ' ' + content
          })
          .slice(-(this.#height - 4))
        this.#logs.lastFetch = now
      }
      this.#logging = false
      this.#monitor()
    })
  }

  monit() {
    this.#monitor()
    setInterval(() => this.#monitor(), 250)

    // Update stats every 2 seconds
    setInterval(() => this.#fetchStats(), 2000)
    // Initial fetch
    this.#fetchStats()

    // Mouse event handler
    process.stdout.write('\x1b[?25l')
    process.stdout.write('\x1b[?1000h')
    process.stdin.setRawMode(true)
    process.stdin.setEncoding('utf8')
    process.stdin.on('data', chunk => {
      const buffer = Buffer.from(chunk)

      const totalItems = this.#domains.length + this.#services.length

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
                if (this.#services.length > 0) currentLine++
              }

              if (targetIndex === -1 && this.#services.length > 0) {
                if (clickedIndex >= currentLine && clickedIndex < currentLine + this.#services.length) {
                  targetIndex = this.#domains.length + (clickedIndex - currentLine)
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

      // R (Restart)
      if (buffer.length === 1 && (buffer[0] === 114 || buffer[0] === 82)) {
        const item = this.#getSelectedItem()
        if (item && item.container) {
          this.#restartContainer(item.container)
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

  #fetchStats() {
    exec('docker stats --no-stream --format "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}"', (error, stdout) => {
      if (error) return
      const lines = stdout.trim().split('\n')

      let maxCpuLen = 0
      let maxMemLen = 0

      for (const line of lines) {
        const [name, cpu, mem] = line.split('|')
        if (name && cpu && mem) {
          let memUsage = mem.split('/')[0].trim()
          let cpuUsage = cpu.trim()

          memUsage = memUsage.replace(/(\d+)\.\d+([a-zA-Z]+)/, '$1$2').replace('iB', 'B')
          cpuUsage = cpuUsage.replace(/(\d+)\.\d+%/, '$1%')

          if (cpuUsage.length > maxCpuLen) maxCpuLen = cpuUsage.length
          if (memUsage.length > maxMemLen) maxMemLen = memUsage.length

          this.#stats[name] = {cpu: cpuUsage, mem: memUsage}
        }
      }
      this.#maxStatsLen = {cpu: maxCpuLen, mem: maxMemLen}
    })
  }

  #getSelectedItem() {
    const domainsCount = this.#domains.length
    const servicesCount = this.#services.length

    if (this.#selected < domainsCount) {
      const name = this.#domains[this.#selected]
      const container = this.#websites[name]?.container || name
      return {type: 'website', name, container}
    } else if (this.#selected < domainsCount + servicesCount) {
      const index = this.#selected - domainsCount
      const service = this.#services[index]
      if (service) {
        return {type: 'service', name: service.name, container: service.name}
      }
    }
    return null
  }

  #restartContainer(name) {
    if (!this.#logs.restarting) this.#logs.restarting = {}
    this.#logs.restarting[name] = Odac.cli('Cli').color(`Restarting ${name}...`, 'yellow')
    this.#monitor()

    exec(`docker restart ${name}`, err => {
      if (err) {
        this.#logs.restarting[name] = Odac.cli('Cli').color(`Error restarting ${name}: ${err.message}`, 'red')
      } else {
        this.#logs.restarting[name] = Odac.cli('Cli').color(`Successfully restarted ${name}`, 'green')
      }
      this.#monitor()

      setTimeout(() => {
        if (this.#logs.restarting && this.#logs.restarting[name]) {
          delete this.#logs.restarting[name]
          this.#monitor()
        }
      }, 2000)
    })
  }

  #safeLog(log, maxWidth) {
    if (!log) return ' '.repeat(maxWidth)
    let content = log.replace(/\r/g, '').replace(/\t/g, '  ')

    const ansiRegex =
      /[\u001b\u009b][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]|[\u001b\u009b]].+?(?:[\u0007]|[\u001b\u009b]\\)/g // eslint-disable-line no-control-regex

    let currentLen = 0
    let lastIterIndex = 0
    let result = ''
    let match

    while ((match = ansiRegex.exec(content)) !== null) {
      const textBefore = content.substring(lastIterIndex, match.index)
      if (currentLen + textBefore.length > maxWidth) {
        const remaining = maxWidth - currentLen
        result += textBefore.substring(0, remaining)
        return result + '\x1b[0m'
      }
      result += textBefore
      currentLen += textBefore.length

      result += match[0]
      lastIterIndex = ansiRegex.lastIndex
    }

    const tail = content.substring(lastIterIndex)
    if (currentLen + tail.length > maxWidth) {
      const remaining = maxWidth - currentLen
      result += tail.substring(0, remaining)
      return result + '\x1b[0m'
    }

    result += tail
    currentLen += tail.length

    return result + ' '.repeat(Math.max(0, maxWidth - currentLen))
  }

  #monitor() {
    if (this.#printing) return
    this.#printing = true
    this.#websites = Odac.core('Config').config.websites ?? {}
    this.#services = Odac.core('Config').config.services ?? []

    this.#domains = Object.keys(this.#websites)

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
      if (renderedLines >= this.#height - 4) break

      let stats = ''
      const containerName = this.#websites[this.#domains[i]].container || this.#domains[i]
      if (this.#stats[containerName]) {
        const s = this.#stats[containerName]
        stats = `[${s.mem.padEnd(this.#maxStatsLen.mem, ' ')}| ${s.cpu.padStart(this.#maxStatsLen.cpu, ' ')}]`
      }

      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(this.#websites[this.#domains[i]].status ?? null, globalIndex == this.#selected)

      const name = this.#domains[i] || ''
      const maxLen = Math.max(0, Math.floor(c1 - 5 - stats.length)) // -5 for icon + padding
      let display = name.length > maxLen ? name.substr(0, maxLen) : name
      display = display.padEnd(maxLen, ' ')

      result += Odac.cli('Cli').color(
        display,
        globalIndex == this.#selected ? 'blue' : 'white',
        globalIndex == this.#selected ? 'white' : null,
        globalIndex == this.#selected ? 'bold' : null
      )

      const statsColor = globalIndex == this.#selected ? 'blue' : 'cyan'
      if (stats) result += Odac.cli('Cli').color(stats, statsColor, globalIndex == this.#selected ? 'white' : null)
      result += Odac.cli('Cli').color(' ', 'white', globalIndex == this.#selected ? 'white' : null)

      result += Odac.cli('Cli').color(' │', 'gray')

      const logLine = this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' '
      result += this.#safeLog(logLine, this.#width - c1)
      result += Odac.cli('Cli').color('│\n', 'gray')

      globalIndex++
      renderedLines++
    }

    // SERVICES SEPARATOR
    if (this.#services.length > 0 && this.#domains.length > 0) {
      if (renderedLines < this.#height - 4) {
        result += Odac.cli('Cli').color(this.#domains.length > 0 ? '├' : '│', 'gray')
        result += Odac.cli('Cli').color('─'.repeat(5), 'gray')
        let title = Odac.cli('Cli').color(__('Services'), null)
        result += ' ' + Odac.cli('Cli').color(title) + ' '
        result += Odac.cli('Cli').color('─'.repeat(c1 - title.length - 7), 'gray')
        result += Odac.cli('Cli').color(this.#domains.length > 0 ? '┤' : '│', 'gray')
        const logLine = this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' '
        result += this.#safeLog(logLine, this.#width - c1)
        result += Odac.cli('Cli').color('│\n', 'gray')
        renderedLines++
      }
    }

    // SERVICES
    for (let i = 0; i < this.#services.length; i++) {
      if (renderedLines >= this.#height - 4) break

      let stats = ''
      const srvName = this.#services[i].name
      if (this.#stats[srvName]) {
        const s = this.#stats[srvName]
        stats = `[${s.mem.padEnd(this.#maxStatsLen.mem, ' ')}| ${s.cpu.padStart(this.#maxStatsLen.cpu, ' ')}]`
      }

      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(this.#services[i].status ?? null, globalIndex == this.#selected)

      const maxLen = Math.max(0, Math.floor(c1 - 5 - stats.length))
      let display = srvName.length > maxLen ? srvName.substr(0, maxLen) : srvName
      display = display.padEnd(maxLen, ' ')

      result += Odac.cli('Cli').color(
        display,
        globalIndex == this.#selected ? 'blue' : 'white',
        globalIndex == this.#selected ? 'white' : null,
        globalIndex == this.#selected ? 'bold' : null
      )

      const statsColor = globalIndex == this.#selected ? 'blue' : 'cyan'
      if (stats) result += Odac.cli('Cli').color(stats, statsColor, globalIndex == this.#selected ? 'white' : null)
      result += Odac.cli('Cli').color(' ', 'white', globalIndex == this.#selected ? 'white' : null)

      result += Odac.cli('Cli').color(' │', 'gray')

      if (this.#logs.selected == globalIndex) {
        const logLine = this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' '
        result += this.#safeLog(logLine, this.#width - c1)
      } else {
        result += ' '.repeat(this.#width - c1)
      }
      result += Odac.cli('Cli').color('│\n', 'gray')
      globalIndex++
      renderedLines++
    }

    // FILL EMPTY LINES
    while (renderedLines < this.#height - 4) {
      result += Odac.cli('Cli').color('│', 'gray')
      result += ' '.repeat(c1)
      result += Odac.cli('Cli').color('│', 'gray')

      const logLine = this.#logs.content[renderedLines] ? this.#logs.content[renderedLines] : ' '
      result += this.#safeLog(logLine, this.#width - c1)

      result += Odac.cli('Cli').color('│\n', 'gray')
      renderedLines++
    }

    result += Odac.cli('Cli').color('└', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Odac.cli('Cli').color('┴', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┘\n', 'gray')
    let shortcuts = 'Mouse | ↑/↓ ' + __('Navigate') + ' | ↵ ' + __('Select') + ' | R ' + __('Restart') + ' | Ctrl+C ' + __('Exit')
    result += Odac.cli('Cli').color(' ODAC', 'magenta', 'bold')
    result += Odac.cli('Cli').color(Odac.cli('Cli').spacing(shortcuts, this.#width + 1 - 'ODAC'.length, 'right'), 'gray')
    if (result !== this.#current) {
      this.#current = result
      process.stdout.clearLine(0)
      process.stdout.write('\x1Bc')
      process.stdout.write(result)
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
    }
    this.#printing = false
  }
}

module.exports = Monitor
