const nodeCrypto = require('crypto')

const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11'
const MAX_PAYLOAD_LENGTH = 10 * 1024 * 1024
const OPCODE = {
  CONTINUATION: 0x0,
  TEXT: 0x1,
  BINARY: 0x2,
  CLOSE: 0x8,
  PING: 0x9,
  PONG: 0xa
}

class WebSocketClient {
  #socket
  #handlers = {}
  #closed = false
  #server
  #id
  #rooms = new Set()
  data = {}

  constructor(socket, server, id) {
    this.#socket = socket
    this.#server = server
    this.#id = id
    this.#setupListeners()
  }

  get id() {
    return this.#id
  }

  get rooms() {
    return Array.from(this.#rooms)
  }

  #setupListeners() {
    let buffer = Buffer.alloc(0)

    this.#socket.on('data', chunk => {
      buffer = Buffer.concat([buffer, chunk])

      while (buffer.length >= 2) {
        const frame = this.#parseFrame(buffer)
        if (!frame) break

        buffer = buffer.slice(frame.totalLength)
        this.#handleFrame(frame)
      }
    })

    this.#socket.on('close', () => this.#handleClose())
    this.#socket.on('error', err => this.#emit('error', err))
  }

  #parseFrame(buffer) {
    if (buffer.length < 2) return null

    const firstByte = buffer[0]
    const secondByte = buffer[1]

    const fin = (firstByte & 0x80) !== 0
    const opcode = firstByte & 0x0f
    const masked = (secondByte & 0x80) !== 0

    if (!masked) {
      this.close(1002, 'Protocol error: client-to-server frames must be masked.')
      return null
    }

    let payloadLength = secondByte & 0x7f

    let offset = 2

    if (payloadLength === 126) {
      if (buffer.length < 4) return null
      payloadLength = buffer.readUInt16BE(2)
      offset = 4
    } else if (payloadLength === 127) {
      if (buffer.length < 10) return null
      const payloadLengthBigInt = buffer.readBigUInt64BE(2)
      if (payloadLengthBigInt > Number.MAX_SAFE_INTEGER) {
        this.close(1009, 'Payload too large')
        return null
      }
      payloadLength = Number(payloadLengthBigInt)
      offset = 10
    }

    if (payloadLength > MAX_PAYLOAD_LENGTH) {
      this.close(1009, 'Payload too large')
      return null
    }

    let maskKey = null
    if (masked) {
      if (buffer.length < offset + 4) return null
      maskKey = buffer.slice(offset, offset + 4)
      offset += 4
    }

    if (buffer.length < offset + payloadLength) return null

    let payload = buffer.slice(offset, offset + payloadLength)

    if (masked && maskKey) {
      payload = Buffer.from(payload)
      for (let i = 0; i < payload.length; i++) {
        payload[i] ^= maskKey[i % 4]
      }
    }

    return {
      fin,
      opcode,
      payload,
      totalLength: offset + payloadLength
    }
  }

  #handleFrame(frame) {
    switch (frame.opcode) {
      case OPCODE.TEXT:
        this.#handleMessage(frame.payload.toString('utf8'))
        break
      case OPCODE.BINARY:
        this.#handleMessage(frame.payload)
        break
      case OPCODE.PING:
        this.#sendFrame(OPCODE.PONG, frame.payload)
        break
      case OPCODE.PONG:
        this.#emit('pong')
        break
      case OPCODE.CLOSE:
        this.close()
        break
    }
  }

  #handleMessage(data) {
    try {
      const parsed = JSON.parse(data)
      this.#emit('message', parsed)
    } catch {
      this.#emit('message', data)
    }
  }

  #handleClose() {
    if (this.#closed) return
    this.#closed = true

    for (const room of this.#rooms) {
      this.#server.leaveRoom(this.#id, room)
    }

    this.#emit('close')
    this.#server.removeClient(this.#id)
  }

  #sendFrame(opcode, data) {
    if (this.#closed) return

    const payload = Buffer.isBuffer(data) ? data : Buffer.from(data)
    const length = payload.length

    let header
    if (length < 126) {
      header = Buffer.alloc(2)
      header[0] = 0x80 | opcode
      header[1] = length
    } else if (length < 65536) {
      header = Buffer.alloc(4)
      header[0] = 0x80 | opcode
      header[1] = 126
      header.writeUInt16BE(length, 2)
    } else {
      header = Buffer.alloc(10)
      header[0] = 0x80 | opcode
      header[1] = 127
      header.writeBigUInt64BE(BigInt(length), 2)
    }

    this.#socket.write(Buffer.concat([header, payload]))
  }

  #emit(event, ...args) {
    if (this.#handlers[event]) {
      for (const handler of this.#handlers[event]) {
        handler(...args)
      }
    }
  }

  on(event, handler) {
    if (!this.#handlers[event]) this.#handlers[event] = []
    this.#handlers[event].push(handler)
    return this
  }

  off(event, handler) {
    if (!this.#handlers[event]) return this
    if (handler) {
      this.#handlers[event] = this.#handlers[event].filter(h => h !== handler)
    } else {
      delete this.#handlers[event]
    }
    return this
  }

  send(data) {
    if (this.#closed) return this
    const payload = typeof data === 'object' ? JSON.stringify(data) : String(data)
    this.#sendFrame(OPCODE.TEXT, payload)
    return this
  }

  sendBinary(data) {
    if (this.#closed) return this
    this.#sendFrame(OPCODE.BINARY, data)
    return this
  }

  ping() {
    this.#sendFrame(OPCODE.PING, Buffer.alloc(0))
    return this
  }

  close(code = 1000, reason = '') {
    if (this.#closed) return

    const reasonBuffer = Buffer.from(reason)
    const payload = Buffer.alloc(2 + reasonBuffer.length)
    payload.writeUInt16BE(code, 0)
    reasonBuffer.copy(payload, 2)

    this.#sendFrame(OPCODE.CLOSE, payload)
    this.#socket.end()
    this.#closed = true
  }

  join(room) {
    this.#rooms.add(room)
    this.#server.joinRoom(this.#id, room)
    return this
  }

  leave(room) {
    this.#rooms.delete(room)
    this.#server.leaveRoom(this.#id, room)
    return this
  }

  to(room) {
    return {
      send: data => this.#server.toRoom(room, data),
      sendBinary: data => this.#server.toRoomBinary(room, data)
    }
  }

  broadcast(data, includesSelf = false) {
    this.#server.broadcast(data, includesSelf ? null : this.#id)
    return this
  }
}

class WebSocketServer {
  #clients = new Map()
  #rooms = new Map()
  #routes = new Map()

  route(path, handler) {
    this.#routes.set(path, handler)
  }

  getRoute(path) {
    if (this.#routes.has(path)) return this.#routes.get(path)

    for (const [pattern, handler] of this.#routes) {
      if (!pattern.includes('{')) continue
      const regex = new RegExp('^' + pattern.replace(/\{[^}]+\}/g, '([^/]+)') + '$')
      const match = path.match(regex)
      if (match) {
        const params = {}
        const paramNames = pattern.match(/\{([^}]+)\}/g) || []
        paramNames.forEach((name, i) => {
          params[name.slice(1, -1)] = match[i + 1]
        })
        return {handler, params}
      }
    }
    return null
  }

  handleUpgrade(req, socket, head, Candy) {
    const path = req.url.split('?')[0]
    const routeInfo = this.getRoute(path)

    if (!routeInfo) {
      socket.write('HTTP/1.1 404 Not Found\r\n\r\n')
      socket.destroy()
      return
    }

    const handler = typeof routeInfo === 'function' ? routeInfo : routeInfo.handler
    const params = routeInfo.params || {}

    const key = req.headers['sec-websocket-key']
    if (!key) {
      socket.write('HTTP/1.1 400 Bad Request\r\n\r\n')
      socket.destroy()
      return
    }

    const acceptKey = nodeCrypto
      .createHash('sha1')
      .update(key + WS_GUID)
      .digest('base64')

    const responseHeaders = [
      'HTTP/1.1 101 Switching Protocols',
      'Upgrade: websocket',
      'Connection: Upgrade',
      `Sec-WebSocket-Accept: ${acceptKey}`,
      '',
      ''
    ]

    socket.write(responseHeaders.join('\r\n'))

    const clientId = nodeCrypto.randomUUID()
    const client = new WebSocketClient(socket, this, clientId)
    this.#clients.set(clientId, client)

    if (params) {
      for (const [k, v] of Object.entries(params)) {
        Candy.Request.data.url[k] = v
      }
    }

    if (Candy.Request && req.headers) {
      Candy.Request._wsHeaders = req.headers
    }

    handler(client, Candy)
  }

  removeClient(id) {
    this.#clients.delete(id)
  }

  joinRoom(clientId, room) {
    if (!this.#rooms.has(room)) this.#rooms.set(room, new Set())
    this.#rooms.get(room).add(clientId)
  }

  leaveRoom(clientId, room) {
    if (!this.#rooms.has(room)) return
    this.#rooms.get(room).delete(clientId)
    if (this.#rooms.get(room).size === 0) this.#rooms.delete(room)
  }

  toRoom(room, data) {
    if (!this.#rooms.has(room)) return
    for (const clientId of this.#rooms.get(room)) {
      const client = this.#clients.get(clientId)
      if (client) client.send(data)
    }
  }

  toRoomBinary(room, data) {
    if (!this.#rooms.has(room)) return
    for (const clientId of this.#rooms.get(room)) {
      const client = this.#clients.get(clientId)
      if (client) client.sendBinary(data)
    }
  }

  broadcast(data, excludeId = null) {
    for (const [id, client] of this.#clients) {
      if (id !== excludeId) client.send(data)
    }
  }

  get clients() {
    return this.#clients
  }

  get clientCount() {
    return this.#clients.size
  }
}

module.exports = {WebSocketServer, WebSocketClient}
