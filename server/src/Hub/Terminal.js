const {log, error} = Odac.core('Log', false).init('Hub', 'Terminal')

const WebSocketLib = require('ws')

// Hub mints both; they land in a URL path and a header, never in a shell command.
const ID_PATTERN = /^[A-Za-z0-9_-]{8,64}$/

const CONNECT_TIMEOUT = 10000

// Terminal output is unbounded — `cat` a large file and the pty will happily outrun a slow
// client. Once the socket has this much unflushed, the session is beyond saving: dropping
// chunks would corrupt the stream, so close it honestly instead of growing the heap.
const MAX_BUFFERED_BYTES = 8 * 1024 * 1024

const DEFAULTS = {
  enabled: true,
  maxSessions: 3,
  idleTimeout: 15 * 60 * 1000,
  maxLifetime: 4 * 60 * 60 * 1000,
  // A shell in a privileged container is effectively a host shell; opening one stays a
  // deliberate operator decision, never the default — even with terminals enabled.
  allowPrivileged: false
}

/**
 * One terminal session: a dedicated WebSocket to the Hub paired with a container exec.
 *
 * The data plane is deliberately kept off the main Hub socket. That socket carries the
 * control protocol behind a 30s ping/pong liveness check, and a WebSocket is a single TCP
 * stream — a few megabytes of terminal output would delay the pong and get the agent's whole
 * Hub connection torn down. It also spares every keystroke an HMAC over canonical JSON.
 *
 * Framing on the session socket: binary frames are raw pty bytes, text frames are JSON
 * control messages. Data therefore needs no envelope at all.
 */
class TerminalSession {
  #id
  #app
  #socket = null
  #terminal = null
  #closing = false
  #onClosed

  constructor({id, app, onClosed}) {
    this.#id = id
    this.#app = app
    this.#onClosed = onClosed
  }

  get id() {
    return this.#id
  }

  get app() {
    return this.#app
  }

