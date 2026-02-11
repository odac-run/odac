require('../../core/Odac.js')

const fs = require('fs')
const os = require('os')
const net = require('net')
const {execFile} = require('child_process')

class Monitor {
  #current = ''
  #height
  #logs = {content: [], mtime: null, selected: null, watched: [], lastFetch: 0}
  #logging = false
  #modules = ['api', 'app', 'config', 'container', 'dns', 'hub', 'mail', 'proxy', 'server', 'ssl', 'updater']
  #printing = false
  #selected = 0
  #apps = []
  #maxStatsLen = {cpu: 0, mem: 0}
  #watch = []
  #width
  #stats = {}
  #lineToAppIndex = []

  constructor() {
    process.stdout.write(process.platform === 'win32' ? `title ODAC Debug\n` : `\x1b]2;ODAC Debug\x1b\x5c`)
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

    const appsCount = this.#apps.length

    if (this.#selected < appsCount) {
      // Apps are now all containers
      const app = this.#apps[this.#selected]
      if (app && app.name) {
        this.#fetchDockerLogs(app.name)
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

    const odacLogFile = os.homedir() + '/.odac/logs/.odac.log'
    const proxyLogFile = os.homedir() + '/.odac/logs/proxy.log'
    let log = ''
    let mtime = null

    // Read main ODAC log
    if (fs.existsSync(odacLogFile)) {
      mtime = fs.statSync(odacLogFile).mtime
      log = fs.readFileSync(odacLogFile, 'utf8')
    }

    // Read and merge proxy logs if proxy is selected or watching all
    const proxyIndex = this.#modules.indexOf('proxy')
    const isProxySelected = this.#watch.length === 0 || this.#watch.includes(proxyIndex)

    if (isProxySelected && fs.existsSync(proxyLogFile)) {
      const proxyMtime = fs.statSync(proxyLogFile).mtime
      if (proxyMtime > mtime) mtime = proxyMtime

      const proxyLog = fs.readFileSync(proxyLogFile, 'utf8')
      // Convert Go log format to our format for merging
      // Go format: 2006/01/02 15:04:05.000000 [INFO] Message
      const proxyLines = proxyLog
        .trim()
        .split('\n')
        .map(line => {
          // Parse Go log format: "2024/01/27 20:50:49.123456 [INFO] Message"
          const match = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2})(?:\.\d+)?\s+(.*)$/)
          if (match) {
            const dateStr = match[1].replace(/\//g, '-').replace(' ', 'T')
            const message = match[2]
            const isError = message.includes('[ERROR]') || message.includes('[WARN]')
            return `[${isError ? 'ERR' : 'LOG'}][${dateStr}][proxy] ${message}`
          }
          return `[LOG][proxy] ${line}`
        })
        .join('\n')
      log = log + '\n' + proxyLines
    }

    // Check cache
    if (JSON.stringify(this.#watch) === JSON.stringify(this.#logs.watched) && mtime == this.#logs.mtime) {
      this.#logging = false
      return
    }

    const selectedModules = this.#watch.length > 0 ? this.#watch.map(index => this.#modules[index]) : this.#modules
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

    execFile('docker', ['logs', '-t', '--tail', String(this.#height), containerName], (error, stdout, stderr) => {
      const rawLines = (stdout + '\n' + stderr)
        .replace(/\r\n/g, '\n')
        .split('\n')
        .filter(l => l.trim())

      if (error && rawLines.length === 0) {
        this.#logs.content = [Odac.cli('Cli').color('Error fetching logs: ' + error.message, 'red')]
      } else {
        this.#logs.content = rawLines
          .map(line => {
            const firstSpace = line.indexOf(' ')
            let timestamp = 0
            let dateObj = null
            if (firstSpace !== -1) {
              const rawDate = line.substring(0, firstSpace)
              dateObj = new Date(rawDate)
              if (!isNaN(dateObj.getTime())) timestamp = dateObj.getTime()
            }
            return {line, timestamp, dateObj}
          })
          .sort((a, b) => a.timestamp - b.timestamp)
          .map(item => {
            const {line, dateObj} = item
            const firstSpace = line.indexOf(' ')
            if (firstSpace === -1 || !dateObj || isNaN(dateObj.getTime())) return line

            let content = line.substring(firstSpace + 1)
            const formattedDate = Odac.cli('Cli').formatDate(dateObj)

            let color = 'green'
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

      const totalItems = this.#apps.length

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
              const clickedLine = y - 2
              const appIndex = this.#lineToAppIndex[clickedLine]
              if (appIndex !== undefined) {
                this.#selected = appIndex
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
        if (buffer[2] === 66 && this.#selected + 1 < this.#apps.length) this.#selected++ // down
        this.#monitor()
      }
      process.stdout.write('\x1b[?25l')
      process.stdout.write('\x1b[?1000h')
    })
  }

  #fetchStats() {
    execFile('docker', ['stats', '--no-stream', '--format', '{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}'], (error, stdout) => {
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
  #getLogLine(index) {
    const offset = this.#height - 4 - this.#logs.content.length
    if (index < offset) return ' '
    return this.#logs.content[index - offset] || ' '
  }
  #getSelectedItem() {
    const appsCount = this.#apps.length

    if (this.#selected < appsCount) {
      const index = this.#selected
      const app = this.#apps[index]
      if (app) {
        return {type: 'app', name: app.name, container: app.name}
      }
    }
    return null
  }

  #restartContainer(name) {
    if (!this.#logs.restarting) this.#logs.restarting = {}
    this.#logs.restarting[name] = Odac.cli('Cli').color(`Restarting ${name}...`, 'yellow')
    this.#monitor()

    const client = net.createConnection({port: 1453}, () => {
      const payload = {
        auth: Odac.core('Config').config.api.auth,
        action: 'app.restart',
        data: [name]
      }
      client.write(JSON.stringify(payload))
    })

    client.on('data', data => {
      try {
        const response = JSON.parse(data.toString())
        if (response.result) {
          this.#logs.restarting[name] = Odac.cli('Cli').color(`Successfully restarted ${name}`, 'green')
        } else {
          this.#logs.restarting[name] = Odac.cli('Cli').color(`Error restarting ${name}: ${response.message || 'Unknown error'}`, 'red')
        }
      } catch {
        this.#logs.restarting[name] = Odac.cli('Cli').color(`Error restarting ${name}: Invalid API response`, 'red')
      }
      client.end()
      this.#monitor()

      setTimeout(() => {
        if (this.#logs.restarting && this.#logs.restarting[name]) {
          delete this.#logs.restarting[name]
          this.#monitor()
        }
      }, 2000)
    })

    client.on('error', err => {
      this.#logs.restarting[name] = Odac.cli('Cli').color(`Connection failed: ${err.message}`, 'red')
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
    const config = Odac.core('Config').config
    const apps = config.apps ?? []
    const domains = config.domains ?? {}

    const appToDomains = {}
    for (const [name, conf] of Object.entries(domains)) {
      if (conf.appId) {
        if (!appToDomains[conf.appId]) appToDomains[conf.appId] = []
        appToDomains[conf.appId].push(name)
      }
    }

    const publicApps = apps
      .filter(app => appToDomains[app.id] || appToDomains[app.name])
      .sort((a, b) => (a.name || '').localeCompare(b.name || ''))
    const internalApps = apps
      .filter(app => !appToDomains[app.id] && !appToDomains[app.name])
      .sort((a, b) => (a.name || '').localeCompare(b.name || ''))

    this.#apps = [...publicApps, ...internalApps]

    let mainTitle = __('Apps')
    if (publicApps.length > 0 && internalApps.length > 0) mainTitle = __('Public')

    this.#width = process.stdout.columns - 3
    this.#height = process.stdout.rows
    this.#load()
    this.#lineToAppIndex = []
    let c1 = (this.#width / 12) * 3
    if (c1 % 1 != 0) c1 = Math.floor(c1)
    if (c1 > 50) c1 = 50

    const ctx = {renderedLines: 0, globalIndex: 0}

    let result = this.#renderHeader(c1, mainTitle)
    result += this.#renderApps(c1, ctx, publicApps.length)
    result += this.#renderEmptyLines(c1, ctx)
    result += this.#renderFooter(c1)

    this.#finalizeRender(result)
  }

  #renderHeader(c1, titleText) {
    let result = ''
    result += Odac.cli('Cli').color('┌', 'gray')
    if (this.#apps.length) {
      result += Odac.cli('Cli').color('─'.repeat(1), 'gray')
      let title = Odac.cli('Cli').color(titleText, null)
      result += ' ' + Odac.cli('Cli').color(title) + ' '
      result += Odac.cli('Cli').color('─'.repeat(c1 - titleText.length - 3), 'gray')
    } else {
      result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    }

    result += Odac.cli('Cli').color('┬', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┐\n', 'gray')
    return result
  }

  #renderApps(c1, ctx, publicCount) {
    let result = ''
    let shownInternalHeader = false

    for (let i = 0; i < this.#apps.length; i++) {
      if (ctx.renderedLines >= this.#height - 4) break

      const isPublic = i < publicCount

      if (!isPublic && publicCount > 0 && !shownInternalHeader) {
        result += this.#renderGroupHeader(c1, ctx, __('Internal'))
        shownInternalHeader = true
      }

      if (ctx.renderedLines >= this.#height - 4) break

      this.#lineToAppIndex[ctx.renderedLines] = ctx.globalIndex

      let stats = ''
      const app = this.#apps[i]
      const appName = app.name
      if (this.#stats[appName]) {
        const s = this.#stats[appName]
        stats = `[${s.mem.padEnd(this.#maxStatsLen.mem, ' ')}| ${s.cpu.padStart(this.#maxStatsLen.cpu, ' ')}]`
      }

      result += Odac.cli('Cli').color('│', 'gray')
      result += Odac.cli('Cli').icon(app.status ?? null, ctx.globalIndex == this.#selected)

      const maxLen = Math.max(0, Math.floor(c1 - 5 - stats.length))
      let display = appName.length > maxLen ? appName.substr(0, maxLen) : appName
      display = display.padEnd(maxLen, ' ')

      result += Odac.cli('Cli').color(
        display,
        ctx.globalIndex == this.#selected ? 'blue' : 'white',
        ctx.globalIndex == this.#selected ? 'white' : null,
        ctx.globalIndex == this.#selected ? 'bold' : null
      )

      const statsColor = ctx.globalIndex == this.#selected ? 'blue' : 'cyan'
      if (stats) result += Odac.cli('Cli').color(stats, statsColor, ctx.globalIndex == this.#selected ? 'white' : null)
      result += Odac.cli('Cli').color(' ', 'white', ctx.globalIndex == this.#selected ? 'white' : null)

      result += Odac.cli('Cli').color(' │', 'gray')

      result += this.#safeLog(this.#getLogLine(ctx.renderedLines), this.#width - c1)
      result += Odac.cli('Cli').color('│\n', 'gray')
      ctx.globalIndex++
      ctx.renderedLines++
    }
    return result
  }

  #renderGroupHeader(c1, ctx, titleText) {
    if (ctx.renderedLines >= this.#height - 4) return ''
    let result = ''
    result += Odac.cli('Cli').color('│', 'gray')

    result += Odac.cli('Cli').color('─'.repeat(1), 'gray')
    let title = Odac.cli('Cli').color(titleText, null)
    result += ' ' + Odac.cli('Cli').color(title) + ' '
    const padding = Math.max(0, c1 - titleText.length - 3)
    result += Odac.cli('Cli').color('─'.repeat(padding), 'gray')

    result += Odac.cli('Cli').color('│', 'gray')
    result += this.#safeLog(this.#getLogLine(ctx.renderedLines), this.#width - c1)
    result += Odac.cli('Cli').color('│\n', 'gray')

    ctx.renderedLines++
    return result
  }

  #renderEmptyLines(c1, ctx) {
    let result = ''
    while (ctx.renderedLines < this.#height - 4) {
      result += Odac.cli('Cli').color('│', 'gray')
      result += ' '.repeat(c1)
      result += Odac.cli('Cli').color('│', 'gray')

      result += this.#safeLog(this.#getLogLine(ctx.renderedLines), this.#width - c1)

      result += Odac.cli('Cli').color('│\n', 'gray')
      ctx.renderedLines++
    }
    return result
  }

  #renderFooter(c1) {
    let result = ''
    result += Odac.cli('Cli').color('└', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(c1), 'gray')
    result += Odac.cli('Cli').color('┴', 'gray')
    result += Odac.cli('Cli').color('─'.repeat(this.#width - c1), 'gray')
    result += Odac.cli('Cli').color('┘\n', 'gray')
    return result
  }

  #finalizeRender(result) {
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
