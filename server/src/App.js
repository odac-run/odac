const {log, error} = Odac.core('Log', false).init('App')
const fs = require('fs')
const path = require('path')
const https = require('https')
const nodeCrypto = require('crypto')
const net = require('net')

class App {
  constructor() {
    this.init()
  }

  init() {
    if (!Odac.core('Config').config.apps) Odac.core('Config').config.apps = {}
  }

  // Check and restart apps if needed (Watchdog)
  check() {
    const apps = Odac.core('Config').config.apps
    for (const [name, app] of Object.entries(apps)) {
      Odac.server('Container')
        .isRunning(name)
        .then(running => {
          if (!running) {
            log(`App ${name} is not running. Restarting...`)
            this.#start(name, app)
          }
        })
    }
  }

  async #start(name, app) {
    try {
      await Odac.server('Container').runApp(name, {
        image: app.image,
        ports: app.ports,
        volumes: app.volumes,
        env: app.env
      })
    } catch (err) {
      error(`Failed to start app ${name}: ${err.message}`)
    }
  }

  async list() {
    const apps = Odac.core('Config').config.apps
    if (Object.keys(apps).length === 0) {
      return Odac.server('Api').result(false, __('No installed apps found.'))
    }

    let list = []
    for (const [name, app] of Object.entries(apps)) {
      list.push(`${name} (${app.image}) - ${app.status || 'Unknown'}`)
    }
    return Odac.server('Api').result(true, __('Installed Apps:\n  ') + list.join('\n  '))
  }

  async delete(name) {
    if (!Odac.core('Config').config.apps[name]) {
      return Odac.server('Api').result(false, __('App %s not found.', name))
    }

    await Odac.server('Container').remove(name)
    delete Odac.core('Config').config.apps[name]

    return Odac.server('Api').result(true, __('App %s deleted.', name))
  }

  async install(type, progress) {
    progress('fetch', 'progress', __('Fetching recipe for %s...', type))

    let recipe
    try {
      recipe = await this.#fetchRecipe(type)
    } catch {
      // Fallback for demo purposes if network fails or repo doesn't exist
      if (type === 'mysql') {
        recipe = {
          name: 'mysql',
          description: 'MySQL Database',
          docker: {
            image: 'mysql:8.0',
            ports: [{container: 3306, host: 'auto'}],
            volumes: [{container: '/var/lib/mysql', host: 'data'}],
            environment: {
              MYSQL_ROOT_PASSWORD: {generate: true, length: 16},
              MYSQL_DATABASE: 'odac'
            }
          }
        }
      } else if (type === 'redis') {
        recipe = {
          name: 'redis',
          description: 'Redis Cache',
          docker: {
            image: 'redis:alpine',
            ports: [{container: 6379, host: 'auto'}],
            volumes: [{container: '/data', host: 'data'}],
            environment: {}
          }
        }
      } else if (type === 'postgres') {
        recipe = {
          name: 'postgres',
          description: 'PostgreSQL Database',
          docker: {
            image: 'postgres:15-alpine',
            ports: [{container: 5432, host: 'auto'}],
            volumes: [{container: '/var/lib/postgresql/data', host: 'data'}],
            environment: {
              POSTGRES_PASSWORD: {generate: true, length: 16},
              POSTGRES_DB: 'odac'
            }
          }
        }
      } else {
        return Odac.server('Api').result(false, __('Could not find recipe for %s', type))
      }
    }

    // 1. Determine unique name
    let name = recipe.name
    let validName = name
    let counter = 1
    while (Odac.core('Config').config.apps[validName]) {
      validName = `${name}-${counter}`
      counter++
    }
    name = validName

    progress('config', 'progress', __('Configuring %s...', name))

    // 2. Prepare Storage Path
    const appDir = path.join(Odac.core('Config').config.web.path, 'apps', name)
    if (!fs.existsSync(appDir)) {
      fs.mkdirSync(appDir, {recursive: true})
    }

    // 3. Process Volumes
    const volumes = []
    if (recipe.docker.volumes) {
      for (const vol of recipe.docker.volumes) {
        let hostPath = vol.host
        if (hostPath === 'data') {
          hostPath = path.join(appDir, 'data')
          if (!fs.existsSync(hostPath)) fs.mkdirSync(hostPath, {recursive: true})
        }
        volumes.push({
          host: hostPath,
          container: vol.container
        })
      }
    }

    // 4. Process Ports
    const ports = []
    if (recipe.docker.ports) {
      for (const port of recipe.docker.ports) {
        let hostPort = port.host
        if (hostPort === 'auto') {
          hostPort = await this.#findPort(30000) // Start search from 30000
        }
        ports.push({
          host: hostPort,
          container: port.container
        })
      }
    }

    // 5. Process Environment Variables
    const env = {}
    if (recipe.docker.environment) {
      for (const [key, val] of Object.entries(recipe.docker.environment)) {
        if (typeof val === 'object' && val.generate) {
          env[key] = this.#generatePassword(val.length || 16)
        } else {
          env[key] = val
        }
      }
    }

    // Save Config
    const appConfig = {
      image: recipe.docker.image,
      ports,
      volumes,
      env,
      created: Date.now(),
      status: 'installing'
    }
    Odac.core('Config').config.apps[name] = appConfig

    progress('container', 'progress', __('Starting container %s...', name))

    try {
      await this.#start(name, appConfig)
      Odac.core('Config').config.apps[name].status = 'running'
    } catch (err) {
      Odac.core('Config').config.apps[name].status = 'errored'
      return Odac.server('Api').result(false, err.message)
    }

    let infoMsg = __('App %s installed successfully.', name)
    // Append auto-generated passwords to the message
    const generatedKeys = Object.keys(recipe.docker.environment || {}).filter(
      k => typeof recipe.docker.environment[k] === 'object' && recipe.docker.environment[k].generate
    )

    if (generatedKeys.length > 0) {
      infoMsg += '\n\n' + __('Generated Credentials:')
      for (const key of generatedKeys) {
        infoMsg += `\n  ${key}: ${env[key]}`
      }
    }

    // Append ports
    if (ports.length > 0) {
      infoMsg += '\n\n' + __('Ports:')
      for (const port of ports) {
        infoMsg += `\n  ${port.container} -> ${port.host}`
      }
    }

    return Odac.server('Api').result(true, infoMsg)
  }

  async #fetchRecipe(source) {
    return new Promise((resolve, reject) => {
      let url = source
      if (!source.startsWith('http')) {
        // Assuming user/repo or just 'name' (official)
        if (source.includes('/')) {
          // user/repo
          url = `https://raw.githubusercontent.com/${source}/main/odac.json`
        } else {
          // official (using the placeholder logic inside install for now as base URL is not real)
          return reject(new Error('Official repo not implemented'))
        }
      }

      https
        .get(url, res => {
          if (res.statusCode !== 200) {
            return reject(new Error(`Failed to fetch recipe: Status ${res.statusCode}`))
          }
          let data = ''
          res.on('data', chunk => (data += chunk))
          res.on('end', () => {
            try {
              resolve(JSON.parse(data))
            } catch (e) {
              reject(e)
            }
          })
        })
        .on('error', reject)
    })
  }

  #generatePassword(length) {
    return nodeCrypto
      .randomBytes(Math.ceil(length / 2))
      .toString('hex')
      .slice(0, length)
  }

  async #findPort(start) {
    let port = start
    while (await this.#isPortInUse(port)) {
      port++
    }
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

module.exports = new App()
