const {log, error} = Odac.core('Log', false).init('Service')
const fs = require('fs')
const path = require('path')
const net = require('net')
const nodeCrypto = require('crypto')

class Service {
  #services = []
  #loaded = false

  #get(id) {
    if (!this.#loaded && this.#services.length == 0) {
      this.#services = Odac.core('Config').config.services ?? []
      this.#loaded = true
    }
    for (const service of this.#services) {
      if (service.id == id || service.name == id || service.file == id) return service
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

    let service = {
      id: this.#services.length,
      name: name,
      file: file,
      type: type, // 'script' or 'container'
      active: true,
      created: Date.now()
    }
    this.#services.push(service)
    // Map both by index and potentially other lookups if needed, but array is safer
    Odac.core('Config').config.services = this.#services
    return service
  }

  #set(id, key, value) {
    let service = this.#get(id)
    if (service) {
      if (typeof key == 'object') {
        for (const k in key) service[k] = key[k]
      } else {
        service[key] = value
      }
      Odac.core('Config').config.services = this.#services
      return true
    }
    return false
  }

  async check() {
    this.#services = Odac.core('Config').config.services ?? []
    for (const service of this.#services) {
      if (service.active) {
        // If it's a script or app container, check if it's running in Docker
        const isRunning = await Odac.server('Container').isRunning(service.name)

        if (!isRunning && service.status === 'running') {
          log('Service %s is not running in Docker. Restarting...', service.name)
          this.#run(service.id)
        } else if (!isRunning && service.status !== 'stopped' && service.status !== 'errored') {
          // Initial start or recovery
          this.#run(service.id)
        }
      }
    }
  }

  async delete(id) {
    return new Promise(resolve => {
      this.#deleteService(id, resolve)
    })
  }

  async #deleteService(id, resolve) {
    let service = this.#get(id)
    if (service) {
      await this.stop(service.id)
      this.#services = this.#services.filter(s => s.name != service.name && s.id != service.id)
      Odac.core('Config').config.services = this.#services

      // Also remove the container
      await Odac.server('Container').remove(service.name)

      return resolve(Odac.server('Api').result(true, __('Service %s deleted successfully.', service.name)))
    } else {
      return resolve(Odac.server('Api').result(false, __('Service %s not found.', id)))
    }
  }

  async #run(id) {
    const service = this.#get(id)
    if (!service) return

    log('Starting service %s (Type: %s)...', service.name, service.type)
    this.#set(id, {status: 'starting', updated: Date.now()})

    try {
      if (service.type === 'script') {
        await this.#runScript(service)
      } else if (service.type === 'container') {
        await this.#runContainer(service)
      }

      this.#set(id, {status: 'running', started: Date.now()})
      return true
    } catch (err) {
      error('Failed to start service %s: %s', service.name, err.message)
      this.#set(id, {status: 'errored', updated: Date.now()})
      return false
    }
  }

  async #runScript(service) {
    const filePath = service.file
    const dir = path.dirname(filePath)
    const filename = path.basename(filePath)
    const ext = path.extname(filename)

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
    await Odac.server('Container').runApp(service.name, {
      image: image,
      cmd: cmd,
      volumes: [{host: dir, container: '/app'}],
      env: {
        ODAC_SERVICE: 'true'
      }
    })
  }

  async #runContainer(service) {
    // For third-party apps like mysql, redis
    await Odac.server('Container').runApp(service.name, {
      image: service.image,
      ports: service.ports,
      volumes: service.volumes,
      env: service.env
    })
  }

  async init() {
    log('Initializing services...')
    this.#services = Odac.core('Config').config.services ?? []
    this.#loaded = true
  }

  async start(file) {
    return new Promise(resolve => {
      this.#startService(file, resolve)
    })
  }

  async #startService(file, resolve) {
    if (file && file.length > 0) {
      file = path.resolve(file)
      if (fs.existsSync(file)) {
        // Check if already exists by file path
        const existing = this.#services.find(s => s.file === file)

        if (!existing) {
          const service = this.#add(file, 'script')
          await this.#run(service.id)
          return resolve(Odac.server('Api').result(true, __('Service %s added successfully.', file)))
        } else {
          // If exists but stopped, restart
          if (existing.status !== 'running') {
            await this.#run(existing.id)
            return resolve(Odac.server('Api').result(true, __('Service %s started successfully.', existing.name)))
          }
          return resolve(Odac.server('Api').result(false, __('Service %s already exists and is running.', file)))
        }
      } else {
        return resolve(Odac.server('Api').result(false, __('Service file %s not found.', file)))
      }
    } else {
      return resolve(Odac.server('Api').result(false, __('Service file not specified.')))
    }
  }

  async install(type) {
    log('Installing app: %s', type)
    let recipe
    try {
      recipe = await this.#fetchRecipe(type)
    } catch {
      // Fallbacks
      if (type === 'mysql') {
        recipe = {
          name: 'mysql',
          image: 'mysql:8.0',
          ports: [{container: 3306, host: 'auto'}],
          volumes: [{container: '/var/lib/mysql', host: 'data'}],
          env: {MYSQL_ROOT_PASSWORD: {generate: true, length: 16}, MYSQL_DATABASE: 'odac'}
        }
      } else if (type === 'postgres') {
        recipe = {
          name: 'postgres',
          image: 'postgres:15-alpine',
          ports: [{container: 5432, host: 'auto'}],
          volumes: [{container: '/var/lib/postgresql/data', host: 'data'}],
          env: {POSTGRES_PASSWORD: {generate: true, length: 16}, POSTGRES_DB: 'odac'}
        }
      } else if (type === 'redis') {
        recipe = {
          name: 'redis',
          image: 'redis:alpine',
          ports: [{container: 6379, host: 'auto'}],
          volumes: [{container: '/data', host: 'data'}],
          env: {}
        }
      } else {
        return Odac.server('Api').result(false, __('Could not find recipe for %s', type))
      }
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

    const service = {
      id: this.#services.length,
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
    this.#services.push(service)
    Odac.core('Config').config.services = this.#services

    try {
      if (await this.#run(service.id)) {
        return Odac.server('Api').result(true, __('App %s installed successfully.', name))
      } else {
        throw new Error('Failed to start service container. Check logs for details.')
      }
    } catch (e) {
      // Rollback: remove service if installation failed
      this.#services = this.#services.filter(s => s.id !== service.id)
      Odac.core('Config').config.services = this.#services

      return Odac.server('Api').result(false, e.message)
    }
  }

  async stop(id) {
    let service = this.#get(id)
    if (service) {
      await Odac.server('Container').stop(service.name)
      this.#set(id, {status: 'stopped', active: false, pid: null})
    } else {
      log(__('Service %s not found.', id))
    }
  }

  async status() {
    let services = Odac.core('Config').config.services ?? []
    const containerServer = Odac.server('Container')

    for (const service of services) {
      const isRunning = await containerServer.isRunning(service.name)
      if (isRunning) {
        service.status = 'running'
        if (service.started) {
          var uptime = Date.now() - service.started
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
          service.uptime = uptimeString
        } else {
          service.uptime = 'Running'
        }
      } else {
        service.status = 'stopped'
        service.uptime = '-'
      }
    }
    return services
  }

  async list() {
    return Odac.server('Api').result(true, await this.status())
  }

  async #fetchRecipe() {
    throw new Error('Remote recipes not implemented yet')
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

module.exports = new Service()
