const {log, error} = Odac.core('Log', false).init('App')
const fs = require('fs')
const path = require('path')
const net = require('net')
const nodeCrypto = require('crypto')
const childProcess = require('child_process')
const os = require('os')

class App {
  #apps = []
  #loaded = false

  #get(id) {
    if (!this.#loaded && this.#apps.length == 0) {
      this.#apps = Odac.core('Config').config.apps ?? []
      this.#loaded = true
    }
    for (const app of this.#apps) {
      if (app.id == id || app.name == id || app.file == id) return app
    }
    return false
  }

  #add(file, type = 'script') {
    let name = path.basename(file)
    // If it's a script, remove extension for name
    if (
      type === 'script' &&
      (name.endsWith('.js') || name.endsWith('.py') || name.endsWith('.php') || name.endsWith('.sh') || name.endsWith('.rb'))
    ) {
      name = name.split('.').slice(0, -1).join('.')
    }

    // Check if name exists, if so append number
    let uniqueName = name
    let counter = 1
    while (this.#get(uniqueName)) {
      uniqueName = `${name}-${counter}`
      counter++
    }
    name = uniqueName

    let app = {
      id: this.#apps.length,
      name: name,
      file: file,
      type: type, // 'script' or 'container'
      active: true,
      created: Date.now()
    }
    this.#apps.push(app)
    // Map both by index and potentially other lookups if needed, but array is safer
    Odac.core('Config').config.apps = this.#apps
    return app
  }

  #set(id, key, value) {
    let app = this.#get(id)
    if (app) {
      if (typeof key == 'object') {
        for (const k in key) app[k] = key[k]
      } else {
        app[key] = value
      }
      Odac.core('Config').config.apps = this.#apps
      return true
    }
    return false
  }

  async check() {
    this.#apps = Odac.core('Config').config.apps ?? []
    for (const app of this.#apps) {
      if (app.active) {
        // If it's a script or app container, check if it's running
        let isRunning = false
        if (app.type === 'container' || Odac.server('Container').available) {
          isRunning = await Odac.server('Container').isRunning(app.name)
        } else if (app.type === 'script' && app.pid) {
          try {
            process.kill(app.pid, 0)
            isRunning = true
          } catch {
            isRunning = false
          }
        }

        if (!isRunning && app.status === 'running') {
          log('App %s is not running. Restarting...', app.name)
          this.#run(app.id)
        } else if (!isRunning && app.status !== 'stopped' && app.status !== 'errored') {
          // Initial start or recovery
          this.#run(app.id)
        }
      }
    }
  }

  async delete(id) {
    return new Promise(resolve => {
      this.#deleteApp(id, resolve)
    })
  }

  async #deleteApp(id, resolve) {
    let app = this.#get(id)
    if (app) {
      await this.stop(app.id)
      this.#apps = this.#apps.filter(s => s.name != app.name && s.id != app.id)
      Odac.core('Config').config.apps = this.#apps

      // Also remove the container
      await Odac.server('Container').remove(app.name)

      return resolve(Odac.server('Api').result(true, __('App %s deleted successfully.', app.name)))
    } else {
      return resolve(Odac.server('Api').result(false, __('App %s not found.', id)))
    }
  }

  async #run(id) {
    const app = this.#get(id)
    if (!app) return

    log('Starting app %s (Type: %s)...', app.name, app.type)
    this.#set(id, {status: 'starting', updated: Date.now()})

    try {
      if (app.type === 'script') {
        await this.#runScript(app)
      } else if (app.type === 'container') {
        await this.#runContainer(app)
      }

      this.#set(id, {status: 'running', started: Date.now()})
      return true
    } catch (err) {
      error('Failed to start app %s: %s', app.name, err.message)
      this.#set(id, {status: 'errored', updated: Date.now()})
      return false
    }
  }

  async #runScript(app) {
    const filePath = app.file
    const dir = path.dirname(filePath)
    const filename = path.basename(filePath)
    const ext = path.extname(filename)

    // Bare-metal execution if Container is not available
    if (!Odac.server('Container').available) {
      let cmd = 'node'
      let args = [filename]

      if (ext === '.py') {
        cmd = 'python3'
        args = ['-u', filename]
      } else if (ext === '.php') {
        cmd = 'php'
        args = [filename]
      } else if (ext === '.rb') {
        cmd = 'ruby'
        args = [filename]
      } else if (ext === '.sh') {
        cmd = 'sh'
        args = [filename]
      }

      // Create a write stream for the log file
      const logDir = path.join(os.homedir(), '.odac', 'logs')
      if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, {recursive: true})
      const logFile = path.join(logDir, `${app.name}.log`)
      const logStream = fs.createWriteStream(logFile, {flags: 'a'})

      log(`Spawning local process for ${app.name}: ${cmd} ${args.join(' ')}`)

      const child = childProcess.spawn(cmd, args, {
        cwd: dir,
        stdio: ['ignore', 'pipe', 'pipe'],
        env: {...process.env, ODAC_APP: 'true'}
      })

      // Update app with PID
      this.#set(app.id, {pid: child.pid})

      child.stdout.on('data', data => {
        logStream.write(`[LOG] [${new Date().getTime()}] ${data}`)
      })

      child.stderr.on('data', data => {
        logStream.write(`[ERR] [${new Date().getTime()}] ${data}`)
      })

      child.on('exit', (code, signal) => {
        log(`App ${app.name} exited with code ${code} signal ${signal}`)
        logStream.write(`[LOG] [${new Date().getTime()}] App exited with code ${code} signal ${signal}\n`)
        logStream.end()
        this.#set(app.id, {status: 'stopped', pid: null, active: false})
      })

      child.on('error', err => {
        error(`Failed to start local app ${app.name}: ${err.message}`)
        logStream.write(`[ERR] [${new Date().getTime()}] Failed to start app: ${err.message}\n`)
        logStream.end()
        this.#set(app.id, {status: 'errored', pid: null})
      })

      return
    }

    let image = 'node:lts-alpine'
    let cmd = ['node', filename]

    if (ext === '.py') {
      image = 'python:alpine'
      cmd = ['python3', '-u', filename] // -u for unbuffered output
    } else if (ext === '.php') {
      image = 'php:cli-alpine'
      cmd = ['php', filename]
    } else if (ext === '.rb') {
      image = 'ruby:alpine'
      cmd = ['ruby', filename]
    } else if (ext === '.sh') {
      image = 'alpine:latest'
      cmd = ['sh', filename]
    }

    // Use the generic runApp from Container
    await Odac.server('Container').runApp(app.name, {
      image: image,
      cmd: cmd,
      volumes: [{host: dir, container: '/app'}],
      env: {
        ODAC_APP: 'true'
      }
    })
  }

  async #runContainer(app) {
    if (!Odac.server('Container').available) {
      throw new Error('Docker is not available via Container service.')
    }
    // For third-party apps like mysql, redis
    await Odac.server('Container').runApp(app.name, {
      image: app.image,
      ports: app.ports,
      volumes: app.volumes,
      env: app.env
    })
  }

  async init() {
    log('Initializing apps...')
    this.#apps = Odac.core('Config').config.apps ?? []
    this.#loaded = true
  }

  async start(file) {
    return new Promise(resolve => {
      this.#startApp(file, resolve)
    })
  }

  async #startApp(file, resolve) {
    if (file && file.length > 0) {
      file = path.resolve(file)
      if (fs.existsSync(file)) {
        // Check if already exists by file path
        const existing = this.#apps.find(s => s.file === file)

        if (!existing) {
          const app = this.#add(file, 'script')
          await this.#run(app.id)
          return resolve(Odac.server('Api').result(true, __('App %s added successfully.', file)))
        } else {
          // If exists but stopped, restart
          if (existing.status !== 'running') {
            await this.#run(existing.id)
            return resolve(Odac.server('Api').result(true, __('App %s started successfully.', existing.name)))
          }
          return resolve(Odac.server('Api').result(false, __('App %s already exists and is running.', file)))
        }
      } else {
        return resolve(Odac.server('Api').result(false, __('App file %s not found.', file)))
      }
    } else {
      return resolve(Odac.server('Api').result(false, __('App file not specified.')))
    }
  }

  async install(type) {
    log('Installing app: %s', type)
    let recipe
    try {
      recipe = await this.#fetchRecipe(type)
    } catch (e) {
      return Odac.server('Api').result(false, __('Could not find recipe for %s: %s', type, e))
    }

    let name = recipe.name
    let counter = 1
    while (this.#get(name)) {
      name = `${recipe.name}-${counter}`
      counter++
    }

    const appDir = path.join(Odac.core('Config').config.web.path, 'apps', name)
    if (!fs.existsSync(appDir)) fs.mkdirSync(appDir, {recursive: true})

    const volumes = []
    if (recipe.volumes) {
      for (const vol of recipe.volumes) {
        let host = vol.host
        if (host === 'data') {
          host = path.join(appDir, 'data')
          if (!fs.existsSync(host)) fs.mkdirSync(host, {recursive: true})
        }
        volumes.push({host, container: vol.container})
      }
    }

    const ports = []
    if (recipe.ports) {
      for (const port of recipe.ports) {
        let hostPort = port.host
        if (hostPort === 'auto') hostPort = await this.#findPort(30000)
        ports.push({host: hostPort, container: port.container})
      }
    }

    const env = {}
    if (recipe.env) {
      for (const [k, v] of Object.entries(recipe.env)) {
        if (typeof v === 'object' && v.generate) {
          env[k] = this.#generatePassword(v.length || 16)
        } else {
          env[k] = v
        }
      }
    }

    const app = {
      id: this.#apps.length,
      name: name,
      type: 'container',
      image: recipe.image,
      ports: ports,
      volumes: volumes,
      env: env,
      active: true,
      created: Date.now(),
      status: 'installing'
    }
    this.#apps.push(app)
    Odac.core('Config').config.apps = this.#apps

    try {
      if (await this.#run(app.id)) {
        return Odac.server('Api').result(true, __('App %s installed successfully.', name))
      } else {
        throw new Error('Failed to start app container. Check logs for details.')
      }
    } catch (e) {
      // Rollback: remove app if installation failed
      this.#apps = this.#apps.filter(s => s.id !== app.id)
      Odac.core('Config').config.apps = this.#apps

      return Odac.server('Api').result(false, e.message)
    }
  }

  async stop(id) {
    let app = this.#get(id)
    if (app) {
      if (app.type === 'container' || (Odac.server('Container').available && !app.pid)) {
        await Odac.server('Container').stop(app.name)
      } else if (app.pid) {
        try {
          process.kill(app.pid)
        } catch {
          /* ignore if already dead */
        }
      }
      this.#set(id, {status: 'stopped', active: false, pid: null})
    } else {
      log(__('App %s not found.', id))
    }
  }

  async status() {
    let apps = Odac.core('Config').config.apps ?? []
    const containerServer = Odac.server('Container')

    for (const app of apps) {
      const isRunning = await containerServer.isRunning(app.name)
      if (isRunning) {
        app.status = 'running'
        if (app.started) {
          var uptime = Date.now() - app.started
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
          if (minutes) uptimeString += minutes + 'm '
          if (seconds) uptimeString += seconds + 's'
          app.uptime = uptimeString
        } else {
          app.uptime = 'Running'
        }
      } else {
        app.status = 'stopped'
        app.uptime = '-'
      }
    }
    return apps
  }

  async list() {
    const apps = await this.status()
    if (apps.length === 0) return Odac.server('Api').result(false, __('No apps found.'))

    let output = []
    output.push(String('NAME').padEnd(20) + String('TYPE').padEnd(15) + String('STATUS').padEnd(15) + String('UPTIME'))
    output.push('-'.repeat(60))

    for (const app of apps) {
      let status = app.status || 'stopped'
      let uptime = app.uptime || '-'
      output.push(String(app.name).padEnd(20) + String(app.type).padEnd(15) + String(status).padEnd(15) + String(uptime))
    }

    return Odac.server('Api').result(true, output.join('\n'))
  }

  async #fetchRecipe(type) {
    return await Odac.server('Hub').getApp(type)
  }

  #generatePassword(length) {
    return nodeCrypto
      .randomBytes(Math.ceil(length / 2))
      .toString('hex')
      .slice(0, length)
  }

  async #findPort(start) {
    let port = start
    while (await this.#isPortInUse(port)) port++
    return port
  }

  #isPortInUse(port) {
    return new Promise(resolve => {
      const server = net.createServer()
      server.on('connection', socket => {
        socket.on('error', () => {})
      })
      server.once('error', err => {
        if (err.code === 'EADDRINUSE') resolve(true)
        else resolve(false)
      })
      server.once('listening', () => {
        server.close()
        resolve(false)
      })
      server.listen(port, '127.0.0.1')
    })
  }
}

module.exports = new App()
