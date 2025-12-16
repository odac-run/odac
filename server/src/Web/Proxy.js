const http = require('http')

class WebProxy {
  #log

  constructor(log) {
    this.#log = log
  }

  #handleEarlyHints(proxyRes, res) {
    if (proxyRes.headers['x-candy-early-hints'] && typeof res.writeEarlyHints === 'function') {
      try {
        const links = JSON.parse(proxyRes.headers['x-candy-early-hints'])
        if (Array.isArray(links) && links.length > 0) {
          res.writeEarlyHints({link: links})
        }
      } catch {
        // Ignore parsing errors
      }
    }
  }

  http2(req, res, website, host) {
    const options = {
      hostname: '127.0.0.1',
      port: website.port,
      path: req.url,
      method: req.method,
      headers: {},
      timeout: 0
    }

    for (const [key, value] of Object.entries(req.headers)) {
      if (!key.startsWith(':')) {
        options.headers[key.toLowerCase()] = value
      }
    }

    options.headers['x-candy-connection-remoteaddress'] = req.socket.remoteAddress ?? ''
    options.headers['x-candy-connection-ssl'] = 'true'

    const proxyReq = http.request(options, proxyRes => {
      this.#handleEarlyHints(proxyRes, res)

      const isSSE = proxyRes.headers['content-type']?.includes('text/event-stream')
      if (isSSE) {
        req.setTimeout(0)
        res.setTimeout(0)

        const abortConnection = () => {
          proxyReq.destroy()
          proxyRes.destroy()
        }
        req.on('close', abortConnection)
        req.on('aborted', abortConnection)
        res.on('close', abortConnection)
      }

      const responseHeaders = {}
      const forbiddenHeaders = [
        'connection',
        'keep-alive',
        'transfer-encoding',
        'upgrade',
        'proxy-connection',
        'proxy-authenticate',
        'trailer',
        'x-candy-early-hints'
      ]

      for (const [key, value] of Object.entries(proxyRes.headers)) {
        if (!forbiddenHeaders.includes(key.toLowerCase())) {
          responseHeaders[key] = value
        }
      }

      res.writeHead(proxyRes.statusCode, responseHeaders)
      proxyRes.pipe(res)
    })

    proxyReq.on('error', err => {
      if (err.code === 'ECONNRESET') return
      this.#log(`HTTP/2 proxy error for ${host}: ${err.message}`)
      if (!res.headersSent) {
        res.writeHead(502)
        res.end('Bad Gateway')
      }
    })

    req.pipe(proxyReq)
  }

  http1(req, res, website, host) {
    const options = {
      hostname: '127.0.0.1',
      port: website.port,
      path: req.url,
      method: req.method,
      headers: {},
      timeout: 0
    }

    for (const [key, value] of Object.entries(req.headers)) {
      options.headers[key.toLowerCase()] = value
    }

    options.headers['x-candy-connection-remoteaddress'] = req.socket.remoteAddress ?? ''
    options.headers['x-candy-connection-ssl'] = 'true'

    const proxyReq = http.request(options, proxyRes => {
      this.#handleEarlyHints(proxyRes, res)

      const isSSE = proxyRes.headers['content-type']?.includes('text/event-stream')
      if (isSSE) {
        req.setTimeout(0)
        res.setTimeout(0)

        const abortConnection = () => {
          proxyReq.destroy()
          proxyRes.destroy()
        }
        req.on('close', abortConnection)
        req.on('aborted', abortConnection)
        res.on('close', abortConnection)
      }

      const responseHeaders = {}
      const forbiddenHeaders = [
        'connection',
        'keep-alive',
        'transfer-encoding',
        'upgrade',
        'proxy-connection',
        'proxy-authenticate',
        'trailer',
        'x-candy-early-hints'
      ]

      for (const [key, value] of Object.entries(proxyRes.headers)) {
        if (!forbiddenHeaders.includes(key.toLowerCase())) {
          responseHeaders[key] = value
        }
      }

      res.writeHead(proxyRes.statusCode, responseHeaders)
      proxyRes.pipe(res)
    })

    proxyReq.on('error', err => {
      if (err.code === 'ECONNRESET') return
      this.#log(`Proxy error for ${host}: ${err.message}`)
      if (!res.headersSent) {
        res.statusCode = 502
        res.end('Bad Gateway')
      }
    })

    req.pipe(proxyReq)
  }
}

module.exports = WebProxy
