const {MockOdac} = require('./__mocks__/globalOdac')

const mockOdac = new MockOdac()
mockOdac.setMock('core', 'Log', {init: jest.fn().mockReturnValue({log: jest.fn(), error: jest.fn()})})
global.Odac = mockOdac

jest.mock('ws', () => {
  const {EventEmitter} = require('events')

  class FakeWebSocket extends EventEmitter {
    static OPEN = 1
    static instances = []

    constructor(url, options) {
      super()
      this.url = url
      this.options = options
      this.readyState = 0
      this.bufferedAmount = 0
      this.sent = []
      this.terminated = false
      FakeWebSocket.instances.push(this)
    }

    send(data, options) {
      this.sent.push({data, binary: Boolean(options?.binary)})
    }

    close() {
      this.readyState = 3
      this.emit('close')
    }

    terminate() {
      this.terminated = true
      this.readyState = 3
    }
  }

  return FakeWebSocket
})

const FakeWebSocket = require('ws')
const TerminalManager = require('../../server/src/Hub/Terminal')

const flush = () => new Promise(resolve => setImmediate(resolve))
const lastSocket = () => FakeWebSocket.instances[FakeWebSocket.instances.length - 1]

/** The Terminal returned by Container.createTerminalSession(). */
function createTerminalStub() {
  const stub = {
    closed: false,
    write: jest.fn(),
    resize: jest.fn(),
    close: jest.fn(async reason => {
      stub.closed = true
      stub.closeReason = reason
    })
  }
  return stub
}

let terminalStub
let createTerminalSession

function configure(terminal = {enabled: true}) {
  mockOdac.setMock('core', 'Config', {config: {hub: {token: 'tok', terminal}}})
}

beforeEach(() => {
  FakeWebSocket.instances = []
  terminalStub = createTerminalStub()

  createTerminalSession = jest.fn(async (app, options) => {
    terminalStub.app = app
    terminalStub.options = options
    return terminalStub
  })

  mockOdac.setMock('server', 'Container', {createTerminalSession})
  configure()
})

const manager = () => new TerminalManager({wsUrl: 'wss://hub.odac.run/ws', getToken: () => 'tok'})

const payload = (overrides = {}) => ({app: 'web', sessionId: 'sess-abcdef12', ticket: 'ticket-abcdef12', cols: 100, rows: 30, ...overrides})

/** Drives open() to completion by letting the fake socket connect. */
async function open(mgr, options = payload()) {
  const pending = mgr.open(options)
  await flush()
  await flush()

  const socket = lastSocket()
  if (socket) {
    socket.readyState = FakeWebSocket.OPEN
    socket.emit('open')
  }

  return pending
}

describe('TerminalManager.open() guards', () => {
  test('is enabled by default (empty config still opens)', async () => {
    configure({})
    const result = await open(manager())

    expect(result.success).toBe(true)
    expect(createTerminalSession).toHaveBeenCalled()
  })

  test('an operator can switch it off with enabled:false', async () => {
    configure({enabled: false})
    const result = await manager().open(payload())

    expect(result).toEqual({success: false, message: 'Terminal access is disabled'})
    expect(createTerminalSession).not.toHaveBeenCalled()
  })

  test.each([
    ['session id', {sessionId: 'short'}],
    ['session id with a path traversal', {sessionId: '../../etc'}],
    ['ticket', {ticket: 'nope'}]
  ])('rejects a malformed %s before dialing', async (_label, override) => {
    const result = await manager().open(payload(override))

    expect(result.success).toBe(false)
    expect(result.message).toBe('Invalid session id or ticket')
    expect(FakeWebSocket.instances).toHaveLength(0)
    expect(createTerminalSession).not.toHaveBeenCalled()
  })

  test('enforces the concurrent session cap', async () => {
    configure({enabled: true, maxSessions: 1})
    const mgr = manager()

    await open(mgr)
    const second = await mgr.open(payload({sessionId: 'sess-99999999'}))

    expect(second).toEqual({success: false, message: 'Too many terminal sessions (max 1)'})
    expect(mgr.count).toBe(1)
  })

  test('refuses a duplicate session id', async () => {
    const mgr = manager()
    await open(mgr)

    const again = await mgr.open(payload())
    expect(again).toEqual({success: false, message: 'Session already open'})
  })

  test('refuses when the agent has no Hub token', async () => {
    const mgr = new TerminalManager({wsUrl: 'wss://hub.odac.run/ws', getToken: () => undefined})
    const result = await mgr.open(payload())

    expect(result).toEqual({success: false, message: 'Not authenticated'})
  })

  test('surfaces a rejected app and frees the slot', async () => {
    createTerminalSession.mockRejectedValue(new Error('Unknown app: odac'))
    const mgr = manager()

    const result = await mgr.open(payload({app: 'odac'}))

    expect(result).toEqual({success: false, message: 'Unknown app: odac'})
    expect(mgr.count).toBe(0)
    // The exec is validated before the Hub is dialed, so no socket was ever opened.
    expect(FakeWebSocket.instances).toHaveLength(0)
  })

  test('reaps the exec when the socket fails to connect', async () => {
    const mgr = manager()
    const pending = mgr.open(payload())
    await flush()
    await flush()

    lastSocket().emit('error', new Error('handshake failed'))
    const result = await pending

    expect(result.success).toBe(false)
    expect(terminalStub.close).toHaveBeenCalledWith('error')
    expect(mgr.count).toBe(0)
  })
})

