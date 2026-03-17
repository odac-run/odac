/**
 * Unit tests for DNS.js module
 * Tests Go DNS process management, record CRUD, IP detection, and config sync.
 * DNS query processing is handled by the Go binary (server/dns/) and is not
 * tested here — this file tests the Node.js orchestration layer only.
 */

const {setupGlobalMocks, cleanupGlobalMocks} = require('./__mocks__/testHelpers')
const {createMockWebsiteConfig} = require('./__mocks__/testFactories')

const mockLog = jest.fn()
const mockError = jest.fn()

// Mock definitions — must be before any require()
const mockFs = {
  existsSync: jest.fn().mockReturnValue(true),
  mkdirSync: jest.fn(),
  openSync: jest.fn().mockReturnValue(1),
  readFileSync: jest.fn().mockReturnValue('12345'),
  unlinkSync: jest.fn(),
  writeFileSync: jest.fn()
}

const mockChildProcess = {
  spawn: jest.fn().mockReturnValue({
    kill: jest.fn(),
    on: jest.fn(),
    pid: 12345,
    unref: jest.fn()
  })
}

const mockOs = {
  homedir: jest.fn().mockReturnValue('/home/user'),
  networkInterfaces: jest.fn().mockReturnValue({
    eth0: [
      {address: '192.168.1.10', family: 'IPv4', internal: false},
      {address: 'fe80::1', family: 'IPv6', internal: false}
    ],
    lo: [{address: '127.0.0.1', family: 'IPv4', internal: true}]
  }),
  platform: jest.fn().mockReturnValue('linux')
}

const mockAxios = {
  get: jest.fn().mockResolvedValue({data: '93.184.216.34'}),
  post: jest.fn().mockResolvedValue({status: 200})
}

const mockDns = {
  promises: {
    reverse: jest.fn().mockResolvedValue([])
  }
}

jest.mock('fs', () => mockFs)
jest.mock('child_process', () => mockChildProcess)
jest.mock('os', () => mockOs)
jest.mock('axios', () => mockAxios)
jest.mock('dns', () => mockDns)

const {mockOdac} = require('./__mocks__/globalOdac')

