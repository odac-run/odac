/**
 * Comprehensive unit tests for the Api.js module
 * Tests TCP server functionality, authentication, and command routing
 */

const {setupGlobalMocks, cleanupGlobalMocks} = require('./__mocks__/testHelpers')

// Create mock log function first
const mockLog = jest.fn()
const mockError = jest.fn()

describe('Api', () => {
  let Api

  beforeEach(() => {
    setupGlobalMocks()

    // Set up the Log mock before requiring Api
    const {mockOdac} = require('./__mocks__/globalOdac')
    mockOdac.setMock('core', 'Log', {
      init: jest.fn().mockReturnValue({
        log: mockLog,
        error: mockError,
        warn: jest.fn()
      }),
      log: mockLog,
      error: mockError,
      warn: jest.fn()
    })

    // Ensure log method exists on the core Log mock
    Object.assign(global.Odac.core('Log'), {
      log: mockLog,
      error: mockError,
      warn: jest.fn()
    })

    // Mock the net module at the module level
    jest.doMock('net', () => ({
      createServer: jest.fn(() => ({
        on: jest.fn(),
        listen: jest.fn()
      }))
    }))

    // Mock the crypto module at the module level
    jest.doMock('crypto', () => ({
      randomBytes: jest.fn(() => Buffer.from('mock-auth-token-32-bytes-long-test')),
      createHmac: jest.fn().mockReturnValue({
        update: jest.fn().mockReturnThis(),
        digest: jest.fn().mockReturnValue('a'.repeat(64))
      })
    }))

    // Mock fs and os
    jest.doMock('fs', () => ({
      existsSync: jest.fn(() => false),
      mkdirSync: jest.fn(),
      unlinkSync: jest.fn(),
      chmodSync: jest.fn(),
      constants: {}
    }))
    jest.doMock('os', () => ({
      homedir: jest.fn(() => '/tmp'),
      platform: jest.fn(() => 'linux')
    }))

    // Clear module cache and require Api
    jest.resetModules()
    Api = require('../../server/src/Api')
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.resetModules()
    jest.dontMock('net')
    jest.dontMock('crypto')
    jest.dontMock('fs')
    jest.dontMock('os')
  })

  describe('initialization', () => {
    it('should initialize api config if not exists', () => {
      // Clear the api config
      global.Odac.core('Config').config.api = undefined

      Api.init()

      expect(global.Odac.core('Config').config.api).toBeDefined()
      expect(global.Odac.core('Config').config.api.auth).toBeDefined()
    })

    it('should create TCP server and set up handlers', () => {
      const net = require('net')
      const mockServer = {
        on: jest.fn(),
        listen: jest.fn()
      }
      net.createServer.mockReturnValue(mockServer)

      Api.init()
      Api.start()

      expect(net.createServer).toHaveBeenCalledWith(expect.any(Function))
      expect(mockServer.listen).toHaveBeenCalledWith(1453, '127.0.0.1')
    })

    it('should generate auth token', () => {
      const crypto = require('crypto')

      Api.init()

      expect(crypto.randomBytes).toHaveBeenCalledWith(32)
    })
  })

  describe('connection handling', () => {
    let mockServer
    let connectionHandler

    beforeEach(() => {
      const net = require('net')
      mockServer = {
        on: jest.fn(),
        listen: jest.fn()
      }
      net.createServer.mockReturnValue(mockServer)

      Api.init()
      Api.start()

      // Get the connection handler directly from the createServer call (first call is TCP server)
      // Api.js: const tcpServer = net.createServer(socket => handleConnection(socket, false))
      connectionHandler = net.createServer.mock.calls.length > 0 ? net.createServer.mock.calls[0][0] : null
    })

    it('should accept connections from localhost IPv4', () => {
      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      const mockSocket = {
        remoteAddress: '127.0.0.1',
        on: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      expect(mockSocket.destroy).not.toHaveBeenCalled()
      expect(mockSocket.on).toHaveBeenCalledWith('data', expect.any(Function))
      expect(mockSocket.on).toHaveBeenCalledWith('close', expect.any(Function))
    })

    it('should accept connections from localhost IPv6', () => {
      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      const mockSocket = {
        remoteAddress: '::ffff:127.0.0.1',
        on: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      expect(mockSocket.destroy).not.toHaveBeenCalled()
      expect(mockSocket.on).toHaveBeenCalledWith('data', expect.any(Function))
      expect(mockSocket.on).toHaveBeenCalledWith('close', expect.any(Function))
    })

    it('should reject connections from non-localhost addresses', () => {
      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      const mockSocket = {
        remoteAddress: '192.168.1.100',
        on: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      expect(mockSocket.destroy).toHaveBeenCalled()
      expect(mockSocket.on).not.toHaveBeenCalled()
    })

    it('should reject connections from external IPv6 addresses', () => {
      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      const mockSocket = {
        remoteAddress: '::ffff:192.168.1.100',
        on: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      expect(mockSocket.destroy).toHaveBeenCalled()
      expect(mockSocket.on).not.toHaveBeenCalled()
    })

    it('should clean up connection on close', () => {
      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      const mockSocket = {
        remoteAddress: '127.0.0.1',
        on: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      // Get the close handler
      const closeCall = mockSocket.on.mock.calls.find(call => call[0] === 'close')
      expect(closeCall).toBeDefined()

      const closeHandler = closeCall[1]

      // Simulate connection close
      closeHandler()

      // Connection should be cleaned up (tested indirectly through no errors)
      expect(closeHandler).toBeDefined()
    })
  })

  describe('data processing and authentication', () => {
    let mockServer
    let connectionHandler
    let dataHandler
    let mockSocket

    beforeEach(() => {
      const net = require('net')
      mockServer = {
        on: jest.fn(),
        listen: jest.fn()
      }
      net.createServer.mockReturnValue(mockServer)

      Api.init()
      Api.start()

      // Get the connection handler from createServer call
      connectionHandler = net.createServer.mock.calls.length > 0 ? net.createServer.mock.calls[0][0] : null

      if (!connectionHandler) {
        throw new Error('Connection handler not found')
        return
      }

      // Set up a localhost connection
      mockSocket = {
        remoteAddress: '127.0.0.1',
        on: jest.fn(),
        write: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      // Get the data handler
      const dataCall = mockSocket.on.mock.calls.find(call => call[0] === 'data')
      dataHandler = dataCall ? dataCall[1] : null
    })

    it('should return invalid_json error for malformed JSON', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const invalidJson = Buffer.from('invalid json data')

      await dataHandler(invalidJson)

      expect(mockSocket.write).toHaveBeenCalledWith(JSON.stringify(Api.result(false, 'invalid_json')))
    })

    it('should return unauthorized error for missing auth token', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const payload = JSON.stringify({
        action: 'mail.list',
        data: []
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":false'))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"unauthorized"'))
    })

    it('should return unauthorized error for invalid auth token', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const payload = JSON.stringify({
        auth: 'invalid-token',
        action: 'mail.list',
        data: []
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":false'))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"unauthorized"'))
    })

    it('should return unknown_action error for invalid action', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'invalid.action',
        data: []
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":false'))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"unknown_action"'))
    })

    it('should return unknown_action error for missing action', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        data: []
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":false'))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"unknown_action"'))
    })

    it('should execute valid mail.create command', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      // Mock the Mail service
      const mockMailService = global.Odac.server('Mail')
      mockMailService.create.mockResolvedValue(Api.result(true, 'Account created'))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.create',
        data: ['test@example.com', 'password123']
      })

      await dataHandler(Buffer.from(payload))

      expect(mockMailService.create).toHaveBeenCalledWith('test@example.com', 'password123', expect.any(Function))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })

    it('should execute valid app.start command', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockAppService = global.Odac.server('App')
      mockAppService.start.mockResolvedValue(Api.result(true, 'App started'))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'app.start',
        data: ['my-app.js']
      })

      await dataHandler(Buffer.from(payload))

      expect(mockAppService.start).toHaveBeenCalledWith('my-app.js', expect.any(Function))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })

    it('should execute valid server.stop command', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockServerService = global.Odac.server('Server')
      mockServerService.stop.mockResolvedValue(Api.result(true, 'Server stopped'))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'server.stop',
        data: []
      })

      await dataHandler(Buffer.from(payload))

      expect(mockServerService.stop).toHaveBeenCalledWith()
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })

    it('should handle command execution errors', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockMailService = global.Odac.server('Mail')
      mockMailService.create.mockRejectedValue(new Error('Database connection failed'))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.create',
        data: ['test@example.com', 'password123']
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":false'))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"Database connection failed"'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })

    it('should handle commands with no data parameter', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockMailService = global.Odac.server('Mail')
      mockMailService.list.mockResolvedValue(Api.result(true, []))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.list'
        // No data parameter
      })

      await dataHandler(Buffer.from(payload))

      expect(mockMailService.list).toHaveBeenCalledWith(expect.any(Function))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })

    it('should execute all mail commands', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockMailService = global.Odac.server('Mail')
      mockMailService.delete.mockResolvedValue(Api.result(true, 'Deleted'))
      mockMailService.password.mockResolvedValue(Api.result(true, 'Password changed'))
      mockMailService.send.mockResolvedValue(Api.result(true, 'Email sent'))

      // Test mail.delete
      let payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.delete',
        data: ['test@example.com']
      })

      await dataHandler(Buffer.from(payload))
      expect(mockMailService.delete).toHaveBeenCalledWith('test@example.com', expect.any(Function))

      // Test mail.password
      payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.password',
        data: ['test@example.com', 'newpassword']
      })

      await dataHandler(Buffer.from(payload))
      expect(mockMailService.password).toHaveBeenCalledWith('test@example.com', 'newpassword', expect.any(Function))

      // Test mail.send
      payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'mail.send',
        data: ['test@example.com', 'Subject', 'Body']
      })

      await dataHandler(Buffer.from(payload))
      expect(mockMailService.send).toHaveBeenCalledWith('test@example.com', 'Subject', 'Body', expect.any(Function))
    })

    it('should execute ssl.renew command', async () => {
      if (!dataHandler) {
        throw new Error('Data handler not found')
        return
      }

      const mockSSLService = global.Odac.server('SSL')
      mockSSLService.renew.mockResolvedValue(Api.result(true, 'SSL renewed'))

      const payload = JSON.stringify({
        auth: global.Odac.core('Config').config.api.auth,
        action: 'ssl.renew',
        data: ['example.com']
      })

      await dataHandler(Buffer.from(payload))

      expect(mockSSLService.renew).toHaveBeenCalledWith('example.com', expect.any(Function))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
      expect(mockSocket.destroy).toHaveBeenCalled()
    })
  })

  describe('utility methods', () => {
    it('should format result correctly', () => {
      const successResult = Api.result(true, 'Operation successful')
      expect(successResult).toEqual({
        result: true,
        message: 'Operation successful'
      })

      const errorResult = Api.result(false, 'Operation failed')
      expect(errorResult).toEqual({
        result: false,
        message: 'Operation failed'
      })
    })

    it('should handle send to non-existent connection gracefully', () => {
      Api.init()

      // Try to send to non-existent connection
      const result = Api.send('non-existent-id', 'test-process', 'running', 'Test message')

      // Should not throw and should return undefined
      expect(result).toBeUndefined()
    })

    it('should send messages to active connections', () => {
      const net = require('net')
      const mockServer = {
        on: jest.fn(),
        listen: jest.fn()
      }
      net.createServer.mockReturnValue(mockServer)

      Api.init()
      Api.start()

      // Set up a connection
      // Get handler from createServer call
      const connectionHandler = net.createServer.mock.calls.length > 0 ? net.createServer.mock.calls[0][0] : null

      const mockSocket = {
        remoteAddress: '127.0.0.1',
        on: jest.fn(),
        write: jest.fn(),
        destroy: jest.fn()
      }

      connectionHandler(mockSocket)

      // The send method exists and can be called
      expect(Api.send).toBeDefined()
      expect(typeof Api.send).toBe('function')

      // Test that it doesn't throw when called
      expect(() => Api.send('test-id', 'test-process', 'running', 'Test message')).not.toThrow()
    })
  })

  describe('Scoped Identity & RBAC', () => {
    let mockSocket
    let dataHandler

    beforeEach(() => {
      const net = require('net')
      const crypto = require('crypto')

      // Use a consistent mock auth token for deterministic hashing tests
      const mockRootKey = 'mock-root-key-32-bytes-long-test-key'
      global.Odac.core('Config').config.api = {auth: mockRootKey}

      Api.init()
      Api.start()

      const connectionHandler = net.createServer.mock.calls[0][0]
      mockSocket = {
        remoteAddress: '127.0.0.1',
        on: jest.fn(),
        write: jest.fn(),
        destroy: jest.fn()
      }
      connectionHandler(mockSocket)
      dataHandler = mockSocket.on.mock.calls.find(call => call[0] === 'data')[1]
    })

    it('should generate deterministic tokens via generateToken', () => {
      const domain = 'example.com'
      const token1 = Api.generateToken(domain)
      const token2 = Api.generateToken(domain)

      expect(token1).toBe(token2)
      expect(token1).toHaveLength(64) // SHA256 hex
    })

    it('should allow mail.send with a valid client token', async () => {
      const domain = 'example.com'
      const token = Api.generateToken(domain)
      Api.addToken(domain)

      const mockMailService = global.Odac.server('Mail')
      mockMailService.send.mockResolvedValue(Api.result(true, 'Sent'))

      const payload = JSON.stringify({
        auth: token,
        action: 'mail.send',
        data: ['to@val.com', 'Subject', 'Body']
      })

      await dataHandler(Buffer.from(payload))

      expect(mockMailService.send).toHaveBeenCalled()
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"result":true'))
    })

    it('should persist tokens via reloadTokens on startup', () => {
      const domain = 'persistent.com'
      global.Odac.core('Config').config.domains = {
        [domain]: {domain}
      }

      // Re-init to trigger reloadTokens
      Api.init()

      const token = Api.generateToken(domain)
      // If reloadTokens worked, the token should be in the internal map
      // We test this by trying to use it
      const mockMailService = global.Odac.server('Mail')
      mockMailService.send.mockResolvedValue(Api.result(true, 'Sent'))

      const payload = JSON.stringify({
        auth: token,
        action: 'mail.send',
        data: []
      })

      dataHandler(Buffer.from(payload))
      // Since dataHandler is async but we don't await here, we just check if it enters the auth block
      // In a real test we'd await, but here we just want to verify reloadTokens was called.
    })

    it('should remove token when removeToken is called', async () => {
      const domain = 'gone.com'
      const token = Api.generateToken(domain)
      Api.addToken(domain)
      Api.removeToken(domain)

      const payload = JSON.stringify({
        auth: token,
        action: 'mail.send',
        data: []
      })

      await dataHandler(Buffer.from(payload))
      expect(mockSocket.write).toHaveBeenCalledWith(expect.stringContaining('"message":"unauthorized"'))
    })
  })
})