describe('TerminalManager.open() success', () => {
  test('dials the per-session endpoint with token and ticket', async () => {
    const mgr = manager()
    const result = await open(mgr)

    expect(result).toEqual({success: true, message: 'Terminal session opened', data: {sessionId: 'sess-abcdef12', app: 'web'}})
    expect(mgr.count).toBe(1)

    const socket = lastSocket()
    expect(socket.url).toBe('wss://hub.odac.run/ws/terminal/sess-abcdef12')
    expect(socket.options.headers).toEqual({Authorization: 'Bearer tok', 'X-Odac-Ticket': 'ticket-abcdef12'})
    expect(socket.options.rejectUnauthorized).toBe(true)
  })

  test('passes the requested size and the configured limits to the exec', async () => {
    configure({enabled: true, idleTimeout: 1000, maxLifetime: 2000})
    await open(manager())

    expect(createTerminalSession).toHaveBeenCalledWith(
      'web',
      expect.objectContaining({cols: 100, rows: 30, idleTimeout: 1000, maxLifetime: 2000})
    )
  })

  test('does not permit privileged containers unless configured', async () => {
    await open(manager())
    expect(createTerminalSession).toHaveBeenCalledWith('web', expect.objectContaining({allowPrivileged: false}))
  })

  test('threads the privileged opt-in through to the exec', async () => {
    configure({enabled: true, allowPrivileged: true})
    await open(manager())
    expect(createTerminalSession).toHaveBeenCalledWith('web', expect.objectContaining({allowPrivileged: true}))
  })
})

describe('TerminalSession framing', () => {
  test('pty output goes out as binary frames', async () => {
    await open(manager())

    terminalStub.options.onData(Buffer.from('hello'))

    expect(lastSocket().sent).toEqual([{data: Buffer.from('hello'), binary: true}])
  })

  test('binary frames are written straight to the pty', async () => {
    await open(manager())

    lastSocket().emit('message', Buffer.from('ls\n'), true)

    expect(terminalStub.write).toHaveBeenCalledWith(Buffer.from('ls\n'))
  })

  test('a text resize frame resizes the pty', async () => {
    await open(manager())

    lastSocket().emit('message', Buffer.from(JSON.stringify({type: 'resize', cols: 120, rows: 40})), false)

    expect(terminalStub.resize).toHaveBeenCalledWith(120, 40)
  })

  test('a text close frame ends the session', async () => {
    const mgr = manager()
    await open(mgr)

    lastSocket().emit('message', Buffer.from(JSON.stringify({type: 'close'})), false)
    await flush()

    expect(terminalStub.close).toHaveBeenCalledWith('remote')
    expect(mgr.count).toBe(0)
  })

  test('a malformed control frame is ignored, not fatal', async () => {
    const mgr = manager()
    await open(mgr)

    lastSocket().emit('message', Buffer.from('{not json'), false)
    await flush()

    expect(mgr.count).toBe(1)
    expect(terminalStub.write).not.toHaveBeenCalled()
  })

  test('a socket error after connect does not crash the session', async () => {
    const mgr = manager()
    await open(mgr)

    // 'ws' emits error before close; an unhandled 'error' would take the process down.
    expect(() => lastSocket().emit('error', new Error('reset by peer'))).not.toThrow()
  })
})

describe('TerminalSession teardown', () => {
  test('the shell exiting tells the Hub, then hangs up', async () => {
    const mgr = manager()
    await open(mgr)
    const socket = lastSocket()

    terminalStub.closed = true
    terminalStub.options.onExit({reason: 'exited', exitCode: 7})
    await flush()

    expect(socket.sent).toEqual([{data: JSON.stringify({type: 'exit', reason: 'exited', exitCode: 7}), binary: false}])
    expect(mgr.count).toBe(0)
  })

  test('the socket dropping reaps the exec', async () => {
    const mgr = manager()
    await open(mgr)

    lastSocket().close()
    await flush()

    expect(terminalStub.close).toHaveBeenCalledWith('socket')
    expect(mgr.count).toBe(0)
  })

  test('a session that outruns its socket is closed rather than buffered', async () => {
    const mgr = manager()
    await open(mgr)
    const socket = lastSocket()

    socket.bufferedAmount = 9 * 1024 * 1024
    terminalStub.options.onData(Buffer.from('flood'))
    await flush()

    expect(socket.sent).toHaveLength(0)
    expect(terminalStub.close).toHaveBeenCalledWith('overflow')
    expect(mgr.count).toBe(0)
  })

  test('close() by command ends the session', async () => {
    const mgr = manager()
    await open(mgr)

    await expect(mgr.close({sessionId: 'sess-abcdef12'})).resolves.toEqual({success: true, message: 'Terminal session closed'})
    expect(terminalStub.close).toHaveBeenCalledWith('command')
    expect(mgr.count).toBe(0)
  })

  test('close() on an unknown session is reported, not thrown', async () => {
    await expect(manager().close({sessionId: 'sess-00000000'})).resolves.toEqual({success: false, message: 'Unknown session'})
  })

  test('closeAll() reaps every session when the Hub connection drops', async () => {
    configure({enabled: true, maxSessions: 5})
    const mgr = manager()

    const first = terminalStub
    await open(mgr)

    terminalStub = createTerminalStub()
    await open(mgr, payload({sessionId: 'sess-22222222'}))

    expect(mgr.count).toBe(2)
    await mgr.closeAll()

    expect(first.close).toHaveBeenCalledWith('hub_disconnected')
    expect(terminalStub.close).toHaveBeenCalledWith('hub_disconnected')
    expect(mgr.count).toBe(0)
  })

  test('closing twice fires the exec teardown once', async () => {
    const mgr = manager()
    await open(mgr)

    await mgr.close({sessionId: 'sess-abcdef12'})
    lastSocket().emit('close')
    await flush()

    expect(terminalStub.close).toHaveBeenCalledTimes(1)
  })
})
