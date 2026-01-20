const noop = () => {}
const {log, error} = typeof Odac !== 'undefined' && Odac.core ? Odac.core('Log', false).init('Web') : {log: noop, error: noop}

const childProcess = require('child_process')
const fs = require('fs')
const net = require('net')
const os = require('os')
const path = require('path')
const axios = require('axios')

const WebFirewall = require('./Web/Firewall.js')

class Web {
  #active = {}
  #error_counts = {}
  #loaded = false
  #firewall
  #started = {}
  #watcher = {}
  #logs = {log: {}, err: {}}
  #ports = {}
  #proxy

  #proxyProcess = null
  #proxySocketPath = null
  #proxyApiPort = null

  constructor() {
    this.#firewall = new WebFirewall()
  }

  clearSSLCache() {
    // Deprecated: SSL cache is now managed by Go Proxy
    // We could implement an API call to clear Go cache if needed
  }

  check() {
    if (!this.#loaded) return
    for (const domain of Object.keys(Odac.core('Config').config.websites ?? {})) {
      if (!Odac.core('Config').config.websites[domain].pid) {
        this.start(domain)
      } else if (!this.#watcher[Odac.core('Config').config.websites[domain].pid]) {
        Odac.core('Process').stop(Odac.core('Config').config.websites[domain].pid)
        Odac.core('Config').config.websites[domain].pid = null
        this.start(domain)
      }
      if (this.#logs.log[domain]) {
        const logDir = path.join(os.homedir(), '.odac', 'logs')
        if (!fs.existsSync(logDir)) {
          try {
            fs.mkdirSync(logDir, {recursive: true})
          } catch (e) {
            log(e)
          }
        }
        fs.writeFile(path.join(logDir, domain + '.log'), this.#logs.log[domain], function (err) {
          if (err) log(err)
        })
      }
      if (this.#logs.err[domain]) {
        fs.writeFile(Odac.core('Config').config.websites[domain].path + '/error.log', this.#logs.err[domain], function (err) {
          if (err) log(err)
        })
      }
    }
    this.server()
  }

  checkPort(port) {
    return new Promise(resolve => {
      const server = net.createServer()
      server.on('connection', socket => {
        socket.on('error', () => {})
      })
      server.once('error', () => resolve(false))
      server.once('listening', () => {
        server.close()
        resolve(true)
      })
      server.listen(port, '127.0.0.1')
    })
  }

  async create(domain, progress) {
    let web = {}
    for (const iterator of ['http://', 'https://', 'ftp://', 'www.']) {
      if (domain.startsWith(iterator)) domain = domain.replace(iterator, '')
    }
    if (domain.length < 3 || (!domain.includes('.') && domain != 'localhost'))
      return Odac.server('Api').result(false, __('Invalid domain.'))
    if (Odac.core('Config').config.websites?.[domain]) return Odac.server('Api').result(false, __('Website %s already exists.', domain))
    progress('domain', 'progress', __('Setting up domain %s...', domain))
    web.domain = domain
    web.path = path.join(Odac.core('Config').config.web.path, domain)
    if (!fs.existsSync(web.path)) fs.mkdirSync(web.path, {recursive: true})
    if (!Odac.core('Config').config.websites) Odac.core('Config').config.websites = {}
    web.cert = false
    Odac.core('Config').config.websites[web.domain] = web
    progress('domain', 'success', __('Domain %s set.', domain))
    if (web.domain != 'localhost' && !web.domain.match(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)) {
      progress('dns', 'progress', __('Setting up DNS records for %s...', domain))
      Odac.core('Config').config.websites[web.domain].subdomain = ['www']
      Odac.server('DNS').record(
        {name: web.domain, type: 'A', value: Odac.server('DNS').ip},
        {name: 'www.' + web.domain, type: 'CNAME', value: web.domain},
        {name: web.domain, type: 'MX', value: web.domain},
        {name: web.domain, type: 'TXT', value: 'v=spf1 a mx ip4:' + Odac.server('DNS').ip + ' ~all'},
        {
          name: '_dmarc.' + web.domain,
          type: 'TXT',
          value: 'v=DMARC1; p=reject; rua=mailto:postmaster@' + web.domain
        }
      )
      progress('dns', 'success', __('DNS records for %s set.', domain))
      Odac.core('Config').config.websites[web.domain].cert = {}
      progress('ssl', 'progress', __('Setting up SSL certificate for %s...', domain))
    }
    progress('directory', 'progress', __('Setting up website files for %s...', domain))

    if (Odac.server('Container').available) {
      // Create package.json manually for Docker environment
      const packageJson = {
        name: domain.replace(/\./g, '-'),
        version: '1.0.0',
        description: '',
        main: 'index.js',
        scripts: {
          test: 'echo "Error: no test specified" && exit 1'
        },
        keywords: [],
        author: '',
        license: 'ISC',
        dependencies: {
          odac: 'latest'
        }
      }
      fs.writeFileSync(path.join(web.path, 'package.json'), JSON.stringify(packageJson, null, 2))

      progress('directory', 'success', __('Website files for %s set.', domain))

      // Start the container immediately
      progress('container', 'progress', __('Starting container for %s...', domain))
      await this.start(domain)

      // Wait for odac to be installed (poll for binary existence)
      // npm install runs at container startup, we need to wait for it to finish
      progress('setup', 'progress', __('Waiting for dependencies installation...'))
      let installed = false
      for (let i = 0; i < 60; i++) {
        // Max 120 seconds
        await new Promise(resolve => setTimeout(resolve, 2000))
        try {
          // Check if odac bin exists by trying to get version
          await Odac.server('Container').execInContainer(domain, './node_modules/.bin/odac --version')
          installed = true
          break
        } catch {
          // Ignore error, probably not installed yet or container not ready
        }
      }

      if (installed) {
        progress('setup', 'progress', __('Running initial setup for %s...', domain))
        await Odac.server('Container').execInContainer(domain, './node_modules/.bin/odac init')
        progress('setup', 'success', __('Initial setup completed.'))
      } else {
        progress('setup', 'error', __('Dependency installation timed out. Please check container logs.'))
      }
    } else {
      // Run directly on host
      childProcess.execSync('npm init -y', {cwd: web.path})
      childProcess.execSync('npm i odac', {cwd: web.path})
      childProcess.execSync('./node_modules/.bin/odac init', {cwd: web.path})

      // Process package.json template after copying
      const packageJsonPath = path.join(web.path, 'package.json')
      try {
        let packageTemplate = fs.readFileSync(packageJsonPath, 'utf8')

        // Replace template variables
        packageTemplate = packageTemplate.replace(/\{\{domain\}\}/g, domain.replace(/\./g, '-')).replace(/\{\{domain_original\}\}/g, domain)

        fs.writeFileSync(packageJsonPath, packageTemplate)
      } catch (err) {
        // Prepare package.json if it doesn't exist or ignore read errors
        if (err.code === 'ENOENT') {
          // Optional: handle missing file if critical, or just ignore as before
        }
      }
      progress('directory', 'success', __('Website files for %s set.', domain))
    }

    return Odac.server('Api').result(true, __('Website %s created at %s.', web.domain, web.path))
  }

  async delete(domain) {
    for (const iterator of ['http://', 'https://', 'ftp://', 'www.']) if (domain.startsWith(iterator)) domain = domain.replace(iterator, '')
    if (!Odac.core('Config').config.websites[domain]) return Odac.server('Api').result(false, __('Website %s not found.', domain))
    const website = Odac.core('Config').config.websites[domain]
    delete Odac.core('Config').config.websites[domain]

    // Stop process if running
    if (website.pid) {
      if (Odac.server('Container').available) {
        await Odac.server('Container').remove(domain)
      } else {
        Odac.core('Process').stop(website.pid)
      }

      delete this.#watcher[website.pid]
      if (website.port) {
        delete this.#ports[website.port]
      }
    }

    // Cleanup logs
    delete this.#logs.log[domain]
    delete this.#logs.err[domain]
    delete this.#error_counts[domain]
    delete this.#active[domain]
    delete this.#started[domain]

    this.syncConfig()
    return Odac.server('Api').result(true, __('Website %s deleted.', domain))
  }

  index(req, res) {
    res.write('ODAC Server')
    res.end()
  }

  async init() {
    this.#loaded = true
    this.server()
    if (!Odac.core('Config').config.web?.path || !fs.existsSync(Odac.core('Config').config.web.path)) {
      if (!Odac.core('Config').config.web) Odac.core('Config').config.web = {}
      // Check environment variable first (Docker support)
      if (process.env.ODAC_WEB_PATH) {
        Odac.core('Config').config.web.path = process.env.ODAC_WEB_PATH
      } else if (os.platform() === 'win32' || os.platform() === 'darwin') {
        Odac.core('Config').config.web.path = os.homedir() + '/Odac/'
      } else {
        Odac.core('Config').config.web.path = '/var/odac/'
      }
    }

    // Start Go Proxy with a slight delay to ensure config loads or immediate
    // this.spawnProxy() -> Moved to start()
  }

  async list() {
    let websites = Object.keys(Odac.core('Config').config.websites ?? {})
    if (websites.length == 0) return Odac.server('Api').result(false, __('No websites found.'))
    return Odac.server('Api').result(true, __('Websites:') + '\n  ' + websites.join('\n  '))
  }

  request(req, res, secure) {
    const result = this.#firewall.check(req)
    if (!result.allowed) {
      if (result.reason === 'blacklist') {
        res.writeHead(403, {'Content-Type': 'text/plain'})
        res.end('Forbidden')
      } else {
        res.writeHead(429, {'Content-Type': 'text/plain'})
        res.end('Too Many Requests')
      }
      return
    }

    let host = req.headers.host || req.headers[':authority']
    if (!host) return this.index(req, res)

    // Remove port from host
    if (host.includes(':')) {
      host = host.split(':')[0]
    }

    // Find matching website (check subdomains)
    let matchedHost = host
    while (!Odac.core('Config').config.websites[matchedHost] && matchedHost.includes('.')) {
      matchedHost = matchedHost.split('.').slice(1).join('.')
    }

    const website = Odac.core('Config').config.websites[matchedHost]
    if (!website) return this.index(req, res)
    if (!website.pid || !this.#watcher[website.pid]) return this.index(req, res)

    try {
      if (!secure) {
        res.writeHead(301, {Location: 'https://' + host + (req.url || req.headers[':path'] || '/')})
        return res.end()
      }

      if (req.httpVersion === '2.0') {
        if (!req.url && req.headers[':path']) {
          req.url = req.headers[':path']
        }
        if (!req.method && req.headers[':method']) {
          req.method = req.headers[':method'].toUpperCase()
        }
        if (!req.headers.host) {
          req.headers.host = host
        }
        return this.#proxy.http2(req, res, website, host)
      }

      return this.#proxy.http1(req, res, website, host)
    } catch (e) {
      log(e)
      return this.index(req, res)
    }
  }

  spawnProxy() {
    const isWindows = os.platform() === 'win32'
    const proxyName = isWindows ? 'odac-proxy.exe' : 'odac-proxy'
    const binPath = path.resolve(__dirname, '../../bin', proxyName)
    const runDir = path.join(os.homedir(), '.odac', 'run')
    const logDir = path.join(os.homedir(), '.odac', 'logs')

    if (!fs.existsSync(runDir)) fs.mkdirSync(runDir, {recursive: true})
    if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, {recursive: true})

    const pidFile = path.join(runDir, 'proxy.pid')
    const logFile = path.join(logDir, 'proxy.log')

    // Set fixed socket path
    if (!isWindows) {
      this.#proxySocketPath = path.join(runDir, 'proxy.sock')
    }

    // 1. Try to adopt existing process
    if (fs.existsSync(pidFile)) {
      try {
        const pid = parseInt(fs.readFileSync(pidFile, 'utf8'))
        // Check if running
        process.kill(pid, 0)

        log(`Found orphaned Go Proxy (PID: ${pid}). Reconnecting...`)

        // Create a fake process object to manage it
        this.#proxyProcess = {
          pid,
          kill: () => {
            try {
              process.kill(pid)
            } catch {
              /* ignore */
            }
          }
        }

        // Sync config immediately
        this.syncConfig()
        return
      } catch {
        // Process dead
        log(`Orphaned proxy PID file exists but process is dead. Cleaning up.`)
        try {
          fs.unlinkSync(pidFile)
        } catch {
          /* ignore */
        }
      }
    }

    if (!fs.existsSync(binPath)) {
      error(`Go proxy binary not found at ${binPath}. Please run 'go build -o bin/${proxyName} ./server/proxy'`)
      return
    }

    // 2. Start new Proxy
    let env = {...process.env}

    if (!isWindows) {
      env.ODAC_SOCKET_PATH = this.#proxySocketPath
      log(`Starting Go Proxy (Socket: ${this.#proxySocketPath})...`)
    } else {
      log(`Starting Go Proxy (TCP Mode)...`)
    }

    try {
      const logFd = fs.openSync(logFile, 'a')

      this.#proxyProcess = childProcess.spawn(binPath, [], {
        detached: true, // Allow running after parent exit
        stdio: ['ignore', logFd, logFd], // Redirect logs to file
        env: env
      })

      this.#proxyProcess.unref() // Don't prevent Node from exiting

      if (this.#proxyProcess.pid) {
        fs.writeFileSync(pidFile, this.#proxyProcess.pid.toString())
        log(`Go Proxy started with PID ${this.#proxyProcess.pid}`)
      }

      this.#proxyProcess.on('exit', code => {
        error(`Go Proxy exited with code ${code}`)
        this.#proxyProcess = null
        try {
          fs.unlinkSync(pidFile)
        } catch {
          /* ignore */
        }
      })

      // Give it a moment to start
      setTimeout(() => this.syncConfig(), 1000)
    } catch (err) {
      error(`Failed to spawn Go Proxy: ${err.message}`)
    }
  }

  async syncConfig() {
    if (typeof Odac === 'undefined') return
    if (!this.#proxyProcess) return
    if (!this.#proxySocketPath && !this.#proxyApiPort) return

    // Ensure socket exists before sending
    if (this.#proxySocketPath && !fs.existsSync(this.#proxySocketPath)) {
      // Socket not ready yet
      return
    }

    const config = {
      websites: Odac.core('Config').config.websites || {},
      firewall: Odac.core('Config').config.firewall || {enabled: true},
      ssl: Odac.core('Config').config.ssl || null
    }

    try {
      if (this.#proxySocketPath) {
        // Unix Socket Request
        await axios.post('http://localhost/config', config, {
          socketPath: this.#proxySocketPath,
          validateStatus: () => true
        })
      } else {
        // TCP Request
        await axios.post(`http://127.0.0.1:${this.#proxyApiPort}/config`, config)
      }
    } catch (e) {
      error(`Failed to sync config to proxy: ${e.message}`)
    }
  }

  server() {
    // Legacy server method replaced by Go Proxy
    // Only kept if called by check() repeatedly
    if (!this.#proxyProcess) this.spawnProxy()
  }

  // Removed #handleUpgrade as it is handled by Go Proxy

  set(domain, data) {
    Odac.core('Config').config.websites[domain] = data
  }

  start(domain) {
    // If domain provided, start specific site container/process
    if (domain) return this.#startSite(domain)

    // Otherwise start the main Web Proxy service
    this.spawnProxy()
  }

  stop() {
    if (this.#proxyProcess) {
      this.#proxyProcess.kill() // SIGTERM
      this.#proxyProcess = null
      this.#proxyApiPort = null
      if (this.#proxySocketPath && fs.existsSync(this.#proxySocketPath)) {
        try {
          fs.unlinkSync(this.#proxySocketPath)
        } catch {
          /* ignore */
        }
      }
    }
  }

  async #startSite(domain) {
    if (this.#active[domain] || !this.#loaded) return
    this.#active[domain] = true
    if (!Odac.core('Config').config.websites[domain]) return (this.#active[domain] = false)
    if (
      Odac.core('Config').config.websites[domain].status == 'errored' &&
      Date.now() - Odac.core('Config').config.websites[domain].updated < this.#error_counts[domain] * 1000
    )
      return (this.#active[domain] = false)
    let port = 60000
    let using = false
    do {
      if (this.#ports[port]) {
        port++
        using = true
      } else {
        if (this.checkPort(port)) {
          using = false
        } else {
          port++
          using = true
        }
      }
      if (port > 65535) {
        port = 1000
        using = true
      }
    } while (using)
    Odac.core('Config').config.websites[domain].port = port
    this.#ports[port] = true

    let child
    let isDocker = false

    if (Odac.server('Container').available) {
      const isRunning = await Odac.server('Container').isRunning(domain)
      let success = false

      if (isRunning) {
        log(`Container for ${domain} is already running. Attaching logs...`)
        success = true
        isDocker = true
      } else {
        // Run via Docker
        const extraBinds = Odac.core('Config').config.websites[domain].volumes || []
        const env = {
          ODAC_API_HOST: 'host.docker.internal',
          ODAC_API_PORT: 1453,
          ODAC_API_KEY: Odac.core('Config').config.api.auth,
          ODAC_API_SOCKET: '/run/odac/api.sock',
          ODAC_HOST: '0.0.0.0'
        }
        success = await Odac.server('Container').run(domain, port, Odac.core('Config').config.websites[domain].path, extraBinds, {
          env
        })
        if (success) isDocker = true
      }

      if (success) {
        child = await Odac.server('Container').logs(domain)

        // Whitelist container IP for API access
        const containerIP = await Odac.server('Container').getIP(domain)
        Odac.core('Config').config.websites[domain].container = domain
        if (containerIP) {
          Odac.server('Api').allow(containerIP)
          Odac.core('Config').config.websites[domain].containerIP = containerIP
          log(`Whitelisted API access for ${domain} (${containerIP})`)
        }

        log('Web container started for ' + domain)
      } else {
        error('Failed to start container for ' + domain)
        return
      }
    } else {
      // Run Local
      const websitePath = Odac.core('Config').config.websites[domain].path
      try {
        const packageJsonPath = path.join(websitePath, 'package.json')
        if (fs.existsSync(packageJsonPath)) {
          const pkg = JSON.parse(fs.readFileSync(packageJsonPath, 'utf8'))
          if (pkg.scripts && pkg.scripts.build) {
            log('Building website ' + domain + '...')
            await new Promise((resolve, reject) => {
              const buildChild = childProcess.spawn('npm', ['run', 'build'], {
                cwd: websitePath,
                stdio: 'ignore'
              })
              buildChild.on('close', code => {
                if (code === 0) resolve()
                else reject(new Error('Build failed with code ' + code))
              })
              buildChild.on('error', err => reject(err))
            })
          }
        }
      } catch (e) {
        error('Failed to build website ' + domain + ': ' + e.message)
      }

      let startCommand = 'odac'
      let startArgs = ['framework', 'run', port]

      try {
        const packageJsonPath = path.join(websitePath, 'package.json')
        if (fs.existsSync(packageJsonPath)) {
          const pkg = JSON.parse(fs.readFileSync(packageJsonPath, 'utf8'))
          if (pkg.scripts && pkg.scripts.start) {
            log('Starting website ' + domain + ' using npm run start...')
            startCommand = 'npm'
            startArgs = ['run', 'start', '--', port]
          }
        }
      } catch {
        // Ignore JSON errors or read errors, fallback to default
      }

      child = childProcess.spawn(startCommand, startArgs, {
        cwd: websitePath
      })
      log('Web server started for ' + domain + ' with PID ' + child.pid)
    }

    // Dockerode returns a stream that doesn't have a pid property (it is undefined)
    // We use a dummy PID for internal tracking if check fails
    let pid = child.pid || domain

    // Dockerode streams handling
    if (isDocker && child) {
      const stdoutStream = new (require('stream').PassThrough)()
      const stderrStream = new (require('stream').PassThrough)()

      Odac.server('Container').docker.modem.demuxStream(child, stdoutStream, stderrStream)

      stdoutStream.on('data', data => {
        if (!this.#logs.log[domain]) this.#logs.log[domain] = ''
        this.#logs.log[domain] += '[LOG][' + Date.now() + '] ' + data.toString().trim() + '\n'
        if (this.#logs.log[domain].length > 100000)
          this.#logs.log[domain] = this.#logs.log[domain].substr(this.#logs.log[domain].length - 1000000)
        if (Odac.core('Config').config.websites[domain] && Odac.core('Config').config.websites[domain].status == 'errored')
          Odac.core('Config').config.websites[domain].status = 'running'
      })

      stderrStream.on('data', data => {
        if (!this.#logs.err[domain]) this.#logs.err[domain] = ''
        if (!this.#logs.log[domain]) this.#logs.log[domain] = ''

        this.#logs.log[domain] +=
          '[ERR][' +
          Date.now() +
          '] ' +
          data
            .toString()
            .trim()
            .split('\n')
            .join('\n[ERR][' + Date.now() + '] ') +
          '\n'

        this.#logs.err[domain] += data.toString()
        if (this.#logs.err[domain].length > 100000)
          this.#logs.err[domain] = this.#logs.err[domain].substr(this.#logs.err[domain].length - 1000000)
        if (Odac.core('Config').config.websites[domain]) Odac.core('Config').config.websites[domain].status = 'errored'
      })

      child.on('end', () => {
        // Handle exit
        child.emit('exit')
      })
    } else if (child) {
      // Child Process standard handling
      child.stdout.on('data', data => {
        if (!this.#logs.log[domain]) this.#logs.log[domain] = ''
        this.#logs.log[domain] +=
          '[LOG][' +
          Date.now() +
          '] ' +
          data
            .toString()
            .trim()
            .split('\n')
            .join('\n[LOG][' + Date.now() + '] ') +
          '\n'
        if (this.#logs.log[domain].length > 100000)
          this.#logs.log[domain] = this.#logs.log[domain].substr(this.#logs.log[domain].length - 1000000)
        if (Odac.core('Config').config.websites[domain] && Odac.core('Config').config.websites[domain].status == 'errored')
          Odac.core('Config').config.websites[domain].status = 'running'
      })
      child.stderr.on('data', data => {
        if (!this.#logs.err[domain]) this.#logs.err[domain] = ''
        this.#logs.log[domain] +=
          '[ERR][' +
          Date.now() +
          '] ' +
          data
            .toString()
            .trim()
            .split('\n')
            .join('\n[ERR][' + Date.now() + '] ') +
          '\n'
        this.#logs.err[domain] += data.toString()
        if (this.#logs.err[domain].length > 100000)
          this.#logs.err[domain] = this.#logs.err[domain].substr(this.#logs.err[domain].length - 1000000)
        if (Odac.core('Config').config.websites[domain]) Odac.core('Config').config.websites[domain].status = 'errored'
      })
    }

    if (child) {
      child.on('exit', () => {
        error((isDocker ? 'Container log stream' : 'Child process') + ' exited for ' + domain)

        if (isDocker) {
          // If the log stream ended, the container likely stopped or crashed
          Odac.server('Container').stop(domain)
        }

        if (!Odac.core('Config').config.websites[domain]) return
        Odac.core('Config').config.websites[domain].pid = null
        Odac.core('Config').config.websites[domain].updated = Date.now()
        if (Odac.core('Config').config.websites[domain].status == 'errored') {
          Odac.core('Config').config.websites[domain].status = 'errored'
          this.#error_counts[domain] = this.#error_counts[domain] ?? 0
          this.#error_counts[domain]++
        } else Odac.core('Config').config.websites[domain].status = 'stopped'
        this.#watcher[pid] = false
        delete this.#ports[Odac.core('Config').config.websites[domain].port]

        // Cleanup whitelisted IP
        if (Odac.core('Config').config.websites[domain].containerIP) {
          Odac.server('Api').disallow(Odac.core('Config').config.websites[domain].containerIP)
          delete Odac.core('Config').config.websites[domain].containerIP
        }

        this.#active[domain] = false
      })
    }

    Odac.core('Config').config.websites[domain].pid = pid
    Odac.core('Config').config.websites[domain].started = Date.now()
    Odac.core('Config').config.websites[domain].status = 'running'
    this.#watcher[pid] = true
    this.#started[domain] = Date.now()
    this.syncConfig()
  }

  async status() {
    this.init()
    return Odac.core('Config').config.websites
  }

  stopAll() {
    for (const domain of Object.keys(Odac.core('Config').config.websites ?? {})) {
      let website = Odac.core('Config').config.websites[domain]
      if (website.pid) {
        if (Odac.server('Container').available) {
          Odac.server('Container').stop(domain)
        } else {
          Odac.core('Process').stop(website.pid)
        }
        website.pid = null
        this.set(domain, website)
      }
    }
    this.syncConfig()
  }

  // Test helper
  reset() {
    this.#proxyProcess = null
    this.#proxySocketPath = null
    this.#proxyApiPort = null
    if (this.#firewall && this.#firewall.reset) this.#firewall.reset()
  }
}

module.exports = new Web()
