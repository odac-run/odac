const {log, error} = Odac.core('Log', false).init('Web')

const childProcess = require('child_process')
const fs = require('fs')
const http = require('http')
const https = require('https')
const http2 = require('http2')
const net = require('net')
const os = require('os')
const path = require('path')
const tls = require('tls')

const WebProxy = require('./Web/Proxy.js')
const WebFirewall = require('./Web/Firewall.js')
const WebSocketProxy = require('./Web/WebSocket.js')

class Web {
  #active = {}
  #error_counts = {}
  #loaded = false
  #log
  #logs = {log: {}, err: {}}
  #sslCache = new Map()
  #ports = {}
  #proxy
  #firewall
  #server_http
  #server_https
  #started = {}
  #watcher = {}

  #wsProxy

  constructor() {
    this.#log = log
    this.#proxy = new WebProxy(this.#log)
    this.#firewall = new WebFirewall()
    this.#wsProxy = new WebSocketProxy(this.#log)
  }

  clearSSLCache(domain) {
    if (domain) {
      for (const key of this.#sslCache.keys()) {
        if (key === domain || key.endsWith('.' + domain)) {
          this.#sslCache.delete(key)
        }
      }
    } else {
      this.#sslCache.clear()
    }
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

  server() {
    if (!this.#loaded) return setTimeout(() => this.server(), 1000)
    if (Object.keys(Odac.core('Config').config.websites ?? {}).length == 0) return

    if (!this.#server_http) {
      this.#server_http = http.createServer((req, res) => this.request(req, res, false))
      this.#server_http.on('upgrade', (req, socket, head) => this.#handleUpgrade(req, socket, head, false))
      this.#server_http.on('error', err => {
        log(`HTTP server error: ${err.message}`)
        if (err.code === 'EADDRINUSE') {
          log('Port 80 is already in use')
        }
      })
      this.#server_http.listen(80)
    }

    let ssl = Odac.core('Config').config.ssl ?? {}
    if (!this.#server_https && ssl && ssl.key && ssl.cert && fs.existsSync(ssl.key) && fs.existsSync(ssl.cert)) {
      const useHttp2 = Odac.core('Config').config.http2 !== false

      const serverOptions = {
        SNICallback: (hostname, callback) => {
          try {
            const cached = this.#sslCache.get(hostname)
            if (cached) return callback(null, cached)

            let sslOptions
            while (!Odac.core('Config').config.websites[hostname] && hostname.includes('.'))
              hostname = hostname.split('.').slice(1).join('.')
            let website = Odac.core('Config').config.websites[hostname]
            if (
              website &&
              website.cert &&
              website.cert.ssl &&
              website.cert.ssl.key &&
              website.cert.ssl.cert &&
              fs.existsSync(website.cert.ssl.key) &&
              fs.existsSync(website.cert.ssl.cert)
            ) {
              sslOptions = {
                key: fs.readFileSync(website.cert.ssl.key),
                cert: fs.readFileSync(website.cert.ssl.cert)
              }
            } else {
              sslOptions = {
                key: fs.readFileSync(ssl.key),
                cert: fs.readFileSync(ssl.cert)
              }
            }
            const ctx = tls.createSecureContext(sslOptions)
            this.#sslCache.set(hostname, ctx)
            callback(null, ctx)
          } catch (err) {
            log(`SSL certificate error for ${hostname}: ${err.message}`)
            callback(err)
          }
        },
        key: fs.readFileSync(ssl.key),
        cert: fs.readFileSync(ssl.cert),
        sessionTimeout: 300,
        ciphers:
          'TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256:TLS_AES_256_GCM_SHA384:' +
          'ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:' +
          'ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:' +
          'ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305',
        honorCipherOrder: true
      }

      if (useHttp2) {
        serverOptions.allowHTTP1 = true
        this.#server_https = http2.createSecureServer(serverOptions, (req, res) => {
          this.request(req, res, true)
        })
        log('HTTPS server starting with HTTP/2 support enabled')
      } else {
        this.#server_https = https.createServer(serverOptions, (req, res) => {
          this.request(req, res, true)
        })
        log('HTTPS server starting with HTTP/1.1 only')
      }

      this.#server_https.on('upgrade', (req, socket, head) => this.#handleUpgrade(req, socket, head, true))
      this.#server_https.on('error', err => {
        log(`HTTPS server error: ${err.message}`)
        if (err.code === 'EADDRINUSE') {
          log('Port 443 is already in use')
        }
      })

      this.#server_https.listen(443)
    }
  }

  #handleUpgrade(req, socket, head, secure) {
    let host = req.headers.host
    if (!host) {
      socket.destroy()
      return
    }

    if (host.includes(':')) {
      host = host.split(':')[0]
    }

    let matchedHost = host
    while (!Odac.core('Config').config.websites[matchedHost] && matchedHost.includes('.')) {
      matchedHost = matchedHost.split('.').slice(1).join('.')
    }

    const website = Odac.core('Config').config.websites[matchedHost]
    if (!website || !website.pid || !this.#watcher[website.pid]) {
      socket.destroy()
      return
    }

    if (!secure) {
      socket.write('HTTP/1.1 301 Moved Permanently\r\nLocation: wss://' + host + req.url + '\r\n\r\n')
      socket.destroy()
      return
    }

    this.#wsProxy.upgrade(req, socket, head, website, host)
  }

  set(domain, data) {
    Odac.core('Config').config.websites[domain] = data
  }

  async start(domain) {
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
      // Run via Docker
      const extraBinds = Odac.core('Config').config.websites[domain].volumes || []
      const env = {
        ODAC_API_HOST: 'host.docker.internal',
        ODAC_API_PORT: 1453,
        ODAC_API_KEY: Odac.core('Config').config.api.auth,
        ODAC_API_SOCKET: '/run/odac/api.sock'
      }
      const success = await Odac.server('Container').run(domain, port, Odac.core('Config').config.websites[domain].path, extraBinds, {
        env
      })
      if (success) {
        isDocker = true
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
      child = childProcess.spawn('odac', ['framework', 'run', port], {
        cwd: Odac.core('Config').config.websites[domain].path
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
  }
}

module.exports = new Web()