  /**
   * Opens the container exec first, then dials the Hub.
   *
   * That order matters: a bad app name or a stopped container has to fail before we claim a
   * session slot on the Hub side, so the failure surfaces in the command response rather
   * than as a socket that opens and immediately dies.
   */
  async open({url, token, ticket, cols, rows, idleTimeout, maxLifetime, allowPrivileged}) {
    this.#terminal = await Odac.server('Container').createTerminalSession(this.#app, {
      cols,
      rows,
      idleTimeout,
      maxLifetime,
      allowPrivileged,
      onData: chunk => this.#send(chunk),
      onExit: info => this.#handleExit(info)
    })

    try {
      await this.#dial(url, token, ticket)
    } catch (e) {
      await this.#terminal.close('error')
      throw e
    }

    log('[Terminal] Session %s attached to %s', this.#id, this.#app)
  }

  #dial(url, token, ticket) {
    return new Promise((resolve, reject) => {
      this.#socket = new WebSocketLib(url, {
        rejectUnauthorized: true,
        headers: {Authorization: `Bearer ${token}`, 'X-Odac-Ticket': ticket}
      })

      let settled = false
      const timer = setTimeout(() => {
        settled = true
        this.#socket.terminate()
        reject(new Error('Terminal socket timed out'))
      }, CONNECT_TIMEOUT)

      this.#socket.once('open', () => {
        settled = true
        clearTimeout(timer)
        resolve()
      })

      // A persistent listener, not once(): 'ws' emits 'error' before 'close' at any point in
      // the socket's life, and an 'error' with no listener takes the process down with it.
      this.#socket.on('error', err => {
        if (settled) {
          error('[Terminal] Session %s socket error: %s', this.#id, err.message)
          return
        }
        settled = true
        clearTimeout(timer)
        reject(err)
      })

      this.#socket.on('message', (data, isBinary) => this.#handleMessage(data, isBinary))
      this.#socket.on('close', () => this.close('socket'))
    })
  }

  #handleMessage(data, isBinary) {
    // Keystrokes are the overwhelming majority; keep them on the cheapest path.
    if (isBinary) {
      this.#terminal?.write(data)
      return
    }

    let message
    try {
      message = JSON.parse(data.toString())
    } catch {
      log('[Terminal] Session %s sent malformed control frame', this.#id)
      return
    }

    switch (message.type) {
      case 'resize':
        this.#terminal?.resize(message.cols, message.rows)
        break
      case 'close':
        this.close('remote')
        break
      default:
        log('[Terminal] Session %s sent unknown control frame: %s', this.#id, message.type)
    }
  }

  #send(chunk) {
    if (this.#socket?.readyState !== WebSocketLib.OPEN) return

    if (this.#socket.bufferedAmount > MAX_BUFFERED_BYTES) {
      error('[Terminal] Session %s outran its socket, closing', this.#id)
      this.close('overflow')
      return
    }

    this.#socket.send(chunk, {binary: true})
  }

  #control(payload) {
    if (this.#socket?.readyState !== WebSocketLib.OPEN) return
    this.#socket.send(JSON.stringify(payload))
  }

  /** The shell ended on its own (or a timer reaped it): tell the Hub, then hang up. */
  #handleExit({reason, exitCode}) {
    this.#control({type: 'exit', reason, exitCode})
    this.close(reason)
  }

  /**
   * Idempotent, and reachable from both ends: the socket closing must reap the exec, and the
   * exec exiting must close the socket.
   * @param {string} [reason]
   */
  async close(reason = 'closed') {
    if (this.#closing) return
    this.#closing = true

    const terminal = this.#terminal
    const socket = this.#socket
    this.#terminal = null
    this.#socket = null

    if (socket) {
      socket.removeAllListeners('close')
      if (socket.readyState === WebSocketLib.OPEN) socket.close()
      else socket.terminate()
    }

    if (terminal && !terminal.closed) {
      try {
        await terminal.close(reason)
      } catch (e) {
        error('[Terminal] Session %s failed to close cleanly: %s', this.#id, e.message)
      }
    }

    log('[Terminal] Session %s detached from %s (%s)', this.#id, this.#app, reason)
    this.#onClosed?.(this.#id)
  }
}

/**
 * Owns the live sessions and enforces the per-agent limits.
 */
class TerminalManager {
  #wsUrl
  #getToken
  #sessions = new Map()

  /**
   * @param {Object} options
   * @param {string} options.wsUrl - Hub WebSocket base, e.g. wss://hub.odac.run/ws
   * @param {function(): string|undefined} options.getToken
   */
  constructor({wsUrl, getToken}) {
    this.#wsUrl = wsUrl
    this.#getToken = getToken
  }

  get count() {
    return this.#sessions.size
  }

  #settings() {
    const configured = Odac.core('Config').config.hub?.terminal || {}
    return {...DEFAULTS, ...configured}
  }

  /**
   * Handles a `terminal.open` command from the Hub.
   * @returns {Promise<Object>} {success, message, data?}
   */
  async open(payload = {}) {
    const settings = this.#settings()

    if (!settings.enabled) {
      return {success: false, message: 'Terminal access is disabled'}
    }

    const {app, sessionId, ticket} = payload

    // The Hub is authenticated, but a bug or a compromise there must not be able to steer
    // these into the URL. Both are opaque tokens; anything else is a red flag.
    if (!ID_PATTERN.test(sessionId || '') || !ID_PATTERN.test(ticket || '')) {
      return {success: false, message: 'Invalid session id or ticket'}
    }

    if (!app || typeof app !== 'string') {
      return {success: false, message: 'Missing app'}
    }

    if (this.#sessions.has(sessionId)) {
      return {success: false, message: 'Session already open'}
    }

    if (this.#sessions.size >= settings.maxSessions) {
      return {success: false, message: `Too many terminal sessions (max ${settings.maxSessions})`}
    }

    const token = this.#getToken()
    if (!token) return {success: false, message: 'Not authenticated'}

    const session = new TerminalSession({id: sessionId, app, onClosed: id => this.#sessions.delete(id)})

    // Reserve the slot before the awaits below, or concurrent opens both see room.
    this.#sessions.set(sessionId, session)

    try {
      await session.open({
        url: `${this.#wsUrl}/terminal/${sessionId}`,
        token,
        ticket,
        cols: payload.cols,
        rows: payload.rows,
        idleTimeout: settings.idleTimeout,
        maxLifetime: settings.maxLifetime,
        allowPrivileged: settings.allowPrivileged
      })
    } catch (e) {
      this.#sessions.delete(sessionId)
      error('[Terminal] Session %s failed to open on %s: %s', sessionId, app, e.message)
      return {success: false, message: e.message}
    }

    return {success: true, message: 'Terminal session opened', data: {sessionId, app}}
  }

  /**
   * Handles a `terminal.close` command from the Hub.
   */
  async close(payload = {}) {
    const session = this.#sessions.get(payload.sessionId)
    if (!session) return {success: false, message: 'Unknown session'}

    await session.close('command')
    return {success: true, message: 'Terminal session closed'}
  }

  /**
   * Tears every session down. The Hub connection dropping means nobody is watching, and an
   * unattended shell must not outlive it.
   */
  async closeAll() {
    if (this.#sessions.size === 0) return

    log('[Terminal] Closing %d active session(s)', this.#sessions.size)
    await Promise.all([...this.#sessions.values()].map(session => session.close('hub_disconnected')))
    this.#sessions.clear()
  }
}

module.exports = TerminalManager
module.exports.TerminalSession = TerminalSession
