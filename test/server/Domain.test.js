/**
 * Unit tests for Domain.js module
 * Tests domain management, DNS integration, and SSL provisioning
 */

// Mock requirements
const {mockOdac} = require('./__mocks__/globalOdac')

describe('Domain', () => {
  let Domain
  let mockConfig
  let mockLog

  beforeEach(() => {
    // Reset mocks
    jest.clearAllMocks()
    mockOdac.resetMocks()

    // Setup Config mock
    mockConfig = mockOdac.core('Config')
    mockConfig.config = {
      domains: {},
      apps: [
        {id: 'app-1', name: 'myapp'},
        {id: 'app-2', name: 'otherapp'}
      ]
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
      result: jest.fn((result, message, data) => {
        if (typeof message === 'object') {
          data = message
          message = undefined
        }
        return {result, success: result, message, data}
      })
    })

    // Setup DNS mock
    mockOdac.setMock('server', 'DNS', {
      record: jest.fn(),
      delete: jest.fn(),
      ip: '1.2.3.4',
      ips: {ipv6: [{public: true, address: '2001:db8::1'}]}
    })

    // Setup SSL mock
    mockOdac.setMock('server', 'SSL', {
      renew: jest.fn()
    })

    // Reset modules to ensure fresh require
    jest.resetModules()

    // Global translation mock
    global.__ = jest.fn((msg, ...args) => {
      let result = msg
      args.forEach(arg => {
        result = result.replace('%s', arg)
      })
      return result
    })

    // Set global Odac BEFORE requiring Domain
    global.Odac = mockOdac

    // Import Domain after mocks
    Domain = require('../../server/src/Domain')
  })

  afterEach(() => {
    delete global.Odac
    delete global.__
  })

  describe('add()', () => {
    test('should add a valid domain to an app', async () => {
      const result = await Domain.add('example.com', 'myapp')

      expect(result.result).toBe(true)
      expect(mockConfig.config.domains['example.com']).toBeDefined()
      expect(mockConfig.config.domains['example.com'].appId).toBe('myapp')

      // Verify DNS records
      const dnsMock = Odac.server('DNS')
      expect(dnsMock.record).toHaveBeenCalled()
      const calls = dnsMock.record.mock.calls[0]
      // Should include A, AAAA, CNAME, MX, TXT (SPF), TXT (DMARC)
      expect(calls.some(r => r.type === 'A')).toBe(true)
      expect(calls.some(r => r.type === 'MX')).toBe(true)
      expect(calls.some(r => r.type === 'TXT' && r.name.includes('_dmarc'))).toBe(true)

      // Verify SSL provisioning
      expect(Odac.server('SSL').renew).toHaveBeenCalledWith('example.com')
    })

    test('should reject invalid domain formats', async () => {
      let result = await Domain.add('invalid', 'myapp')
      expect(result.result).toBe(false)
      expect(result.message).toContain('Invalid domain format')

      result = await Domain.add('../hack.com', 'myapp')
      expect(result.result).toBe(false)
    })

    test('should reject if app does not exist', async () => {
      const result = await Domain.add('example.com', 'nonexistent')

      expect(result.result).toBe(false)
      expect(result.message).toContain('App nonexistent not found')
      expect(mockConfig.config.domains['example.com']).toBeUndefined()
    })

    test('should reject duplicate domains', async () => {
      // Pre-populate domain
      mockConfig.config.domains['example.com'] = {appId: 'myapp'}

      const result = await Domain.add('example.com', 'otherapp')

      expect(result.result).toBe(false)
      expect(result.message).toContain('Domain example.com is already registered')
    })

    test('should skip DNS and SSL for localhost', async () => {
      const result = await Domain.add('localhost', 'myapp')

      expect(result.result).toBe(true)
      expect(mockConfig.config.domains['localhost']).toBeDefined()

      expect(Odac.server('DNS').record).not.toHaveBeenCalled()
      expect(Odac.server('SSL').renew).not.toHaveBeenCalled()
    })

    test('should add a subdomain to an existing parent domain', async () => {
      // Setup parent domain
      mockConfig.config.domains['example.com'] = {appId: 'myapp', created: Date.now()}

      const result = await Domain.add('sub.example.com', 'myapp')

      expect(result.result).toBe(true)
      expect(result.message).toContain('Added sub.example.com as a subdomain of example.com')

      // Verify parent record updated
      const parent = mockConfig.config.domains['example.com']
      expect(parent.subdomain).toContain('sub')

      // Verify subdomain is NOT added as a separate domain
      expect(mockConfig.config.domains['sub.example.com']).toBeUndefined()

      // Verify DNS CNAME record
      const dnsMock = Odac.server('DNS')
      expect(dnsMock.record).toHaveBeenCalledWith({
        name: 'sub.example.com',
        type: 'CNAME',
        value: 'example.com'
      })

      // Verify SSL renew for parent
      expect(Odac.server('SSL').renew).toHaveBeenCalledWith('example.com')
    })
  })

  describe('delete()', () => {
    beforeEach(() => {
      mockConfig.config.domains['example.com'] = {
        appId: 'myapp',
        created: Date.now()
      }
    })

    test('should delete an existing domain', async () => {
      const result = await Domain.delete('example.com')

      expect(result.result).toBe(true)
      expect(mockConfig.config.domains['example.com']).toBeUndefined()

      // Verify DNS cleanup
      expect(Odac.server('DNS').delete).toHaveBeenCalled()
    })

    test('should fail if domain does not exist', async () => {
      const result = await Domain.delete('other.com')

      expect(result.result).toBe(false)
      expect(result.message).toContain('Domain other.com not found')
    })

    test('should delete an existing subdomain', async () => {
      // Setup parent with subdomain
      mockConfig.config.domains['example.com'] = {
        appId: 'myapp',
        created: Date.now(),
        subdomain: ['sub']
      }

      const result = await Domain.delete('sub.example.com')

      expect(result.result).toBe(true)
      expect(result.message).toContain('Subdomain sub removed from example.com')

      // Verify removal from parent
      expect(mockConfig.config.domains['example.com'].subdomain).toStrictEqual([])

      // Verify DNS delete
      const dnsMock = Odac.server('DNS')
      expect(dnsMock.delete).toHaveBeenCalledWith({
        name: 'sub.example.com',
        type: 'CNAME'
      })

      // Verify SSL renew for parent
      expect(Odac.server('SSL').renew).toHaveBeenCalledWith('example.com')
    })
  })

  describe('list()', () => {
    beforeEach(() => {
      mockConfig.config.domains = {
        'app1.com': {appId: 'myapp', created: 1000},
        'app2.com': {appId: 'otherapp', created: 2000}
      }
    })

    test('should list all domains', async () => {
      const result = await Domain.list()

      expect(result.result).toBe(true)
      expect(result.data.some(d => d.domain === 'app1.com')).toBe(true)
      expect(result.data.some(d => d.domain === 'app2.com')).toBe(true)
    })

    test('should filter domains by app', async () => {
      const result = await Domain.list('myapp')

      expect(result.result).toBe(true)
      expect(result.data.some(d => d.domain === 'app1.com')).toBe(true)
      expect(result.data.some(d => d.domain === 'app2.com')).toBe(false)
    })

    test('should return empty list if no domains found', async () => {
      mockConfig.config.domains = {}
      const result = await Domain.list()

      expect(result.result).toBe(true)
      expect(result.data).toEqual([])
    })
  })

  describe('deleteByApp()', () => {
    beforeEach(() => {
      mockConfig.config.domains = {
        'site1.com': {appId: 'myapp', created: 1000},
        'site2.com': {appId: 'myapp', created: 1100},
        'other.com': {appId: 'otherapp', created: 2000}
      }
    })

    test('should delete all domains for a specific app', async () => {
      await Domain.deleteByApp('myapp')

      expect(mockConfig.config.domains['site1.com']).toBeUndefined()
      expect(mockConfig.config.domains['site2.com']).toBeUndefined()
      expect(mockConfig.config.domains['other.com']).toBeDefined()

      // Verify DNS cleanup called for each main domain
      // 8 records * 2 domains = 16 calls (A, AAAA, CNAME, mail A, MX, TXT, DMARC TXT, DKIM TXT)
      const dnsMock = Odac.server('DNS')
      expect(dnsMock.delete).toHaveBeenCalledTimes(16)
    })

    test('should handle apps with no domains gracefully', async () => {
      const initialCount = Object.keys(mockConfig.config.domains).length
      await Domain.deleteByApp('nonexistent-app')
      expect(Object.keys(mockConfig.config.domains).length).toBe(initialCount)
    })
  })
})