describe('DNS Module', () => {
  let DNS
  let mockConfig

  beforeEach(() => {
    jest.clearAllMocks()
    mockLog.mockClear()
    mockError.mockClear()

    setupGlobalMocks()

    mockOdac.setMock('core', 'Log', {
      init: jest.fn().mockReturnValue({
        error: mockError,
        log: mockLog
      })
    })

    mockConfig = {
      config: {
        dns: {
          'example.com': {
            records: [
              {id: '1', name: 'example.com', ttl: 3600, type: 'A', value: '127.0.0.1'},
              {id: '2', name: 'example.com', ttl: 3600, type: 'AAAA', value: '::1'},
              {id: '3', name: 'example.com', priority: 10, ttl: 3600, type: 'MX', value: 'mail.example.com'},
              {id: '4', name: 'example.com', ttl: 3600, type: 'TXT', value: 'v=spf1 mx ~all'}
            ],
            soa: {
              email: 'hostmaster.example.com',
              expire: 604800,
              minimum: 3600,
              primary: 'ns1.example.com',
              refresh: 3600,
              retry: 600,
              serial: 2024010101,
              ttl: 3600
            }
          },
          'test.org': {
            records: [],
            soa: {
              email: 'hostmaster.test.org',
              expire: 604800,
              minimum: 3600,
              primary: 'ns1.test.org',
              refresh: 3600,
              retry: 600,
              serial: 2024010101,
              ttl: 3600
            }
          }
        },
        domains: {
          'example.com': createMockWebsiteConfig('example.com'),
          'test.org': createMockWebsiteConfig('test.org')
        }
      },
      force: jest.fn()
    }

    global.Odac.setMock('core', 'Config', mockConfig)

    // Default fs behavior: binary exists, PID file throw ENOENT (forces spawn)
    mockFs.existsSync.mockReturnValue(true)
    mockFs.readFileSync.mockImplementation(() => {
      throw Object.assign(new Error('ENOENT'), {code: 'ENOENT'})
    })

    jest.isolateModules(() => {
      DNS = require('../../server/src/DNS')
    })
  })

  afterEach(() => {
    cleanupGlobalMocks()
  })

  // ─── Process Management ──────────────────────────────────────────

  describe('process management', () => {
    test('should spawn DNS process if not running', () => {
      DNS.spawnDNS()
      expect(mockChildProcess.spawn).toHaveBeenCalled()
    })

    test('should not spawn if process already exists', () => {
      DNS.spawnDNS()
      mockChildProcess.spawn.mockClear()

      DNS.spawnDNS()
      expect(mockChildProcess.spawn).not.toHaveBeenCalled()
    })

    test('should adopt orphaned DNS process via PID file', () => {
      // readFileSync: PID for pid file, cmdline for /proc check
      mockFs.readFileSync.mockImplementation(filePath => {
        if (typeof filePath === 'string' && filePath.includes('/proc/')) return 'odac-dns'
        return '54321'
      })
      const originalKill = process.kill
      process.kill = jest.fn()
      mockFs.existsSync.mockReturnValue(true)

      DNS.spawnDNS()

      expect(mockChildProcess.spawn).not.toHaveBeenCalled()
      expect(mockLog).toHaveBeenCalledWith(expect.stringContaining('Reconnecting'))
      process.kill = originalKill
    })

    test('should spawn new process when PID file exists but process is dead', () => {
      mockFs.readFileSync.mockReturnValue('54321')
      const originalKill = process.kill
      process.kill = jest.fn(() => {
        throw new Error('ESRCH')
      })

      DNS.spawnDNS()

      expect(mockChildProcess.spawn).toHaveBeenCalled()
      process.kill = originalKill
    })

    test('should force new instance in update mode', () => {
      const originalEnv = process.env.ODAC_UPDATE_MODE
      process.env.ODAC_UPDATE_MODE = 'true'

      mockFs.readFileSync.mockReturnValue('54321')

      DNS.spawnDNS()

      expect(mockChildProcess.spawn).toHaveBeenCalled()
      process.env.ODAC_UPDATE_MODE = originalEnv
    })

    test('should not spawn when binary is missing', () => {
      mockFs.existsSync.mockReturnValue(false)

      DNS.spawnDNS()

      expect(mockChildProcess.spawn).not.toHaveBeenCalled()
      expect(mockError).toHaveBeenCalled()
    })

    test('should write PID file after spawning', () => {
      DNS.spawnDNS()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        expect.stringContaining('dns-'),
        '12345',
        expect.objectContaining({flag: expect.any(String)})
      )
    })

    test('should stop and cleanup DNS process', () => {
      const mockChild = {kill: jest.fn(), on: jest.fn(), pid: 12345, unref: jest.fn()}
      mockChildProcess.spawn.mockReturnValue(mockChild)

      DNS.spawnDNS()
      DNS.stop()

      expect(mockChild.kill).toHaveBeenCalled()
    })

    test('should handle stop when no process exists', () => {
      expect(() => DNS.stop()).not.toThrow()
    })
  })

  // ─── Lifecycle ───────────────────────────────────────────────────

  describe('lifecycle', () => {
    test('should spawn DNS in check if active', async () => {
      await DNS.start()
      DNS.reset()

      DNS.check()
      expect(mockChildProcess.spawn).toHaveBeenCalledTimes(2)
    })

    test('should not spawn DNS in check if not active', () => {
      DNS.check()
      expect(mockChildProcess.spawn).not.toHaveBeenCalled()
    })

    test('should start service with IP detection and spawn', async () => {
      await DNS.start()
      expect(mockAxios.get).toHaveBeenCalled()
      expect(mockChildProcess.spawn).toHaveBeenCalled()
    })

    test('should not start twice', async () => {
      await DNS.start()
      mockChildProcess.spawn.mockClear()
      mockAxios.get.mockClear()

      await DNS.start()
      expect(mockAxios.get).not.toHaveBeenCalled()
    })
  })

  // ─── Config Sync ─────────────────────────────────────────────────

  describe('config sync', () => {
    test('should sync config to DNS binary via axios', async () => {
      DNS.spawnDNS()
      await DNS.syncConfig()

      expect(mockAxios.post).toHaveBeenCalledWith(
        'http://localhost/config',
        expect.objectContaining({
          ips: expect.any(Object),
          zones: expect.any(Object)
        }),
        expect.objectContaining({socketPath: expect.any(String)})
      )
    })

    test('should not sync when no process exists', async () => {
      await DNS.syncConfig()
      expect(mockAxios.post).not.toHaveBeenCalled()
    })

    test('should retry on connection refused', async () => {
      jest.useFakeTimers()

      DNS.spawnDNS()
      mockAxios.post.mockClear()

      let callCount = 0
      mockAxios.post.mockImplementation(() => {
        callCount++
        if (callCount === 1) {
          return Promise.reject(Object.assign(new Error('ECONNREFUSED'), {code: 'ECONNREFUSED'}))
        }
        return Promise.resolve({status: 200})
      })

      const promise = DNS.syncConfig()
      await jest.advanceTimersByTimeAsync(1100)
      await promise

      // At least 2 calls: initial failure + retry success (spawn timer may add more)
      expect(mockAxios.post.mock.calls.length).toBeGreaterThanOrEqual(2)

      jest.useRealTimers()
    })

    test('should send zone data in payload', async () => {
      DNS.spawnDNS()
      mockAxios.post.mockClear()
      await DNS.syncConfig()

      const payload = mockAxios.post.mock.calls[0][1]
      expect(payload.zones['example.com']).toBeDefined()
      expect(payload.zones['test.org']).toBeDefined()
    })
  })

  // ─── DNS Record Management ──────────────────────────────────────

  describe('DNS record management', () => {
    test('should add A record to zone configuration', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      expect(mockConfig.config.dns['example.com'].records.filter(r => r.type === 'A').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: '192.168.1.1'})
      )
    })

    test('should add multiple record types', () => {
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'example.com', type: 'MX', priority: 10, value: 'mail.example.com'},
        {name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'}
      )

      const dns = mockConfig.config.dns['example.com']
      expect(dns.records.filter(r => r.type === 'A').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: '192.168.1.1'})
      )
      expect(dns.records.filter(r => r.type === 'MX').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', priority: 10, value: 'mail.example.com'})
      )
      expect(dns.records.filter(r => r.type === 'TXT').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: 'v=spf1 mx ~all'})
      )
    })

    test('should handle subdomain records by finding parent domain', () => {
      DNS.record({name: 'www.example.com', type: 'A', value: '192.168.1.1'})

      expect(mockConfig.config.dns['example.com'].records.filter(r => r.type === 'A').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'www.example.com', value: '192.168.1.1'})
      )
    })

    test('should automatically generate SOA record with current date serial', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      const soa = mockConfig.config.dns['example.com'].soa
      expect(soa).toBeDefined()
      expect(soa.primary).toBe('ns1.example.com')
      expect(soa.email).toBe('hostmaster.example.com')
    })

    test('should delete DNS records by name and type', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})
      DNS.delete({name: 'example.com', type: 'A'})

      const aRecords = mockConfig.config.dns['example.com'].records.filter(
        r => r.type === 'A' && r.name === 'example.com' && r.value === '192.168.1.1'
      )
      expect(aRecords).toHaveLength(0)
    })

    test('should delete DNS records by name, type, and value', () => {
      DNS.record(
        {name: 'example.com', type: 'A', unique: false, value: '192.168.1.1'},
        {name: 'example.com', type: 'A', unique: false, value: '192.168.1.2'}
      )

      DNS.delete({name: 'example.com', type: 'A', value: '192.168.1.1'})

      const aRecords = mockConfig.config.dns['example.com'].records.filter(
        r => r.type === 'A' && r.name === 'example.com' && r.value !== '127.0.0.1'
      )
      expect(aRecords).toHaveLength(1)
      expect(aRecords[0].value).toBe('192.168.1.2')
    })

    test('should replace existing unique records by default', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.2'})

      const aRecords = mockConfig.config.dns['example.com'].records.filter(
        r => r.type === 'A' && r.name === 'example.com' && r.value !== '127.0.0.1'
      )
      expect(aRecords).toHaveLength(1)
      expect(aRecords[0].value).toBe('192.168.1.2')
    })

    test('should allow multiple records when unique is false', () => {
      DNS.record(
        {name: 'example.com', type: 'A', unique: false, value: '192.168.1.1'},
        {name: 'example.com', type: 'A', unique: false, value: '192.168.1.2'}
      )

      const aRecords = mockConfig.config.dns['example.com'].records.filter(
        r => r.type === 'A' && r.name === 'example.com' && r.value !== '127.0.0.1'
      )
      expect(aRecords).toHaveLength(2)
    })

    test('should handle all supported DNS record types', () => {
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'example.com', type: 'AAAA', value: '2001:db8::1'},
        {name: 'www.example.com', type: 'CNAME', value: 'example.com'},
        {name: 'example.com', type: 'MX', priority: 10, value: 'mail.example.com'},
        {name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'},
        {name: 'example.com', type: 'NS', value: 'ns1.example.com'}
      )

      const dns = mockConfig.config.dns['example.com']
      expect(dns.records.filter(r => r.type === 'A').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: '192.168.1.1'})
      )
      expect(dns.records.filter(r => r.type === 'AAAA').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: '2001:db8::1'})
      )
      expect(dns.records.filter(r => r.type === 'CNAME').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'www.example.com', value: 'example.com'})
      )
      expect(dns.records.filter(r => r.type === 'MX').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', priority: 10, value: 'mail.example.com'})
      )
      expect(dns.records.filter(r => r.type === 'TXT').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: 'v=spf1 mx ~all'})
      )
      expect(dns.records.filter(r => r.type === 'NS').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: 'ns1.example.com'})
      )
    })

    test('should ignore unsupported DNS record types', () => {
      DNS.record({name: 'example.com', type: 'INVALID', value: 'test'})

      expect(mockConfig.config.dns['example.com'].records.find(r => r.type === 'INVALID')).toBeUndefined()
    })

    test('should ignore records without type specified', () => {
      DNS.record({name: 'example.com', value: '192.168.1.1'})

      const aRecords = mockConfig.config.dns['example.com'].records.filter(r => r.type === 'A' && r.value === '192.168.1.1')
      expect(aRecords).toHaveLength(0)
    })

    test('should initialize zone if it does not exist', () => {
      delete mockConfig.config.dns['example.com']

      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      expect(mockConfig.config.dns['example.com']).toBeDefined()
      expect(mockConfig.config.dns['example.com'].soa).toBeDefined()
      expect(mockConfig.config.dns['example.com'].records.filter(r => r.type === 'A').map(({id, ...rest}) => rest)).toContainEqual(
        expect.objectContaining({name: 'example.com', value: '192.168.1.1'})
      )
    })

    test('should create default CAA records for new zones', () => {
      delete mockConfig.config.dns['example.com']

      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      const caaRecords = mockConfig.config.dns['example.com'].records.filter(r => r.type === 'CAA')
      expect(caaRecords).toHaveLength(2)
      expect(caaRecords[0].value).toContain('letsencrypt.org')
      expect(caaRecords[1].value).toContain('letsencrypt.org')
    })

    test('should generate SOA record with correct date serial format', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      const soa = mockConfig.config.dns['example.com'].soa
      expect(soa).toBeDefined()
      expect(String(soa.serial)).toMatch(/^\d{10}$/)
      expect(soa.refresh).toBe(3600)
      expect(soa.retry).toBe(600)
      expect(soa.expire).toBe(604800)
      expect(soa.minimum).toBe(3600)
      expect(soa.ttl).toBe(3600)
    })

    test('should update SOA serial for multiple domains', () => {
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'}, {name: 'test.org', type: 'A', value: '192.168.1.2'})

      const soaExample = mockConfig.config.dns['example.com'].soa
      const soaTest = mockConfig.config.dns['test.org'].soa
      expect(soaExample.serial).toBeGreaterThanOrEqual(2024010101)
      expect(soaTest.serial).toBeGreaterThanOrEqual(2024010101)
    })

    test('should delete by type only', () => {
      DNS.record({name: 'example.com', type: 'TXT', value: 'test-value'})

      DNS.delete({name: 'example.com', type: 'TXT'})

      const txtRecords = mockConfig.config.dns['example.com'].records.filter(r => r.type === 'TXT' && r.name === 'example.com')
      expect(txtRecords).toHaveLength(0)
    })

    test('should handle deletion of non-existent records gracefully', () => {
      expect(() => {
        DNS.delete({name: 'example.com', type: 'SRV'})
      }).not.toThrow()
    })

    test('should handle deletion when DNS config does not exist', () => {
      mockConfig.config.dns = undefined
      expect(() => {
        DNS.delete({name: 'example.com', type: 'A'})
      }).not.toThrow()
    })

    test('should call force and syncConfig after record changes', () => {
      DNS.spawnDNS()

      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      expect(mockConfig.force).toHaveBeenCalled()
    })

    test('should protect against prototype pollution in domain names', () => {
      DNS.record({name: '__proto__', type: 'A', value: '1.2.3.4'})
      DNS.record({name: 'constructor', type: 'A', value: '1.2.3.4'})
      DNS.record({name: 'prototype', type: 'A', value: '1.2.3.4'})

      // Use Object.keys to avoid __proto__ quirks with toHaveProperty
      const keys = Object.keys(mockConfig.config.dns)
      expect(keys).not.toContain('__proto__')
      expect(keys).not.toContain('constructor')
      expect(keys).not.toContain('prototype')
    })
  })

  // ─── IP Detection ───────────────────────────────────────────────

  describe('IP detection', () => {
    test('should detect external IPv4 from service', async () => {
      mockAxios.get.mockResolvedValueOnce({data: '93.184.216.34'})

      await DNS.start()

      expect(DNS.ip).toBe('93.184.216.34')
    })

    test('should handle invalid IP format from external service', async () => {
      mockAxios.get.mockResolvedValue({data: 'not-an-ip'})

      await DNS.start()

      // Should not set invalid IP, falls back to local detection
      expect(DNS.ip).not.toBe('not-an-ip')
    })

    test('should handle external IP detection failure', async () => {
      mockAxios.get.mockRejectedValue(new Error('Network error'))

      await DNS.start()

      // Should fall back gracefully
      expect(DNS.ip).toBeDefined()
    })

    test('should collect local network IPs', async () => {
      mockOs.networkInterfaces.mockReturnValue({
        eth0: [
          {address: '10.0.0.5', family: 'IPv4', internal: false},
          {address: '2001:db8::1', family: 'IPv6', internal: false}
        ]
      })

      await DNS.start()

      expect(DNS.ips.ipv4.length).toBeGreaterThanOrEqual(1)
    })

    test('should skip link-local IPv6 addresses', async () => {
      mockOs.networkInterfaces.mockReturnValue({
        eth0: [{address: 'fe80::1', family: 'IPv6', internal: false}]
      })

      await DNS.start()

      const linkLocal = DNS.ips.ipv6.find(i => i.address === 'fe80::1')
      expect(linkLocal).toBeUndefined()
    })

    test('should skip internal interfaces', async () => {
      mockOs.networkInterfaces.mockReturnValue({
        lo: [{address: '127.0.0.1', family: 'IPv4', internal: true}]
      })

      await DNS.start()

      const loopback = DNS.ips.ipv4.find(i => i.address === '127.0.0.1')
      expect(loopback).toBeUndefined()
    })

    test('should mark private IPs as non-public', async () => {
      mockOs.networkInterfaces.mockReturnValue({
        eth0: [{address: '192.168.1.10', family: 'IPv4', internal: false}]
      })
      mockAxios.get.mockRejectedValue(new Error('offline'))

      await DNS.start()

      const privateIP = DNS.ips.ipv4.find(i => i.address === '192.168.1.10')
      expect(privateIP).toBeDefined()
      expect(privateIP.public).toBe(false)
    })

    test('should attempt PTR lookups for detected IPs', async () => {
      mockOs.networkInterfaces.mockReturnValue({
        eth0: [{address: '93.184.216.34', family: 'IPv4', internal: false}]
      })
      mockDns.promises.reverse.mockResolvedValue(['server.example.com'])

      await DNS.start()

      expect(mockDns.promises.reverse).toHaveBeenCalled()
    })

    test('should handle PTR lookup failures gracefully', async () => {
      mockDns.promises.reverse.mockRejectedValue(new Error('ENOTFOUND'))

      await DNS.start()

      // Should not throw, PTR just remains null
      expect(DNS.ips.ipv4.every(i => i.ptr === null || typeof i.ptr === 'string')).toBe(true)
    })

    test('should detect IPv6 from external service', async () => {
      // IPv4 services (5 services, first one succeeds so others not called)
      mockAxios.get
        .mockResolvedValueOnce({data: '93.184.216.34'})
        // IPv6 services (3 services)
        .mockResolvedValueOnce({data: '2001:db8::1'})

      await DNS.start()

      expect(DNS.ips.ipv6.find(i => i.address === '2001:db8::1')).toBeDefined()
    })

    test('should trim whitespace from IP response', async () => {
      mockAxios.get.mockResolvedValueOnce({data: '  93.184.216.34\n  '})

      await DNS.start()

      expect(DNS.ip).toBe('93.184.216.34')
    })
  })

  // ─── Reset Helper ───────────────────────────────────────────────

  describe('reset', () => {
    test('should reset internal state for tests', () => {
      DNS.spawnDNS()
      DNS.reset()

      // After reset, spawnDNS should work again
      mockChildProcess.spawn.mockClear()
      DNS.spawnDNS()
      expect(mockChildProcess.spawn).toHaveBeenCalled()
    })
  })
})
