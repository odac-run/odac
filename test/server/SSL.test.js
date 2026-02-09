/**
 * Unit tests for SSL.js module
 * Tests SSL certificate management, renewal, and ACME integration
 */

// Mock dependencies
jest.mock('acme-client', () => ({
  Client: jest.fn(),
  forge: {
    createPrivateKey: jest.fn(),
    createCsr: jest.fn()
  },
  directory: {
    letsencrypt: {
      production: 'mock-url'
    }
  }
}))
jest.mock('selfsigned')
jest.mock('fs')
jest.mock('os')

const {mockOdac} = require('./__mocks__/globalOdac')

describe('SSL', () => {
  let SSL
  let mockConfig
  let mockLog
  let acme
  let selfsigned
  let fs
  let os

  // Helper to wait for detached promises
  const wait = () => new Promise(resolve => setImmediate(resolve))

  beforeEach(() => {
    jest.resetModules()
    jest.clearAllMocks()
    mockOdac.resetMocks()

    // Re-require mocked modules to get fresh instances
    acme = require('acme-client')
    selfsigned = require('selfsigned')
    fs = require('fs')
    os = require('os')

    // Setup Config mock
    mockConfig = mockOdac.core('Config')
    mockConfig.config = {
      domains: {
        'example.com': {
          appId: 'myapp',
          subdomain: ['www'],
          cert: {
            ssl: {
              expiry: Date.now() + 1000 * 60 * 60 * 24 * 40 // > 30 days (valid)
            }
          }
        },
        'expired.com': {
          appId: 'myapp',
          subdomain: [],
          cert: {
            ssl: {
              expiry: Date.now() - 100000 // expired
            }
          }
        }
      },
      ssl: {}
    }

    // Setup Log mock
    const mockLogInstance = {
      log: jest.fn(),
      error: jest.fn(),
      init: jest.fn().mockReturnThis()
    }
    mockOdac.setMock('server', 'Log', {
      init: jest.fn().mockReturnValue(mockLogInstance)
    })
    mockLog = mockLogInstance

    // Setup API mock
    mockOdac.setMock('server', 'Api', {
      result: jest.fn((success, message) => ({success, message}))
    })

    // Setup DNS mock
    mockOdac.setMock('server', 'DNS', {
      record: jest.fn(),
      delete: jest.fn(),
      ip: '1.2.3.4',
      ips: {ipv6: [{public: true, address: '2001:db8::1'}]}
    })

    // Setup Web & Mail mocks for cache clearing
    mockOdac.setMock('server', 'Web', {
      clearSSLCache: jest.fn()
    })
    mockOdac.setMock('server', 'Mail', {
      clearSSLCache: jest.fn()
    })

    // Global translation mock
    global.__ = jest.fn((msg, ...args) => msg.replace('%s', args[0]))
    global.Odac = mockOdac

    // Mock fs
    fs.existsSync.mockReturnValue(true)
    fs.mkdirSync.mockImplementation(() => {})
    fs.writeFileSync.mockImplementation(() => {})

    // Mock os
    os.homedir.mockReturnValue('/home/user')

    // Mock selfsigned
    selfsigned.generate.mockReturnValue({
      private: 'mock-private-key',
      cert: 'mock-cert'
    })

    // Setup default ACME mock implementations
    acme.forge.createPrivateKey.mockResolvedValue('mock-account-key')
    acme.forge.createCsr.mockResolvedValue(['mock-domain-key', 'mock-csr'])
    acme.Client.mockImplementation(() => ({
      auto: jest.fn().mockResolvedValue('mock-certificate')
    }))

    // Import SSL
    SSL = require('../../server/src/SSL')
  })

  afterEach(() => {
    delete global.Odac
    delete global.__
  })

  describe('check()', () => {
    test('should renew expired certificates', async () => {
      await SSL.check()

      // Check for errors first for easier debugging
      if (mockLog.error.mock.calls.length > 0) {
        console.error('SSL Error Logs:', mockLog.error.mock.calls)
      }

      // Should attempt to renew 'expired.com'
      expect(acme.Client).toHaveBeenCalled()

      // Get the returned client object from the mock results
      const clientInstance = acme.Client.mock.results[0].value
      expect(clientInstance.auto).toHaveBeenCalled()

      // Should save new certificate
      expect(fs.writeFileSync).toHaveBeenCalledWith(expect.stringContaining('expired.com.crt'), 'mock-certificate')

      // Verify config update
      const domainConfig = mockConfig.config.domains['expired.com']
      expect(domainConfig.cert.ssl.expiry).toBeGreaterThan(Date.now())
    })

    test('should skip valid certificates', async () => {
      // clear invocations to be sure
      acme.Client.mockClear()

      // Remove expired domain to focus on valid one
      mockConfig.config.domains['expired.com'].cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 40

      await SSL.check()

      expect(acme.Client).not.toHaveBeenCalled()
    })
  })

  describe('renew()', () => {
    test('should renew certificate for valid domain', async () => {
      const result = await SSL.renew('example.com')

      expect(result.success).toBe(true)

      // Wait for detached async operation
      await wait()
      await wait() // Wait enough ticks

      if (mockLog.error.mock.calls.length > 0) {
        console.error('SSL Error Logs:', mockLog.error.mock.calls)
      }

      expect(acme.Client).toHaveBeenCalled()

      // Verify DNS challenge creation
      const clientInstance = acme.Client.mock.results[0].value
      const autoOpts = clientInstance.auto.mock.calls[0][0]

      // Simulate challenge creation callback
      const challengeFn = autoOpts.challengeCreateFn
      await challengeFn({identifier: {value: 'example.com'}}, {type: 'dns-01'}, 'mock-auth')

      expect(Odac.server('DNS').record).toHaveBeenCalledWith(
        expect.objectContaining({
          name: '_acme-challenge.example.com',
          type: 'TXT'
        })
      )
    })

    test('should handle subdomain lookups', async () => {
      // Mock lookup where 'www.example.com' maps to 'example.com'
      const result = await SSL.renew('www.example.com')

      expect(result.success).toBe(true)

      // Wait for detached async operation
      await wait()
      await wait()

      expect(acme.Client).toHaveBeenCalled()
      // Should renew for the main domain 'example.com'
      expect(fs.writeFileSync).toHaveBeenCalledWith(expect.stringContaining('example.com.crt'), expect.any(String))
    })

    test('should fail for non-existent domain', async () => {
      const result = await SSL.renew('unknown.com')

      expect(result.success).toBe(false)
      expect(result.message).toContain('Domain unknown.com not found')
      expect(acme.Client).not.toHaveBeenCalled()
    })

    test('should fail for IP addresses', async () => {
      const result = await SSL.renew('1.2.3.4')

      expect(result.success).toBe(false)
      expect(result.message).toContain('SSL renewal is not available for IP addresses')
    })
  })
})
