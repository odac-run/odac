require('../../core/Odac.js')

const childProcess = require('child_process')
const readline = require('readline')

class Cli {
  #backgrounds = {red: 41, green: 42, yellow: 43, blue: 44, magenta: 45, white: 47, gray: 100}
  colors = {red: 31, green: 32, yellow: 33, blue: 34, magenta: 35, white: 37, gray: 90}
  rl
  boot() {
    if (!this.booting) this.booting = true
    else return
    return new Promise(resolve => {
      console.log(__('Starting Odac Server...'))
      const child = childProcess.spawn('node', [__dirname + '/../../watchdog/index.js'], {
        detached: true,
        stdio: 'ignore'
      })
      child.unref()
      setTimeout(() => {
        Odac.core('Config').reload()
        resolve()
      }, 1000)
    })
  }

  close() {
    if (this.rl) this.rl.close()
    this.rl = null
  }

  color(text, color, ...args) {
    let output = text
    if (this.colors[color]) output = '\x1b[' + this.colors[color] + 'm' + output + '\x1b[0m'
    for (const arg of args) {
      if (this.#backgrounds[arg]) output = '\x1b[' + this.#backgrounds[arg] + 'm' + output + '\x1b[0m'
      if (arg == 'bold') output = '\x1b[1m' + output + '\x1b[0m'
    }
    return output
  }

  #format(text, raw) {
    if (typeof text !== 'string') return text
    let output = text.toString()
    if (output.toString().length > 1) {
      let begin = ''
      let end = ''
      while (output.substr(output.length - 1) == ' ') {
        end += ' '
        output = output.substr(0, output.length - 1)
      }
      while (output.substr(0, 1) == ' ') {
        begin += ' '
        output = output.substr(1)
      }
      if (output.substr(output.length - 1) == ':') {
        end = ':'
        output = output.substr(0, output.length - 1)
      }
      output = begin + output + end
    }
    if (!raw) {
      if (text == 'Odac') output = this.color(output, 'magenta')
      if (text == __('Running')) output = this.color(output, 'green')
      if (text == '\u2713') output = this.color(output, 'green')
      if (text == '\u2717') output = this.color(output, 'red')
    }
    return output
  }

  async #detail(command, obj) {
    let result = ''
    let space = 0
    if (obj.title) result += '\n\x1b[90m' + (await obj.title) + '\x1b\n'
    if (obj.description) {
      let args = ''
      if (obj.args) {
        // Only show positional arguments (not prefix arguments starting with -)
        let positionalArgs = obj.args.filter(arg => !arg.startsWith('-'))

        if (positionalArgs.length > 0) {
          args += ' <' + positionalArgs.join('> <') + '>'
        }
      }
      let line = '\x1b[91mcandy ' + command + '\x1b[0m\x1b[90m' + args + '\x1b[0m : ' + __(obj.description)
      result += line
      line = line.split(':')[0]
      if (line.length > space) space = line.length
    }
    if (obj.sub) {
      let lines = []
      for (const sub in obj.sub) {
        let detail = await this.#detail(command + ' ' + sub, obj.sub[sub])
        lines.push(detail.result)
        if (detail.space > space) space = detail.space
      }
      result += lines.join('\n')
    }

