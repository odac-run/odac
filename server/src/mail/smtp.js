const {log, error} = Odac.core('Log', false).init('Mail', 'SMTP')

const nodeCrypto = require('crypto')
const dns = require('dns')
const net = require('net')
const tls = require('tls')
const fs = require('fs')
const {promisify} = require('util')
const DKIMSign = require('dkim-signer').DKIMSign

// DNS resolver promisify
const resolveMx = promisify(dns.resolveMx)

class smtp {
  constructor() {
    // Configuration with defaults
    this.config = {
      timeout: 30000, // 30 seconds
      retryAttempts: 3,
      retryDelay: 1000, // 1 second
      maxConnections: 10,
      ports: [25, 587, 465, 2525],
      enableAuth: true,
      enableDKIM: true,
      maxEmailSize: 25 * 1024 * 1024, // 25MB
      connectionPoolTimeout: 300000, // 5 minutes
      dnsTimeout: 10000, // 10 seconds
      rateLimitPerHour: 1000,
      tls: {
        minVersion: 'TLSv1.2',
        ciphers: [
          'ECDHE-RSA-AES256-GCM-SHA384',
          'ECDHE-RSA-AES128-GCM-SHA256',
          'ECDHE-RSA-AES256-SHA384',
          'ECDHE-RSA-AES128-SHA256',
          'AES256-GCM-SHA384',
          'AES128-GCM-SHA256',
          'AES256-SHA256',
          'AES128-SHA256',
          'HIGH',
          '!aNULL',
          '!eNULL',
          '!EXPORT',
          '!DES',
          '!RC4',
          '!MD5',
          '!PSK',
          '!SRP',
          '!CAMELLIA'
        ].join(':')
      }
    }

    // Connection pool and caches
    this.connectionPool = new Map()
    this.mxCache = new Map()
    this.rateLimiter = new Map()

    // Cleanup interval for connection pool
    setInterval(() => this.#cleanupConnections(), 60000) // 1 minute
  }

  #validateEmailObject(obj) {
    if (!obj || typeof obj !== 'object') {
      throw new Error('Invalid email object')
    }

    if (!obj.from || !obj.from.value || !Array.isArray(obj.from.value) || !obj.from.value[0]?.address) {
      throw new Error('Invalid sender address')
    }

    if (!obj.to || !obj.to.value || !Array.isArray(obj.to.value) || obj.to.value.length === 0) {
      throw new Error('Invalid recipient addresses')
    }

    // Email address validation
    const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
    if (!emailRegex.test(obj.from.value[0].address)) {
      throw new Error('Invalid sender email format')
    }

    for (const recipient of obj.to.value) {
      if (!emailRegex.test(recipient.address)) {
        throw new Error(`Invalid recipient email format: ${recipient.address}`)
      }
    }

    // Content validation
    if (!obj.text && !obj.html && (!obj.attachments || obj.attachments.length === 0)) {
      throw new Error('Email must have content (text, html, or attachments)')
    }

    return true
  }

