/**
 * Unit tests for SSL.js module
 * Tests SSL certificate management, renewal, and ACME integration
 */

// Mock dependencies
jest.mock('../../server/src/SSL/Acme', () => ({
  create: jest.fn(),
  createCsr: jest.fn(),
  generateKeyPair: jest.fn()
}))
jest.mock('selfsigned')
jest.mock('fs')
jest.mock('os')

const {mockOdac} = require('./__mocks__/globalOdac')

describe('SSL', () => {
  let SSL
  let Acme
  let mockConfig
  let mockLog
  let mockOrderFn
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
    Acme = require('../../server/src/SSL/Acme')
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

    // Setup API mock (matches real Api.result() signature: {result, message})
    mockOdac.setMock('server', 'Api', {
      result: jest.fn((result, message) => ({result, message}))
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

    // Setup Proxy mock for ACME HTTP-01 challenge management
    mockOdac.setMock('server', 'Proxy', {
      deleteACMEChallenge: jest.fn().mockResolvedValue(undefined),
      setACMEChallenge: jest.fn().mockResolvedValue(undefined),
      syncConfig: jest.fn().mockResolvedValue(undefined)
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
    mockOrderFn = jest.fn().mockResolvedValue('mock-certificate')
    Acme.create.mockResolvedValue({order: mockOrderFn})
    Acme.createCsr.mockReturnValue(Buffer.from('mock-csr'))
    Acme.generateKeyPair.mockReturnValue({pem: 'mock-domain-key', privateKey: {}})

    // Import SSL
    SSL = require('../../server/src/SSL')
  })

  afterEach(() => {})

  describe('check()', () => {
    test('should renew expired certificates', async () => {
      await SSL.check()

      // Check for errors first for easier debugging
      if (mockLog.error.mock.calls.length > 0) {
        console.error('SSL Error Logs:', mockLog.error.mock.calls)
      }

      // Should attempt to renew 'expired.com'
      expect(Acme.create).toHaveBeenCalled()
      expect(mockOrderFn).toHaveBeenCalled()

      // Should save new certificate
      expect(fs.writeFileSync).toHaveBeenCalledWith(expect.stringContaining('expired.com.crt'), 'mock-certificate')

      // Verify config update
      const domainConfig = mockConfig.config.domains['expired.com']
      expect(domainConfig.cert.ssl.expiry).toBeGreaterThan(Date.now())
    })

    test('should skip valid certificates', async () => {
      // Fresh module to avoid singleton state leak
      jest.resetModules()
      Acme = require('../../server/src/SSL/Acme')
      fs = require('fs')
      os = require('os')
      selfsigned = require('selfsigned')

      Acme.create.mockResolvedValue({order: jest.fn().mockResolvedValue('mock-certificate')})
      Acme.createCsr.mockReturnValue(Buffer.from('mock-csr'))
      Acme.generateKeyPair.mockReturnValue({pem: 'mock-domain-key', privateKey: {}})
      fs.existsSync.mockReturnValue(true)
      fs.mkdirSync.mockImplementation(() => {})
      fs.writeFileSync.mockImplementation(() => {})
      fs.readFileSync.mockImplementation(path => Buffer.from(path))
      os.homedir.mockReturnValue('/home/user')
      selfsigned.generate.mockReturnValue({private: 'mock-private-key', cert: 'mock-cert'})

      // Both domains have valid certificates with cert paths
      mockConfig.config.domains = {
        'example.com': {
          appId: 'myapp',
          subdomain: ['www'],
          cert: {
            ssl: {
              key: '/home/user/.odac/cert/ssl/example.com.key',
              cert: '/home/user/.odac/cert/ssl/example.com.crt',
              expiry: Date.now() + 1000 * 60 * 60 * 24 * 40
            }
          }
        },
        'expired.com': {
          appId: 'myapp',
          subdomain: [],
          cert: {
            ssl: {
              key: '/home/user/.odac/cert/ssl/expired.com.key',
              cert: '/home/user/.odac/cert/ssl/expired.com.crt',
              expiry: Date.now() + 1000 * 60 * 60 * 24 * 40
            }
          }
        }
      }

      // Mock X509Certificate for SAN mismatch check — return matching SANs
      const nodeCrypto = require('crypto')
      const originalX509 = nodeCrypto.X509Certificate
      nodeCrypto.X509Certificate = jest.fn().mockImplementation(buf => {
        const certPath = buf.toString()
        if (certPath.endsWith('example.com.crt')) {
          return {subjectAltName: 'DNS:example.com, DNS:www.example.com'}
        }
        return {subjectAltName: 'DNS:expired.com'}
      })

      SSL = require('../../server/src/SSL')
      await SSL.check()

      expect(Acme.create).not.toHaveBeenCalled()

      // Restore
      nodeCrypto.X509Certificate = originalX509
    })
  })

  describe('renew()', () => {
    test('should renew certificate for valid domain', async () => {
      const result = await SSL.renew('example.com')

      expect(result.result).toBe(true)

      // Wait for detached async operation
      await wait()
      await wait() // Wait enough ticks

      if (mockLog.error.mock.calls.length > 0) {
        console.error('SSL Error Logs:', mockLog.error.mock.calls)
      }

      expect(Acme.create).toHaveBeenCalled()

      // Verify DNS challenge creation
      const orderOpts = mockOrderFn.mock.calls[0][0]

      // Simulate challenge creation callback
      const challengeFn = orderOpts.challengeCreateFn
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

      expect(result.result).toBe(true)

      // Wait for detached async operation
      await wait()
      await wait()

      expect(Acme.create).toHaveBeenCalled()
      // Should renew for the main domain 'example.com'
      expect(fs.writeFileSync).toHaveBeenCalledWith(expect.stringContaining('example.com.crt'), expect.any(String))
    })

    test('should use http-01 as primary challenge', async () => {
      const result = await SSL.renew('example.com')
      expect(result.result).toBe(true)

      await wait()
      await wait()

      expect(Acme.create).toHaveBeenCalled()

      // First order() call should use http-01
      const orderOpts = mockOrderFn.mock.calls[0][0]
      expect(orderOpts.challengeType).toBe('http-01')
    })

    test('should fallback to dns-01 when http-01 fails', async () => {
      // First order call (http-01) rejects, second (dns-01) resolves
      const orderFn = jest
        .fn()
        .mockRejectedValueOnce(new Error('HTTP-01 challenge validation failed'))
        .mockResolvedValueOnce('mock-certificate-dns')

      jest.resetModules()
      Acme = require('../../server/src/SSL/Acme')
      fs = require('fs')
      os = require('os')
      selfsigned = require('selfsigned')

      Acme.create.mockResolvedValue({order: orderFn})
      Acme.createCsr.mockReturnValue(Buffer.from('mock-csr'))
      Acme.generateKeyPair.mockReturnValue({pem: 'mock-domain-key', privateKey: {}})
      fs.existsSync.mockReturnValue(true)
      fs.mkdirSync.mockImplementation(() => {})
      fs.writeFileSync.mockImplementation(() => {})
      os.homedir.mockReturnValue('/home/user')
      selfsigned.generate.mockReturnValue({private: 'mock-private-key', cert: 'mock-cert'})

      // Only one domain that needs renewal to isolate the fallback test
      mockConfig.config.domains = {
        'expired.com': {
          appId: 'myapp',
          subdomain: [],
          cert: {ssl: {expiry: Date.now() - 100000}}
        }
      }

      SSL = require('../../server/src/SSL')
      await SSL.check()

      // Should have called order() twice: first http-01, then dns-01
      expect(orderFn).toHaveBeenCalledTimes(2)
      expect(orderFn.mock.calls[0][0].challengeType).toBe('http-01')
      expect(orderFn.mock.calls[1][0].challengeType).toBe('dns-01')

      // Should save the certificate from dns-01 fallback
      expect(fs.writeFileSync).toHaveBeenCalledWith(expect.stringContaining('expired.com.crt'), 'mock-certificate-dns')
    })

    test('should set ACME challenge on proxy for http-01', async () => {
      const result = await SSL.renew('example.com')
      expect(result.result).toBe(true)

      await wait()
      await wait()

      const orderOpts = mockOrderFn.mock.calls[0][0]

      // Simulate HTTP-01 challenge creation
      await orderOpts.challengeCreateFn(
        {identifier: {value: 'example.com'}},
        {type: 'http-01', token: 'test-token-abc123'},
        'test-key-authorization'
      )

      expect(Odac.server('Proxy').setACMEChallenge).toHaveBeenCalledWith('test-token-abc123', 'test-key-authorization')
      expect(Odac.server('DNS').record).not.toHaveBeenCalled()
    })

    test('should remove ACME challenge from proxy for http-01', async () => {
      const result = await SSL.renew('example.com')
      expect(result.result).toBe(true)

      await wait()
      await wait()

      const orderOpts = mockOrderFn.mock.calls[0][0]

      // Simulate HTTP-01 challenge removal
      await orderOpts.challengeRemoveFn(
        {identifier: {value: 'example.com'}},
        {type: 'http-01', token: 'test-token-abc123'},
        'test-key-authorization'
      )

      expect(Odac.server('Proxy').deleteACMEChallenge).toHaveBeenCalledWith('test-token-abc123')
      expect(Odac.server('DNS').delete).not.toHaveBeenCalled()
    })

    test('should create DNS record for dns-01 challenge via fallback', async () => {
      // HTTP-01 fails → DNS-01 fallback. We capture the dns-01 callbacks.
      const orderFn = jest.fn().mockImplementation(opts => {
        // Simulate calling challengeCreateFn for the given type
        if (opts.challengeType === 'http-01') return Promise.reject(new Error('HTTP-01 failed'))
        // For dns-01, invoke the callback so we can verify it
        opts.challengeCreateFn({identifier: {value: 'example.com'}}, {type: 'dns-01'}, 'dns-key-auth')
        return Promise.resolve('mock-cert')
      })

      Acme.create.mockResolvedValue({order: orderFn})

      const result = await SSL.renew('example.com')
      expect(result.result).toBe(true)

      await wait()
      await wait()

      expect(Odac.server('DNS').record).toHaveBeenCalledWith(
        expect.objectContaining({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'dns-key-auth'
        })
      )
      // Proxy should NOT have been called for dns-01 flow
      expect(Odac.server('Proxy').setACMEChallenge).not.toHaveBeenCalled()
    })

    test('should fail for non-existent domain', async () => {
      const result = await SSL.renew('unknown.com')

      expect(result.result).toBe(false)
      expect(result.message).toContain('Domain unknown.com not found')
      expect(Acme.create).not.toHaveBeenCalled()
    })

    test('should fail for IP addresses', async () => {
      const result = await SSL.renew('1.2.3.4')

      expect(result.result).toBe(false)
      expect(result.message).toContain('SSL renewal is not available for IP addresses')
    })
  })

  describe('cancellation', () => {
    test('should cancel in-progress SSL when new request arrives for same domain', async () => {
      let resolveFirstAuto
      const firstAutoPromise = new Promise(resolve => {
        resolveFirstAuto = resolve
      })

      // First ACME order call hangs until we resolve it
      const orderFn = jest.fn().mockReturnValueOnce(firstAutoPromise).mockResolvedValueOnce('mock-certificate-v2')

      Acme.create.mockResolvedValue({order: orderFn})

      // Trigger first SSL (will block on auto())
      SSL.renew('expired.com')
      await wait()
      await wait()

      // Trigger second SSL for same domain — should cancel the first
      SSL.renew('expired.com')
      await wait()

      // Resolve the first auto — certificate should be discarded
      resolveFirstAuto('mock-certificate-stale')
      await wait()
      await wait()
      await wait()

      // Only the second certificate should be saved
      const writeCalls = fs.writeFileSync.mock.calls.filter(call => typeof call[0] === 'string' && call[0].includes('expired.com.crt'))

      if (writeCalls.length > 0) {
        const lastSavedCert = writeCalls[writeCalls.length - 1][1]
        expect(lastSavedCert).toBe('mock-certificate-v2')
        expect(lastSavedCert).not.toBe('mock-certificate-stale')
      }
    })
  })
})