    return {result: result, space: space}
  }

  async help(commands) {
    let result = []
    let space = 0
    if (typeof commands == 'string') {
      let obj = Odac.core('Commands')
      let command = commands.shift()
      if (!obj[command]) return console.log(__(`'%s' is not a valid command.`, this.color(`candy ${commands.join(' ')}`, 'yellow')))
      obj = obj[command]
      while (commands.length > 0 && commands.length && obj.sub[commands[0]]) {
        command = commands.shift()
        if (!obj.sub[command]) return console.log(__(`'%s' is not a valid command.`, this.color(`candy ${commands.join(' ')}`, 'yellow')))
        obj = obj.sub[command]
      }
      let detail = await this.#detail(command, obj)
      if (detail.space > space) space = detail.space
      let lines = detail.result.split('\n')
      for (let line of lines) result.push(line)
    } else {
      for (const command in Odac.core('Commands')) {
        if (commands && commands !== true && commands[0] !== command) continue
        if (Odac.server('Hub').isAuthenticated() && command === 'auth') continue
        let obj = Odac.core('Commands')[command]
        if (commands === true && !obj.action) continue
        let detail = await this.#detail(command, obj)
        if (detail.space > space) space = detail.space
        let lines = detail.result.split('\n')
        for (let line of lines) result.push(line)
      }
      result = result.map(line => {
        if (line.includes(':')) {
          let parts = line.split(':')
          parts[0] = parts[0] + ' '.repeat(space - parts[0].length)
          line = parts.join(':')
        }
        return line
      })
    }
    result.push('')
    for (let line of result) console.log(line)
  }

  icon(status, selected) {
    if (status == 'errored') return this.color(' ! ', 'red', selected ? 'white' : null)
    if (status == 'progress') return this.color(' - ', 'gray', selected ? 'white' : null)
    if (status == 'running') return this.color(' \u25B6 ', 'green', selected ? 'white' : null)
    if (status == 'stopped') return this.color(' \u23F8 ', 'yellow', selected ? 'white' : null)
    if (status == 'success') return this.color(' \u2713 ', 'green', selected ? 'white' : null)
    return '   '
  }

  async init() {
    console.log('\n', this.#format('Odac'), '\n')
    if (!(await Odac.cli('Connector').check())) await this.boot()
    let args = process.argv.slice(2)
    let cmds = process.argv.slice(2)
    if (args.length == 0) return this.#status()
    let command = args.shift()
    if (!Odac.core('Commands')[command])
      return console.log(__(`'%s' is not a valid command.`, this.color(`candy ${cmds.join(' ')}`, 'yellow')))
    let action = Odac.core('Commands')[command]
    while (args.length > 0 && !action.args) {
      command = args.shift()
      if (!action.sub || !action.sub[command]) return this.help(cmds)
      action = action.sub[command]
    }
    if (action.action) return action.action(args)
    else return this.help(cmds)
  }

  #length(text) {
    return (
      this.#value(text, true)
        .toString()
        // eslint-disable-next-line no-control-regex
        .replace(/\x1b\[[0-9;]*m/g, '').length
    )
  }

  formatDate(date) {
    const YYYY = date.getFullYear()
    const MM = String(date.getMonth() + 1).padStart(2, '0')
    const DD = String(date.getDate()).padStart(2, '0')
    const HH = String(date.getHours()).padStart(2, '0')
    const mm = String(date.getMinutes()).padStart(2, '0')
    const ss = String(date.getSeconds()).padStart(2, '0')
    return `${YYYY}-${MM}-${DD} ${HH}:${mm}:${ss}`
  }

  async log(...args) {
    let output = []
    for (let i = 0; i < args.length; i++) {
      if (typeof args[i] != 'string') output.push(args[i])
      else output.push(this.#format(args[i]))
    }
    console.log(...output)
  }

  spacing(text, len, direction) {
    if (direction == 'right') return ' '.repeat(len - this.#length(text)) + text
    if (direction == 'center')
      return ' '.repeat(Math.floor((len - this.#length(text)) / 2)) + text + ' '.repeat(Math.ceil((len - this.#length(text)) / 2))
    if (this.#length(text) > len) return text.substr(0, text.length - this.#length(text) + len)
    return text + ' '.repeat(len - this.#length(text))
  }

  async #status() {
    let status = {
      online: false,
      services: 0,
      auth: false,
      uptime: 0
    }
    status.online = await Odac.cli('Connector').check()
    var uptime = Date.now() - Odac.core('Config').config.server.started
    let seconds = Math.floor(uptime / 1000)
    let minutes = Math.floor(seconds / 60)
    let hours = Math.floor(minutes / 60)
    let days = Math.floor(hours / 24)
    seconds %= 60
    minutes %= 60
    hours %= 24
    let uptimeString = ''
    if (days) uptimeString += days + 'd '
    if (hours) uptimeString += hours + 'h '
    if (minutes && !days) uptimeString += minutes + 'm '
    if (seconds && !hours) uptimeString += seconds + 's'
    status.uptime = uptimeString
    status.services = Odac.core('Config').config.services ? Object.keys(Odac.core('Config').config.services).length : 0
    status.websites = Odac.core('Config').config.websites ? Object.keys(Odac.core('Config').config.websites).length : 0
    status.auth = Odac.server('Hub').isAuthenticated()
    var args = process.argv.slice(2)
    if (args.length == 0) {
      let length = 0
      for (let i = 0; i < 2; i++) {
        for (let iterator of ['Status', 'Uptime', 'Websites', 'Services', 'Auth']) {
          let title = __(iterator)
          if (title.length > length) length = title.length
          if (i) {
            let space = ''
            for (let j = 0; j < length - title.length; j++) space += ' '
            switch (iterator) {
              case 'Status':
                console.log(title + space + ' : ' + (status.online ? '\x1b[32m ' + __('Online') : '\x1b[33m ' + __('Offline')) + '\x1b[0m')
                break
              case 'Uptime':
                if (status.online) console.log(title + space + ' : ' + '\x1b[32m ' + status.uptime + '\x1b[0m')
                break
              case 'Websites':
                if (status.online) console.log(title + space + ' : ' + '\x1b[32m ' + status.websites + '\x1b[0m')
                break
              case 'Services':
                if (status.online) console.log(title + space + ' : ' + '\x1b[32m ' + status.services + '\x1b[0m')
                break
              case 'Auth':
                console.log(
                  title + space + ' : ' + (status.auth ? '\x1b[32m ' + __('Logged in') : '\x1b[33m ' + __('Not logged in')) + '\x1b[0m'
                )
                break
            }
          }
        }
      }
      if (!status.auth) console.log(__('Login on %s to manage all your server operations.', '\x1b[95mhttps://odac.dev\x1b[0m'))
      console.log()
      console.log(__('Commands:'))
      length = 0
      this.help(true)
      console.log('')
    }
  }

  table(input) {
    let result = ''
    let width = []
    for (const row of input) {
      for (const key of Object.keys(row)) {
        if (input.indexOf(row) == 0) width[key] = this.#length(key)
        if (this.#length(row[key]) > width[key]) width[key] = this.#length(row[key])
      }
    }
    for (const row of input) {
      let insert = ''
      if (input.indexOf(row) == 0) {
        result += '┌─'
        for (const key of Object.keys(row)) result += '─'.repeat(width[key]) + '─┬─'
        result = this.color(result.substr(0, result.length - 3) + '─┐\n', 'gray')
        result += this.color('│ ', 'gray')
        for (const key of Object.keys(row)) {
          result += this.color(this.#value(key), 'blue') + ' '.repeat(width[key] - this.#length(key)) + this.color(' │ ', 'gray')
        }
        result += '\n'
      }
      insert += '├─'
      for (const key of Object.keys(row)) insert += '─'.repeat(width[key]) + '─┼─'
      insert = insert.substr(0, insert.length - 3) + '─┤\n'
      insert += '│ '
      result += this.color(insert, 'gray')
      for (const key of Object.keys(row)) {
        result += this.#value(row[key]) + ' '.repeat(width[key] - this.#length(row[key])) + this.color(' │ ', 'gray')
      }
      result += '\n'
    }
    let insert = '└─'
    for (const key of Object.keys(input[0])) insert += '─'.repeat(width[key]) + '─┴─'
    insert = insert.substr(0, insert.length - 3) + '─┘'
    result += this.color(insert, 'gray')
    console.log(result)
  }

  #value(text, raw) {
    if (!text) return ''
    let result = ''
    if (typeof text == 'object') result = this.#format(text.content)
    else result = this.#format(text, raw)
    return result
  }

  parseArg(args, prefixes) {
    if (!args || !prefixes) return null

    for (let i = 0; i < args.length; i++) {
      if (prefixes.includes(args[i]) && i + 1 < args.length) {
        return args[i + 1]
      }
    }
    return null
  }

  question(question) {
    return new Promise(resolve => {
      if (!this.rl) {
        this.rl = readline.createInterface({
          input: process.stdin,
          output: process.stdout
        })
      }
      this.rl.question(question, answer => {
        this.close()
        return resolve(answer.trim())
      })
    })
  }
}

module.exports = new Cli()