  #sanitizeInput(input) {
    if (typeof input !== 'string') return input
    // Prevent SMTP injection
    return input.replace(/[\r\n]/g, '').substring(0, 1000)
  }

  #checkRateLimit(domain) {
    const now = Date.now()
    const hourAgo = now - 3600000 // 1 hour

    if (!this.rateLimiter.has(domain)) {
      this.rateLimiter.set(domain, [])
    }

    const timestamps = this.rateLimiter.get(domain)
    // Remove old timestamps
    const recentTimestamps = timestamps.filter(ts => ts > hourAgo)

    if (recentTimestamps.length >= this.config.rateLimitPerHour) {
      throw new Error(`Rate limit exceeded for domain ${domain}`)
    }

    recentTimestamps.push(now)
    this.rateLimiter.set(domain, recentTimestamps)
  }

  #commandWithTimeout(socket, command, timeoutMs = this.config.timeout) {
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        socket.removeAllListeners('data')
        reject(new Error(`Command timeout: ${command.trim()}`))
      }, timeoutMs)

      socket.once('data', data => {
        clearTimeout(timeout)
        const response = data.toString()
        log('SMTP Response', response.trim())
        resolve(response)
      })

      socket.once('error', err => {
        clearTimeout(timeout)
        reject(err)
      })

      try {
        if (socket.writable) {
          socket.write(command)
        } else {
          reject(new Error('Socket not writable'))
        }
      } catch (err) {
        clearTimeout(timeout)
        reject(err)
      }
    })
  }

  #cleanupConnections() {
    const now = Date.now()
    for (const [key, connection] of this.connectionPool.entries()) {
      if (now - connection.lastUsed > this.config.connectionPoolTimeout) {
        try {
          connection.socket.end()
        } catch {
          // Ignore cleanup errors - connection already closed
        }
        this.connectionPool.delete(key)
        log('Connection Pool', `Cleaned up connection to ${key}`)
      }
    }
  }

  #encodeQuotedPrintable(str) {
    const buffer = Buffer.from(str, 'utf-8')
    let result = ''
    let lineLength = 0

    for (const byte of buffer) {
      if ((byte >= 33 && byte <= 60) || (byte >= 62 && byte <= 126) || byte === 9 || byte === 32) {
        if (lineLength + 1 > 75) {
          result += '=\r\n'
          lineLength = 0
        }
        result += String.fromCharCode(byte)
        lineLength++
      } else if (byte === 13 || byte === 10) {
        result += String.fromCharCode(byte)
        lineLength = 0
      } else {
        const encoded = '=' + byte.toString(16).toUpperCase().padStart(2, '0')
        if (lineLength + 3 > 75) {
          result += '=\r\n'
          lineLength = 0
        }
        result += encoded
        lineLength += 3
      }
    }

    return result.replace(/[ \t]+$/gm, match => {
      return match.replace(/./g, char => '=' + char.charCodeAt(0).toString(16).toUpperCase().padStart(2, '0'))
    })
  }

  #encodeBase64(buffer) {
    return buffer.toString('base64').replace(/(.{76})/g, '$1\r\n')
  }

  async #authenticateSocket(socket, username, password) {
    if (!this.config.enableAuth) return true

    try {
      // Try AUTH LOGIN
      let response = await this.#commandWithTimeout(socket, 'AUTH LOGIN\r\n')
      if (response.startsWith('334')) {
        // Send username
        response = await this.#commandWithTimeout(socket, Buffer.from(username).toString('base64') + '\r\n')
        if (response.startsWith('334')) {
          // Send password
          response = await this.#commandWithTimeout(socket, Buffer.from(password).toString('base64') + '\r\n')
          if (response.startsWith('235')) {
            log('SMTP Auth', 'Authentication successful')
            return true
          }
        }
      }

      // Try AUTH PLAIN as fallback
      const authString = Buffer.from(`\0${username}\0${password}`).toString('base64')
      response = await this.#commandWithTimeout(socket, `AUTH PLAIN ${authString}\r\n`)
      if (response.startsWith('235')) {
        log('SMTP Auth', 'PLAIN authentication successful')
        return true
      }

      log('SMTP Auth', 'Authentication failed')
      return false
    } catch (err) {
      error('SMTP Auth Error', err.message)
      return false
    }
  }

  async #getConnectionFromPool(host, port) {
    const key = `${host}:${port}`
    const connection = this.connectionPool.get(key)

    if (connection && connection.socket.readyState === 'open') {
      connection.lastUsed = Date.now()
      // Remove default error listener added when pooling
      connection.socket.removeAllListeners('error')
      log('Connection Pool', `Reusing connection to ${key}`)
      return connection.socket
    }

    if (connection) {
      this.connectionPool.delete(key)
    }

    return null
  }

  #addToConnectionPool(host, port, socket) {
    const key = `${host}:${port}`
    if (this.connectionPool.size >= this.config.maxConnections) {
      // Remove oldest connection
      const oldestKey = this.connectionPool.keys().next().value
      const oldConnection = this.connectionPool.get(oldestKey)
      try {
        oldConnection.socket.end()
      } catch {
        // Ignore cleanup errors
      }
      this.connectionPool.delete(oldestKey)
    }

    // Clean up listeners before pooling to prevent memory leaks
    socket.removeAllListeners('timeout')
    socket.removeAllListeners('data')
    socket.removeAllListeners('error')

    // Add a default error listener to catch background errors while allowed in pool
    socket.on('error', err => {
      log('Connection Pool', `Background socket error for ${key}: ${err.message}`)
      this.connectionPool.delete(key)
      try {
        socket.destroy()
      } catch {
        // Ignore cleanup errors
      }
    })

    this.connectionPool.set(key, {
      socket: socket,
      lastUsed: Date.now()
    })
    log('Connection Pool', `Added connection to ${key}`)
  }

  async #connectWithRetry(sender, host, port, retryCount = 0) {
    try {
      return await this.#connect(sender, host, port)
    } catch (err) {
      if (retryCount < this.config.retryAttempts) {
        log('SMTP Retry', `Retrying connection to ${host}:${port} (attempt ${retryCount + 1})`)
        await new Promise(resolve => setTimeout(resolve, this.config.retryDelay * (retryCount + 1)))
        return await this.#connectWithRetry(sender, host, port, retryCount + 1)
      }
      throw err
    }
  }

  #connect(sender, host, port) {
    return new Promise((resolve, reject) => {
      // Check connection pool first
      this.#getConnectionFromPool(host, port)
        .then(pooledSocket => {
          if (pooledSocket) {
            return resolve(pooledSocket)
          }

          let socket
          const timeout = setTimeout(() => {
            if (socket) {
              socket.destroy()
            }
            reject(new Error(`Connection timeout to ${host}:${port}`))
          }, this.config.timeout)

          const cleanup = () => {
            clearTimeout(timeout)
          }

          if (port == 465) {
            socket = tls.connect(
              {
                host: host,
                port: port,
                rejectUnauthorized: false,
                timeout: this.config.timeout,
                ...this.config.tls
              },
              async () => {
                cleanup()
                try {
                  socket.setEncoding('utf8')
                  await new Promise(resolve => socket.once('data', resolve))
                  await this.#commandWithTimeout(socket, `EHLO ${this.#sanitizeInput(sender)}\r\n`)
                  this.#addToConnectionPool(host, port, socket)
                  resolve(socket)
                } catch (err) {
                  socket.destroy()
                  reject(err)
                }
              }
            )

            socket.on('error', err => {
              if (err.code === 'ERR_SSL_NO_SHARED_CIPHER') {
                error('TLS Cipher Error on port 465 - Trying fallback:', err.message)
                // Try with minimal TLS configuration
                const fallbackSocket = tls.connect({
                  host: host,
                  port: port,
                  rejectUnauthorized: false,
                  timeout: this.config.timeout,
                  minVersion: 'TLSv1.2'
                })
                fallbackSocket.on('secureConnect', async () => {
                  log('Fallback SSL connection successful on port 465')
                  try {
                    fallbackSocket.setEncoding('utf8')
                    await new Promise(resolve => fallbackSocket.once('data', resolve))
                    await this.#commandWithTimeout(fallbackSocket, `EHLO ${this.#sanitizeInput(sender)}\r\n`)
                    this.#addToConnectionPool(host, port, fallbackSocket)
                    resolve(fallbackSocket)
                  } catch (fallbackErr) {
                    fallbackSocket.destroy()
                    reject(fallbackErr)
                  }
                })
                fallbackSocket.on('error', fallbackErr => {
                  error('Fallback SSL connection also failed:', fallbackErr)
                  reject(err)
                })
              } else {
                error('SSL Error on port 465:', err)
                socket.destroy()
                reject(err)
              }
            })
          } else {
            socket = net.createConnection(port, host, async () => {
              cleanup()
              try {
                socket.setEncoding('utf8')
                socket.setTimeout(this.config.timeout)
                await new Promise(resolve => socket.once('data', resolve))
                let response = await this.#commandWithTimeout(socket, `EHLO ${this.#sanitizeInput(sender)}\r\n`)

                if (!response.startsWith('2') || !response.includes('STARTTLS')) {
                  this.#addToConnectionPool(host, port, socket)
                  return resolve(socket)
                }

                response = await this.#commandWithTimeout(socket, `STARTTLS\r\n`)
                if (!response.startsWith('2')) {
                  this.#addToConnectionPool(host, port, socket)
                  return resolve(socket)
                }

                socket.removeAllListeners()
                socket = tls.connect(
                  {
                    socket: socket,
                    servername: host,
                    rejectUnauthorized: false,
                    ...this.config.tls
                  },
                  async () => {
                    try {
                      socket.setEncoding('utf8')
                      await new Promise(resolve => setTimeout(resolve, 1000))
                      response = await this.#commandWithTimeout(socket, `EHLO ${this.#sanitizeInput(sender)}\r\n`)
                      this.#addToConnectionPool(host, port, socket)
                      resolve(socket)
                    } catch (err) {
                      socket.destroy()
                      reject(err)
                    }
                  }
                )

                socket.on('error', err => {
                  if (err.code === 'ERR_SSL_NO_SHARED_CIPHER') {
                    error('TLS Cipher Error - Trying fallback connection:', err.message)
                    // Try without custom cipher configuration as fallback
                    const fallbackSocket = tls.connect({
                      socket: socket,
                      servername: host,
                      rejectUnauthorized: false,
                      minVersion: 'TLSv1.2'
                    })
                    fallbackSocket.on('secureConnect', () => {
                      log('Fallback TLS connection successful')
                      resolve(fallbackSocket)
                    })
                    fallbackSocket.on('error', fallbackErr => {
                      error('Fallback connection also failed:', fallbackErr)
                      reject(err)
                    })
                  } else {
                    error('Error connecting to the server (TLS):', err)
                    socket.destroy()
                    reject(err)
                  }
                })
              } catch (err) {
                socket.destroy()
                reject(err)
              }
            })
          }

          socket.on('error', err => {
            cleanup()
            error('Error connecting to the server:', err)
            reject(err)
          })

          socket.on('timeout', () => {
            cleanup()
            socket.destroy()
            reject(new Error(`Socket timeout to ${host}:${port}`))
          })
        })
        .catch(reject)
    })
  }

  #content(obj) {
    try {
      let domain = obj.from.value[0].address.split('@')[1]
      let headers = obj.headerLines.map(header => `${header.line}`).join('\r\n')
      let content = ''

      if (obj.html.length || obj.attachments.length) {
        let boundary = headers.match(/boundary="(.*)"/)?.[1]
        if (!boundary) {
          boundary = 'boundary_' + nodeCrypto.randomBytes(16).toString('hex')
          headers = headers.replace(/Content-Type: multipart\/mixed/, `Content-Type: multipart/mixed; boundary="${boundary}"`)
        }

        if (obj.text.length) {
          content += `--${boundary}\r\nContent-Type: text/plain; charset="UTF-8"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n${this.#encodeQuotedPrintable(obj.text)}\r\n`
        }

        if (obj.html.length) {
          content += `--${boundary}\r\nContent-Type: text/html; charset="UTF-8"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n${this.#encodeQuotedPrintable(obj.html)}\r\n`
        }

        for (let attachment of obj.attachments) {
          if (!attachment.filename || !attachment.content) {
            error('Invalid attachment', attachment)
            continue
          }

          const encodedContent = Buffer.isBuffer(attachment.content)
            ? this.#encodeBase64(attachment.content)
            : Buffer.from(attachment.content).toString('base64')

          content += `--${boundary}\r\nContent-Type: ${attachment.contentType || 'application/octet-stream'}; name="${this.#sanitizeInput(attachment.filename)}"\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename="${this.#sanitizeInput(attachment.filename)}"\r\n\r\n${encodedContent}\r\n`
        }
        content += `--${boundary}--\r\n`
      } else {
        content = this.#encodeQuotedPrintable(obj.text || '')
      }

      if (content) content = content.replace(/\r\n/g, '\n').replace(/\n/g, '\r\n')

      let signature = ''
      if (this.config.enableDKIM) {
        try {
          let dkim = Odac.core('Config').config.websites[domain]?.cert?.dkim
          if (dkim && this.#validateDKIMConfig(dkim)) {
            signature = this.#dkim({
              header: headers,
              content: content,
              domain: domain,
              private: fs.readFileSync(dkim.private, 'utf8'),
              selector: dkim.selector || 'default'
            })
          }
        } catch (err) {
          error('DKIM Error', err.message)
          // Continue without DKIM if there's an error
        }
      }

      content = signature + (signature ? '\r\n' : '') + headers + '\r\n\r\n' + content + '\r\n'

      // Check email size
      const emailSize = Buffer.byteLength(content, 'utf8')
      if (emailSize > this.config.maxEmailSize) {
        throw new Error(`Email size (${emailSize} bytes) exceeds maximum allowed size (${this.config.maxEmailSize} bytes)`)
      }

      return content
    } catch (err) {
      error('Content generation error', err.message)
      throw err
    }
  }

  #validateDKIMConfig(dkim) {
    if (!dkim || !dkim.private) {
      return false
    }

    try {
      // Check if private key file exists and is readable
      if (!fs.existsSync(dkim.private)) {
        error('DKIM Error', `Private key file not found: ${dkim.private}`)
        return false
      }

      const stats = fs.statSync(dkim.private)
      if (!stats.isFile()) {
        error('DKIM Error', `Private key path is not a file: ${dkim.private}`)
        return false
      }

      // Check file permissions (should not be world readable)
      if (stats.mode & 0o044) {
        error('DKIM Warning', `Private key file has loose permissions: ${dkim.private}`)
      }

      // Try to read the key to validate format
      const keyContent = fs.readFileSync(dkim.private, 'utf8')
      if (!keyContent.includes('BEGIN') || !keyContent.includes('PRIVATE KEY')) {
        error('DKIM Error', 'Invalid private key format')
        return false
      }

      return true
    } catch (err) {
      error('DKIM Validation Error', err.message)
      return false
    }
  }

  #dkim(obj) {
    try {
      const options = {
        domainName: obj.domain,
        keySelector: obj.selector,
        privateKey: obj.private,
        headerFieldNames: 'from:to:subject:date:message-id'
      }
      return DKIMSign(obj.header + '\r\n\r\n' + obj.content, options)
    } catch (err) {
      error('DKIM Signing Error', err.message)
      throw err
    }
  }

  async #host(domain) {
    // Check cache first
    if (this.mxCache.has(domain)) {
      const cached = this.mxCache.get(domain)
      if (Date.now() - cached.timestamp < 3600000) {
        // 1 hour cache
        log('DNS Cache', `Using cached MX for ${domain}: ${cached.host}`)
        return cached.host
      } else {
        this.mxCache.delete(domain)
      }
    }

    try {
      const addresses = await Promise.race([
        resolveMx(domain),
        new Promise((_, reject) => setTimeout(() => reject(new Error('DNS timeout')), this.config.dnsTimeout))
      ])

      if (!addresses || addresses.length === 0) {
        throw new Error(`No MX records found for ${domain}`)
      }

      addresses.sort((a, b) => a.priority - b.priority)
      const host = addresses[0].exchange

      // Cache the result
      this.mxCache.set(domain, {
        host: host,
        timestamp: Date.now()
      })

      log('DNS Resolution', `MX for ${domain}: ${host}`)
      return host
    } catch (err) {
      error('DNS Resolution Error', `Failed to resolve MX for ${domain}: ${err.message}`)
      throw new Error(`Failed to resolve MX records for ${domain}`)
    }
  }

  async #sendSingle(to, obj, retryCount = 0) {
    try {
      log('Mail', `Sending email to ${to}`)

      const domain = to.split('@')[1]
      this.#checkRateLimit(domain)

      const host = await this.#host(domain)
      const sender = obj.from.value[0].address.split('@')[1]

      let socket = null
      let lastError = null

      // Try different ports
      for (const port of this.config.ports) {
        try {
          socket = await this.#connectWithRetry(sender, host, port)
          if (socket) {
            log('Mail', `Connected to ${host}:${port}`)
            break
          }
        } catch (err) {
          lastError = err
          log('Mail', `Failed to connect to ${host}:${port} - ${err.message}`)
        }
      }

      if (!socket) {
        throw new Error(`Could not connect to any port for ${host}. Last error: ${lastError?.message}`)
      }

      try {
        // Authentication if configured
        const config = Odac.core('Config').config.websites[sender]
        if (config?.smtp?.auth) {
          const authSuccess = await this.#authenticateSocket(socket, config.smtp.username, config.smtp.password)
          if (!authSuccess) {
            throw new Error('SMTP authentication failed')
          }
        }

        let result = await this.#commandWithTimeout(socket, `MAIL FROM:<${this.#sanitizeInput(obj.from.value[0].address)}>\r\n`)
        if (!result.startsWith('2')) {
          throw new Error(`MAIL FROM rejected: ${result.trim()}`)
        }

        result = await this.#commandWithTimeout(socket, `RCPT TO:<${this.#sanitizeInput(to)}>\r\n`)
        if (!result.startsWith('2')) {
          throw new Error(`RCPT TO rejected: ${result.trim()}`)
        }

        result = await this.#commandWithTimeout(socket, `DATA\r\n`)
        if (!result.startsWith('2') && !result.startsWith('3')) {
          throw new Error(`DATA command rejected: ${result.trim()}`)
        }

        const emailContent = this.#content(obj)
        if (socket.writable) {
          socket.write(emailContent)
        } else {
          throw new Error('Socket became unwritable during DATA transfer')
        }

        result = await this.#commandWithTimeout(socket, `.\r\n`)
        if (!result.startsWith('2')) {
          throw new Error(`Email content rejected: ${result.trim()}`)
        }

        log('Mail', `Email sent successfully to ${to}`)
      } finally {
        // Don't close the socket, let it be reused from pool
        // if (socket) socket.end()
      }
    } catch (err) {
      if (retryCount < this.config.retryAttempts) {
        log('Mail Retry', `Retrying email to ${to} (attempt ${retryCount + 1}): ${err.message}`)
        await new Promise(resolve => setTimeout(resolve, this.config.retryDelay * (retryCount + 1)))
        return await this.#sendSingle(to, obj, retryCount + 1)
      } else {
        error('Mail Error', `Failed to send email to ${to} after ${this.config.retryAttempts} attempts: ${err.message}`)
        throw err
      }
    }
  }

  async send(obj) {
    try {
      // Validate email object
      this.#validateEmailObject(obj)

      log('Mail', `Starting to send email from ${obj.from.value[0].address} to ${obj.to.value.length} recipients`)

      const results = []
      const errors = []

      // Send to all recipients
      for (const recipient of obj.to.value) {
        try {
          await this.#sendSingle(recipient.address, obj)
          results.push({
            address: recipient.address,
            status: 'sent',
            timestamp: new Date().toISOString()
          })
        } catch (err) {
          const errorInfo = {
            address: recipient.address,
            status: 'failed',
            error: err.message,
            timestamp: new Date().toISOString()
          }
          results.push(errorInfo)
          errors.push(errorInfo)
          error('Mail Send Error', `Failed to send to ${recipient.address}: ${err.message}`)
        }
      }

      // Log summary
      const successful = results.filter(r => r.status === 'sent').length
      const failed = errors.length

      log('Mail Summary', `Email sending completed: ${successful} successful, ${failed} failed`)

      if (errors.length > 0) {
        log('Mail Errors', `Failed recipients: ${errors.map(e => e.address).join(', ')}`)
      }

      return {
        total: obj.to.value.length,
        successful: successful,
        failed: failed,
        results: results,
        errors: errors
      }
    } catch (err) {
      error('Mail Send Error', `Email sending failed: ${err.message}`)
      throw err
    }
  }

  // Public method to get connection pool stats
  getStats() {
    return {
      connectionPoolSize: this.connectionPool.size,
      mxCacheSize: this.mxCache.size,
      rateLimiterDomains: this.rateLimiter.size,
      config: {
        timeout: this.config.timeout,
        retryAttempts: this.config.retryAttempts,
        maxConnections: this.config.maxConnections,
        enableAuth: this.config.enableAuth,
        enableDKIM: this.config.enableDKIM
      }
    }
  }

  // Public method to clear caches
  clearCaches() {
    this.mxCache.clear()
    this.rateLimiter.clear()
    for (const [, connection] of this.connectionPool.entries()) {
      try {
        connection.socket.end()
      } catch {
        // Ignore cleanup errors
      }
    }
    this.connectionPool.clear()
    log('SMTP', 'All caches and connections cleared')
  }

  // Public method to update configuration
  updateConfig(newConfig) {
    this.config = {...this.config, ...newConfig}
    log('SMTP', 'Configuration updated')
  }
}

module.exports = new smtp()
