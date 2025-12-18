const http = require('http')

const FORBIDDEN_HEADERS = [
  'connection',
  'keep-alive',
  'transfer-encoding',
  'upgrade',
  'proxy-connection',
  'proxy-authenticate',
  'trailer',
  'x-odac-early-hints'
]

class WebProxy {
  #log

  constructor(log) {
    this.#log = log
  }

  #handleEarlyHints(proxyRes, res) {
    if (proxyRes.headers['x-odac-early-hints'] && typeof res.writeEarlyHints === 'function') {
      try {
        const links = JSON.parse(proxyRes.headers['x-odac-early-hints'])
        if (Array.isArray(links) && links.length > 0) {
          res.writeEarlyHints({link: links})
        }
      } catch {
        // Ignore parsing errors
      }
    }
  }

  #proxy(req, res, website, host, isHttp2) {
    const options = {
      hostname: '127.0.0.1',
      port: website.port,
      path: req.url,
      method: req.method,
      headers: {},
      timeout: 0
    }

    for (const [key, value] of Object.entries(req.headers)) {
      if (isHttp2 && key.startsWith(':')) continue
      options.headers[key.toLowerCase()] = value
    }

    options.headers['x-odac-connection-remoteaddress'] = req.socket.remoteAddress ?? ''
    options.headers['x-odac-connection-ssl'] = 'true'

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
      for (const [key, value] of Object.entries(proxyRes.headers)) {
        if (!FORBIDDEN_HEADERS.includes(key.toLowerCase())) {
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
        res.writeHead(502)
        res.end('Bad Gateway')
      }
    })

    req.pipe(proxyReq)
  }

  http2(req, res, website, host) {
    this.#proxy(req, res, website, host, true)
  }

  http1(req, res, website, host) {
    this.#proxy(req, res, website, host, false)
  }
}

module.exports = WebProxy
