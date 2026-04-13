const http = require('http')
const https = require('https')

/**
 * Enterprise-grade HTTP client focusing on performance and zero-dependency.
 * Optimized for high-throughput, low-latency communication.
 * Supports standard TCP (HTTP/HTTPS) and Unix Domain Sockets.
 */
class Http {
  /**
   * Performs an HTTP request.
   *
   * @param {string} url - Target URL
   * @param {Object} options - Request options (method, data, headers, socketPath, validateStatus, rejectUnauthorized)
   * @returns {Promise<Object>} Response object {status, statusText, data, headers}
   */
  async request(url, options = {}) {
    const {
      method = 'GET',
      data = null,
      family,
      headers = {},
      lookup,
      servername,
      socketPath,
      validateStatus,
      timeout = 30000,
      rejectUnauthorized = true // Default to true for enterprise security
    } = options

    return new Promise((resolve, reject) => {
      const parsedUrl = new URL(url)
      const isHttps = parsedUrl.protocol === 'https:'
      const transport = isHttps ? https : http
      const body = data ? (typeof data === 'string' ? data : JSON.stringify(data)) : ''

      const requestOptions = {
        method: method.toUpperCase(),
        headers: {
          'Content-Type': 'application/json',
          ...headers
        },
        timeout,
        rejectUnauthorized
      }

      // Forward Node.js socket options when explicitly provided
      if (family !== undefined) requestOptions.family = family
      if (lookup !== undefined) requestOptions.lookup = lookup
      if (servername !== undefined) requestOptions.servername = servername

      if (socketPath) {
        requestOptions.socketPath = socketPath
        requestOptions.path = parsedUrl.pathname + parsedUrl.search
      } else {
        requestOptions.hostname = parsedUrl.hostname
        if (parsedUrl.port) requestOptions.port = parsedUrl.port
        requestOptions.path = parsedUrl.pathname + parsedUrl.search
      }

      if (body) {
        requestOptions.headers['Content-Length'] = Buffer.byteLength(body)
      }

      const req = transport.request(requestOptions, res => {
        const chunks = []
        res.on('data', chunk => chunks.push(chunk))
        res.on('end', () => {
          const rawData = Buffer.concat(chunks).toString()
          let parsedData
          try {
            const contentType = res.headers['content-type'] || ''
            if (contentType.includes('application/json')) {
              parsedData = rawData ? JSON.parse(rawData) : null
            } else {
              parsedData = rawData
            }
          } catch {
            parsedData = rawData
          }

          const response = {
            status: res.statusCode,
            statusText: res.statusMessage,
            data: parsedData,
            headers: res.headers
          }

          if (validateStatus && !validateStatus(res.statusCode)) {
            const error = new Error(`HTTP Error ${res.statusCode}: ${res.statusMessage}`)
            error.response = response
            reject(error)
          } else if (!validateStatus && (res.statusCode < 200 || res.statusCode >= 300)) {
            const error = new Error(`HTTP Error ${res.statusCode}: ${res.statusMessage}`)
            error.response = response
            reject(error)
          } else {
            resolve(response)
          }
        })
      })

      req.on('error', e => {
        // Enforce enterprise-grade error mapping
        if (e.code === 'ECONNREFUSED') {
          e.message = `Connection refused at ${url}`
        }
        reject(e)
      })

      req.on('timeout', () => {
        req.destroy()
        const error = new Error(`Request timed out after ${timeout}ms`)
        error.code = 'ETIMEDOUT'
        reject(error)
      })

      if (body) req.write(body)
      req.end()
    })
  }

  /**
   * Convenience GET method.
   */
  async get(url, options = {}) {
    return this.request(url, {...options, method: 'GET'})
  }

  /**
   * Convenience POST method.
   */
  async post(url, data, options = {}) {
    return this.request(url, {...options, method: 'POST', data})
  }

  /**
   * Convenience PUT method.
   */
  async put(url, data, options = {}) {
    return this.request(url, {...options, method: 'PUT', data})
  }

  /**
   * Convenience DELETE method.
   */
  async delete(url, options = {}) {
    return this.request(url, {...options, method: 'DELETE'})
  }
}

module.exports = new Http()
