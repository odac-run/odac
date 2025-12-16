const http = require('http')

class WebSocketProxy {
  #log

  constructor(log) {
    this.#log = log
  }

  #validateClientFrame(buffer) {
    if (buffer.length < 2) return true

    const secondByte = buffer[1]
    const masked = (secondByte & 0x80) !== 0

    return masked
  }

  upgrade(req, socket, head, website, host) {
    const options = {
      hostname: '127.0.0.1',
      port: website.port,
      path: req.url,
      method: 'GET',
      headers: {}
    }

    for (const [key, value] of Object.entries(req.headers)) {
      options.headers[key.toLowerCase()] = value
    }

    options.headers['x-candy-connection-remoteaddress'] = socket.remoteAddress ?? ''
    options.headers['x-candy-connection-ssl'] = 'true'
    options.headers['x-candy-websocket'] = 'true'

    const proxyReq = http.request(options)

    proxyReq.on('upgrade', (proxyRes, proxySocket, proxyHead) => {
      const responseHeaders = ['HTTP/1.1 101 Switching Protocols']

      for (const [key, value] of Object.entries(proxyRes.headers)) {
        responseHeaders.push(`${key}: ${value}`)
      }

      responseHeaders.push('', '')
      socket.write(responseHeaders.join('\r\n'))

      if (proxyHead.length > 0) {
        socket.write(proxyHead)
      }
      if (head.length > 0) {
        proxySocket.write(head)
      }

      socket.on('data', chunk => {
        if (!this.#validateClientFrame(chunk)) {
          socket.destroy()
          proxySocket.destroy()
          return
        }
        proxySocket.write(chunk)
      })

      proxySocket.on('data', chunk => {
        socket.write(chunk)
      })

      socket.on('error', () => proxySocket.destroy())
      proxySocket.on('error', () => socket.destroy())
      socket.on('close', () => proxySocket.destroy())
      proxySocket.on('close', () => socket.destroy())
    })

    proxyReq.on('error', err => {
      this.#log(`WebSocket proxy error for ${host}: ${err.message}`)
      socket.destroy()
    })

    proxyReq.on('response', res => {
      if (res.statusCode !== 101) {
        const headers = [`HTTP/1.1 ${res.statusCode} ${res.statusMessage}`]
        for (const [key, value] of Object.entries(res.headers)) {
          headers.push(`${key}: ${value}`)
        }
        headers.push('', '')
        socket.write(headers.join('\r\n'))
        res.pipe(socket)
      }
    })

    proxyReq.end()
  }
}

module.exports = WebSocketProxy
