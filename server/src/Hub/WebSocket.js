const {log} = Odac.core('Log', false).init('Hub', 'WebSocket')

const WebSocketLib = require('ws')
const nodeCrypto = require('crypto')

const WS_STATE = {
  CONNECTING: 0,
  OPEN: 1,
  CLOSING: 2,
  CLOSED: 3
}

/**
 * WebSocket client for Hub communication.
 * Handles connection, reconnection, heartbeat and message routing.
 */
class WebSocketClient {
  #socket = null
  #pingInterval = null
  #isAlive = false
  #nextReconnectTime = 0

  #onMessage = null
  #onConnect = null
  #onDisconnect = null

  /**
   * Check if WebSocket is currently connected and ready.
   * @returns {boolean}
   */
  get connected() {
    return this.#socket?.readyState === WS_STATE.OPEN
  }

  /**
   * Get the underlying WebSocket instance.
   * @returns {WebSocket|null}
   */
  get socket() {
    return this.#socket
  }

  /**
   * Set event handlers for WebSocket events.
   * @param {Object} handlers - Event handlers
   * @param {Function} [handlers.onMessage] - Called when a message is received
   * @param {Function} [handlers.onConnect] - Called when connection is established
   * @param {Function} [handlers.onDisconnect] - Called when connection is closed
   */
  setHandlers({onMessage, onConnect, onDisconnect}) {
    this.#onMessage = onMessage
    this.#onConnect = onConnect
    this.#onDisconnect = onDisconnect
  }

  /**
   * Connect to WebSocket server.
   * @param {string} url - WebSocket server URL
   * @param {string} token - Authentication token
   */
  connect(url, token) {
    if (this.#socket) {
      log('WebSocket already connected')
      return
    }

    try {
      log('Connecting to WebSocket: %s', url)

      this.#socket = new WebSocketLib(url, {
        rejectUnauthorized: true,
        headers: {Authorization: `Bearer ${token}`}
      })

      this.#socket.on('open', this.#handleOpen.bind(this))
      this.#socket.on('pong', this.#handlePong.bind(this))
      this.#socket.on('message', this.#handleMessage.bind(this))
      this.#socket.on('close', this.#handleClose.bind(this))
      this.#socket.on('error', this.#handleError.bind(this))
    } catch (error) {
      log('Failed to connect WebSocket: %s', error.message)
      this.#socket = null
    }
  }

  /**
   * Disconnect from WebSocket server.
   */
  disconnect() {
    if (!this.#socket) return

    log('Disconnecting WebSocket')
    this.#stopHeartbeat()
    this.#socket.close()
    this.#socket = null
  }

  /**
   * Send data through WebSocket.
   * @param {Object|string} data - Data to send (objects are JSON stringified)
   * @returns {boolean} True if sent successfully
   */
  send(data) {
    if (!this.connected) return false

    const payload = typeof data === 'string' ? data : JSON.stringify(data)
    this.#socket.send(payload)
    return true
  }

  /**
   * Check if enough time has passed to attempt reconnection.
   * @returns {boolean}
   */
  shouldReconnect() {
    return Date.now() >= this.#nextReconnectTime
  }

  #handleOpen() {
    log('WebSocket connected')
    this.#isAlive = true
    this.#startHeartbeat()
    this.#onConnect?.()
  }

  #handlePong() {
    this.#isAlive = true
  }

  #handleMessage(data) {
    this.#onMessage?.(data)
  }

  #handleClose() {
    log('WebSocket disconnected')
    this.#stopHeartbeat()
    this.#socket = null
    this.#scheduleReconnect()
    this.#onDisconnect?.()
  }

  #handleError(error) {
    log('WebSocket error: %s', error.message)
  }

  #scheduleReconnect() {
    const delay = 5000 + Math.floor(Math.random() * 15000)
    this.#nextReconnectTime = Date.now() + delay
  }

  #startHeartbeat() {
    this.#stopHeartbeat()

    this.#pingInterval = setInterval(() => {
      if (!this.#socket) return

      if (!this.#isAlive) {
        log('WebSocket connection dead (no pong), terminating...')
        this.#socket.terminate()
        return
      }

      this.#isAlive = false

      if (this.connected) {
        this.#socket.ping()
      }
    }, 30000)
  }

  #stopHeartbeat() {
    if (this.#pingInterval) {
      clearInterval(this.#pingInterval)
      this.#pingInterval = null
    }
  }
}

/**
 * Utility class for signing and verifying WebSocket messages using HMAC-SHA256.
 */
class MessageSigner {
  /**
   * Sign a message with HMAC-SHA256.
   * @param {Object} message - Message to sign
   * @param {string} message.type - Message type
   * @param {Object} message.data - Message data
   * @param {number} message.timestamp - Unix timestamp
   * @param {string} secret - HMAC secret key
   * @returns {string|null} Hex signature or null if no secret
   */
  static sign(message, secret) {
    if (!secret) return null

    const payloadObj = {}
    if (message.id) payloadObj.id = message.id

    payloadObj.type = message.type
    payloadObj.data = message.data
    payloadObj.timestamp = message.timestamp

    return nodeCrypto.createHmac('sha256', secret).update(JSON.stringify(payloadObj)).digest('hex')
  }

  /**
   * Verify a signed message.
   * @param {Object} message - Message to verify
   * @param {string} message.type - Message type
   * @param {Object} message.data - Message data
   * @param {number} message.timestamp - Unix timestamp
   * @param {string} message.signature - Expected signature
   * @param {string} secret - HMAC secret key
   * @returns {boolean} True if signature is valid
   */
  static verify(message, secret) {
    const {timestamp, signature} = message

    if (!signature || !timestamp) {
      log('Missing signature or timestamp in WebSocket message')
      return false
    }

    const now = Math.floor(Date.now() / 1000)
    const maxAge = 300

    if (Math.abs(now - timestamp) > maxAge) {
      log('WebSocket message timestamp too old or in future')
      return false
    }

    const expectedSignature = this.sign(message, secret)

    if (signature !== expectedSignature) {
      log('Invalid WebSocket message signature')
      return false
    }

    return true
  }
}

module.exports = {WebSocketClient, MessageSigner}
