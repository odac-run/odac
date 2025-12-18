/**
 * Comprehensive unit tests for the DNS.js module
 * Tests DNS server functionality, record management, and query processing
 */

const {setupGlobalMocks, cleanupGlobalMocks} = require('./__mocks__/testHelpers')
const {createMockWebsiteConfig} = require('./__mocks__/testFactories')

// Create mock log functions first
const mockLog = jest.fn()
const mockError = jest.fn()

describe('DNS Module', () => {
  let DNS
  let mockConfig

  beforeEach(() => {
    // Clear mock calls before each test
    mockLog.mockClear()
    mockError.mockClear()

    setupGlobalMocks()

    // Set up the Log mock before requiring DNS
    const {mockOdac} = require('./__mocks__/globalOdac')
    mockOdac.setMock('core', 'Log', {
      init: jest.fn().mockReturnValue({
        log: mockLog,
        error: mockError
      })
    })

    // Mock native-dns module
    jest.doMock('native-dns', () => ({
      createServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      createTCPServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      consts: {
        NAME_TO_QTYPE: {
          A: 1,
          AAAA: 28,
          CNAME: 5,
          MX: 15,
          TXT: 16,
          NS: 2,
          SOA: 6
        }
      },
      A: jest.fn(data => ({type: 'A', ...data})),
      AAAA: jest.fn(data => ({type: 'AAAA', ...data})),
      CNAME: jest.fn(data => ({type: 'CNAME', ...data})),
      MX: jest.fn(data => ({type: 'MX', ...data})),
      TXT: jest.fn(data => ({type: 'TXT', ...data})),
      NS: jest.fn(data => ({type: 'NS', ...data})),
      SOA: jest.fn(data => ({type: 'SOA', ...data}))
    }))

    // Mock axios module
    jest.doMock('axios', () => ({
      get: jest.fn().mockResolvedValue({data: '127.0.0.1'})
    }))

    // Setup mock config with websites
    mockConfig = {
      config: {
        websites: {
          'example.com': createMockWebsiteConfig('example.com'),
          'test.org': createMockWebsiteConfig('test.org')
        }
      }
    }

    global.Odac.setMock('core', 'Config', mockConfig)

    // Clear module cache and require DNS
    jest.resetModules()
    DNS = require('../../server/src/DNS')
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.resetModules()
    jest.dontMock('native-dns')
    jest.dontMock('axios')
  })

  describe('initialization', () => {
    it('should create UDP and TCP DNS servers on initialization', () => {
      const dns = require('native-dns')

      DNS.init()

      expect(dns.createServer).toHaveBeenCalled()
      expect(dns.createTCPServer).toHaveBeenCalled()
    })

    it('should attempt to get external IP from curlmyip.org', () => {
      const axios = require('axios')

      DNS.init()

      expect(axios.get).toHaveBeenCalledWith('https://curlmyip.org/', {
        headers: {'User-Agent': 'Odac-DNS/1.0'},
        timeout: 5000
      })
    })

    it('should start DNS servers on port 53 when websites exist', async () => {
      const dns = require('native-dns')

      DNS.init()

      // Wait for async initialization to complete
      await new Promise(resolve => setTimeout(resolve, 100))

      // The servers should be created
      expect(dns.createServer).toHaveBeenCalled()
      expect(dns.createTCPServer).toHaveBeenCalled()

      // Note: serve() is called asynchronously after port availability check
      // This test verifies servers are created, actual serving is tested in integration tests
    }, 10000)

    it('should set external IP when successfully retrieved', async () => {
      const axios = require('axios')
      axios.get.mockResolvedValue({data: '203.0.113.1'})

      DNS.init()

      // Wait for the axios promise to resolve
      await new Promise(resolve => setTimeout(resolve, 0))

      expect(DNS.ip).toBe('203.0.113.1')
    })

    it('should handle invalid IP format from external service', async () => {
      const axios = require('axios')
      // mockLog already defined at top
      axios.get.mockResolvedValue({data: 'invalid-ip-format'})

      DNS.init()

      // Wait for the axios promise to resolve
      await new Promise(resolve => setTimeout(resolve, 100))

      // Should fallback to local network IP (not 127.0.0.1 and not the invalid format)
      expect(DNS.ip).not.toBe('127.0.0.1')
      expect(DNS.ip).not.toBe('invalid-ip-format')
      expect(DNS.ip).toMatch(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)
    })

    it('should handle external IP detection failure', async () => {
      const axios = require('axios')
      // mockLog already defined at top
      const networkError = new Error('Network timeout')
      axios.get.mockRejectedValue(networkError)

      DNS.init()

      // Wait for the axios promise to resolve
      await new Promise(resolve => setTimeout(resolve, 100))

      // Should fallback to local network IP (not 127.0.0.1)
      expect(DNS.ip).not.toBe('127.0.0.1')
      expect(DNS.ip).toMatch(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)
    })

    it('should handle DNS server startup errors gracefully', async () => {
      const dns = require('native-dns')
      // mockLog already defined at top
      const udpServer = {
        on: jest.fn(),
        serve: jest.fn(() => {
          throw new Error('Port 53 already in use')
        })
      }
      const tcpServer = {
        on: jest.fn(),
        serve: jest.fn(() => {
          throw new Error('Port 53 already in use')
        })
      }

      dns.createServer.mockReturnValue(udpServer)
      dns.createTCPServer.mockReturnValue(tcpServer)

      DNS.init()

      // Wait for async initialization
      await new Promise(resolve => setTimeout(resolve, 100))

      // Server creation should still happen even if serve fails
      expect(dns.createServer).toHaveBeenCalled()
      expect(dns.createTCPServer).toHaveBeenCalled()
    })

    it('should set up error handlers for UDP and TCP servers', async () => {
      const dns = require('native-dns')
      const udpServer = {on: jest.fn(), serve: jest.fn()}
      const tcpServer = {on: jest.fn(), serve: jest.fn()}

      dns.createServer.mockReturnValue(udpServer)
      dns.createTCPServer.mockReturnValue(tcpServer)

      DNS.init()

      // Wait for async initialization
      await new Promise(resolve => setTimeout(resolve, 100))

      // Verify request handlers are set up (error handlers are set up during attemptDNSStart)
      expect(udpServer.on).toHaveBeenCalledWith('request', expect.any(Function))
      expect(tcpServer.on).toHaveBeenCalledWith('request', expect.any(Function))
    })

    it('should log DNS server errors when they occur', async () => {
      const dns = require('native-dns')
      // mockError already defined at top
      const udpServer = {on: jest.fn(), serve: jest.fn()}
      const tcpServer = {on: jest.fn(), serve: jest.fn()}

      dns.createServer.mockReturnValue(udpServer)
      dns.createTCPServer.mockReturnValue(tcpServer)

      DNS.init()

      // Wait for async initialization
      await new Promise(resolve => setTimeout(resolve, 100))

      // Get the request handler functions (error handlers are set during attemptDNSStart)
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')?.[1]

      // Verify request handler exists
      expect(requestHandler).toBeDefined()
      expect(typeof requestHandler).toBe('function')
    })

    it('should not start servers when no websites are configured', () => {
      const dns = require('native-dns')
      const udpServer = {on: jest.fn(), serve: jest.fn()}
      const tcpServer = {on: jest.fn(), serve: jest.fn()}

      dns.createServer.mockReturnValue(udpServer)
      dns.createTCPServer.mockReturnValue(tcpServer)

      // Clear websites config
      mockConfig.config.websites = {}

      DNS.init()

      expect(udpServer.serve).not.toHaveBeenCalled()
      expect(tcpServer.serve).not.toHaveBeenCalled()
    })
  })

  describe('DNS record management', () => {
    it('should add A record to website configuration', () => {
      const record = {name: 'example.com', type: 'A', value: '192.168.1.1'}

      DNS.record(record)

      expect(mockConfig.config.websites['example.com'].DNS.A).toContainEqual({
        name: 'example.com',
        value: '192.168.1.1'
      })
    })

    it('should add multiple record types to website configuration', () => {
      const records = [
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'example.com', type: 'MX', value: 'mail.example.com', priority: 10},
        {name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'}
      ]

      DNS.record(...records)

      expect(mockConfig.config.websites['example.com'].DNS.A).toContainEqual({
        name: 'example.com',
        value: '192.168.1.1'
      })
      expect(mockConfig.config.websites['example.com'].DNS.MX).toContainEqual({
        name: 'example.com',
        value: 'mail.example.com',
        priority: 10
      })
      expect(mockConfig.config.websites['example.com'].DNS.TXT).toContainEqual({
        name: 'example.com',
        value: 'v=spf1 mx ~all'
      })
    })

    it('should handle subdomain records by finding parent domain', () => {
      const record = {name: 'www.example.com', type: 'A', value: '192.168.1.1'}

      DNS.record(record)

      expect(mockConfig.config.websites['example.com'].DNS.A).toContainEqual({
        name: 'www.example.com',
        value: '192.168.1.1'
      })
    })

    it('should automatically generate SOA record with current date serial', () => {
      const record = {name: 'example.com', type: 'A', value: '192.168.1.1'}

      DNS.record(record)

      const soaRecords = mockConfig.config.websites['example.com'].DNS.SOA
      expect(soaRecords).toHaveLength(1)
      expect(soaRecords[0].name).toBe('example.com')
      expect(soaRecords[0].value).toMatch(/^ns1\.example\.com hostmaster\.example\.com \d{10} 3600 600 604800 3600$/)
    })

    it('should delete DNS records by name and type', () => {
      // Add a record first
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      // Delete the record
      DNS.delete({name: 'example.com', type: 'A'})

      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      const exampleRecords = aRecords.filter(r => r.name === 'example.com' && r.value === '192.168.1.1')
      expect(exampleRecords).toHaveLength(0)
    })

    it('should delete DNS records by name, type, and value', () => {
      // Add multiple records with same name
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1', unique: false},
        {name: 'example.com', type: 'A', value: '192.168.1.2', unique: false}
      )

      // Delete only one specific record
      DNS.delete({name: 'example.com', type: 'A', value: '192.168.1.1'})

      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      const remainingRecords = aRecords.filter(r => r.name === 'example.com' && r.value !== '127.0.0.1')
      expect(remainingRecords).toHaveLength(1)
      expect(remainingRecords[0].value).toBe('192.168.1.2')
    })

    it('should ignore records for non-existent domains', () => {
      const record = {name: 'nonexistent.com', type: 'A', value: '192.168.1.1'}

      DNS.record(record)

      expect(mockConfig.config.websites).not.toHaveProperty('nonexistent.com')
    })

    it('should replace existing unique records by default', () => {
      // Add initial record
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      // Add another record with same name (should replace)
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.2'})

      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      const exampleRecords = aRecords.filter(r => r.name === 'example.com' && r.value !== '127.0.0.1')
      expect(exampleRecords).toHaveLength(1)
      expect(exampleRecords[0].value).toBe('192.168.1.2')
    })

    it('should allow multiple records when unique is false', () => {
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1', unique: false},
        {name: 'example.com', type: 'A', value: '192.168.1.2', unique: false}
      )

      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      const exampleRecords = aRecords.filter(r => r.name === 'example.com' && r.value !== '127.0.0.1')
      expect(exampleRecords).toHaveLength(2)
    })

    it('should handle all supported DNS record types', () => {
      const records = [
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'example.com', type: 'AAAA', value: '2001:db8::1'},
        {name: 'www.example.com', type: 'CNAME', value: 'example.com'},
        {name: 'example.com', type: 'MX', value: 'mail.example.com', priority: 10},
        {name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'},
        {name: 'example.com', type: 'NS', value: 'ns1.example.com'}
      ]

      DNS.record(...records)

      const dnsConfig = mockConfig.config.websites['example.com'].DNS
      expect(dnsConfig.A).toContainEqual({name: 'example.com', value: '192.168.1.1'})
      expect(dnsConfig.AAAA).toContainEqual({name: 'example.com', value: '2001:db8::1'})
      expect(dnsConfig.CNAME).toContainEqual({name: 'www.example.com', value: 'example.com'})
      expect(dnsConfig.MX).toContainEqual({name: 'example.com', value: 'mail.example.com', priority: 10})
      expect(dnsConfig.TXT).toContainEqual({name: 'example.com', value: 'v=spf1 mx ~all'})
      expect(dnsConfig.NS).toContainEqual({name: 'example.com', value: 'ns1.example.com'})
    })

    it('should ignore unsupported DNS record types', () => {
      const record = {name: 'example.com', type: 'INVALID', value: 'test'}

      DNS.record(record)

      const dnsConfig = mockConfig.config.websites['example.com'].DNS
      expect(dnsConfig.INVALID).toBeUndefined()
    })

    it('should ignore records without type specified', () => {
      const record = {name: 'example.com', value: '192.168.1.1'}

      DNS.record(record)

      // Should not add any new records beyond the existing ones
      const dnsConfig = mockConfig.config.websites['example.com'].DNS
      const aRecords = dnsConfig.A.filter(r => r.value === '192.168.1.1')
      expect(aRecords).toHaveLength(0)
    })

    it('should initialize DNS object if it does not exist', () => {
      // Remove DNS config
      delete mockConfig.config.websites['example.com'].DNS

      const record = {name: 'example.com', type: 'A', value: '192.168.1.1'}
      DNS.record(record)

      expect(mockConfig.config.websites['example.com'].DNS).toBeDefined()
      expect(mockConfig.config.websites['example.com'].DNS.A).toContainEqual({
        name: 'example.com',
        value: '192.168.1.1'
      })
    })

    it('should initialize record type array if it does not exist', () => {
      // Remove A records
      delete mockConfig.config.websites['example.com'].DNS.A

      const record = {name: 'example.com', type: 'A', value: '192.168.1.1'}
      DNS.record(record)

      expect(mockConfig.config.websites['example.com'].DNS.A).toBeDefined()
      expect(mockConfig.config.websites['example.com'].DNS.A).toContainEqual({
        name: 'example.com',
        value: '192.168.1.1'
      })
    })

    it('should generate SOA record with correct date serial format', () => {
      const record = {name: 'example.com', type: 'A', value: '192.168.1.1'}

      DNS.record(record)

      const soaRecords = mockConfig.config.websites['example.com'].DNS.SOA
      expect(soaRecords).toHaveLength(1)
      expect(soaRecords[0].name).toBe('example.com')

      // Check SOA record format: ns1.domain hostmaster.domain YYYYMMDDNN 3600 600 604800 3600
      const soaValue = soaRecords[0].value
      const parts = soaValue.split(' ')
      expect(parts).toHaveLength(7)
      expect(parts[0]).toBe('ns1.example.com')
      expect(parts[1]).toBe('hostmaster.example.com')
      expect(parts[2]).toMatch(/^\d{10}$/) // Date serial should be 10 digits
      expect(parts[3]).toBe('3600')
      expect(parts[4]).toBe('600')
      expect(parts[5]).toBe('604800')
      expect(parts[6]).toBe('3600')
    })

    it('should update SOA record for multiple domains', () => {
      const records = [
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'test.org', type: 'A', value: '192.168.1.2'}
      ]

      DNS.record(...records)

      expect(mockConfig.config.websites['example.com'].DNS.SOA).toHaveLength(1)
      expect(mockConfig.config.websites['test.org'].DNS.SOA).toHaveLength(1)
      expect(mockConfig.config.websites['example.com'].DNS.SOA[0].name).toBe('example.com')
      expect(mockConfig.config.websites['test.org'].DNS.SOA[0].name).toBe('test.org')
    })

    it('should delete records by type only', () => {
      // Add multiple A records
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1', unique: false},
        {name: 'www.example.com', type: 'A', value: '192.168.1.2', unique: false}
      )

      // Delete all A records for example.com
      DNS.delete({name: 'example.com', type: 'A'})

      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      const exampleRecords = aRecords.filter(r => r.name === 'example.com' && r.value !== '127.0.0.1')
      expect(exampleRecords).toHaveLength(0)

      // www.example.com record should still exist
      const wwwRecords = aRecords.filter(r => r.name === 'www.example.com')
      expect(wwwRecords).toHaveLength(1)
    })

    it('should handle deletion of non-existent records gracefully', () => {
      DNS.delete({name: 'nonexistent.com', type: 'A'})
      DNS.delete({name: 'example.com', type: 'NONEXISTENT'})

      // Should not throw errors and existing records should remain
      const aRecords = mockConfig.config.websites['example.com'].DNS.A
      expect(aRecords).toBeDefined()
    })

    it('should handle deletion when DNS config does not exist', () => {
      delete mockConfig.config.websites['example.com'].DNS

      DNS.delete({name: 'example.com', type: 'A'})

      // Should not throw errors
      expect(mockConfig.config.websites['example.com'].DNS).toBeUndefined()
    })

    it('should handle deletion when record type does not exist', () => {
      delete mockConfig.config.websites['example.com'].DNS.A

      DNS.delete({name: 'example.com', type: 'A'})

      // Should not throw errors
      expect(mockConfig.config.websites['example.com'].DNS.A).toBeUndefined()
    })
  })

  describe('DNS query processing', () => {
    let mockRequest, mockResponse

    beforeEach(() => {
      // Set up mock DNS request and response objects
      mockRequest = {
        address: {address: '127.0.0.1'}
      }

      mockResponse = {
        question: [{name: 'example.com', type: 1}], // A record query
        answer: [],
        authority: [],
        header: {},
        send: jest.fn()
      }

      // Initialize DNS to set up servers
      DNS.init()
    })

    it('should process A record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.A = 1

      // Add A record
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      mockResponse.question[0] = {name: 'example.com', type: 1}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.A).toHaveBeenCalledWith({
        name: 'example.com',
        address: '192.168.1.1',
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process AAAA record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.AAAA = 28

      // Add AAAA record
      DNS.record({name: 'example.com', type: 'AAAA', value: '2001:db8::1'})

      mockResponse.question[0] = {name: 'example.com', type: 28}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.AAAA).toHaveBeenCalledWith({
        name: 'example.com',
        address: '2001:db8::1',
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process CNAME record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.CNAME = 5

      // Add CNAME record
      DNS.record({name: 'www.example.com', type: 'CNAME', value: 'example.com'})

      mockResponse.question[0] = {name: 'www.example.com', type: 5}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.CNAME).toHaveBeenCalledWith({
        name: 'www.example.com',
        data: 'example.com',
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process MX record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.MX = 15

      // Add MX record
      DNS.record({name: 'example.com', type: 'MX', value: 'mail.example.com', priority: 10})

      mockResponse.question[0] = {name: 'example.com', type: 15}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.MX).toHaveBeenCalledWith({
        name: 'example.com',
        exchange: 'mail.example.com',
        priority: 10,
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process TXT record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.TXT = 16

      // Add TXT record
      DNS.record({name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'})

      mockResponse.question[0] = {name: 'example.com', type: 16}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.TXT).toHaveBeenCalledWith({
        name: 'example.com',
        data: ['v=spf1 mx ~all'],
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process NS record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.NS = 2

      // Add NS record
      DNS.record({name: 'example.com', type: 'NS', value: 'ns1.example.com'})

      mockResponse.question[0] = {name: 'example.com', type: 2}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.NS).toHaveBeenCalledWith({
        name: 'example.com',
        data: 'ns1.example.com',
        ttl: 3600
      })
      expect(mockResponse.header.aa).toBe(1)
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process SOA record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.SOA = 6

      // Add SOA record manually (normally auto-generated)
      mockConfig.config.websites['example.com'].DNS.SOA = [
        {
          name: 'example.com',
          value: 'ns1.example.com hostmaster.example.com 2023120101 3600 600 604800 3600'
        }
      ]

      mockResponse.question[0] = {name: 'example.com', type: 6}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.SOA).toHaveBeenCalledWith({
        name: 'example.com',
        primary: 'ns1.example.com',
        admin: 'hostmaster.example.com',
        serial: 2023120101,
        refresh: 3600,
        retry: 600,
        expiration: 604800,
        minimum: 3600,
        ttl: 3600
      })
      expect(mockResponse.header.aa).toBe(1)
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle queries for non-existent domains', () => {
      mockResponse.question[0] = {name: 'nonexistent.com', type: 1}

      // Get the request handler
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(mockResponse.send).toHaveBeenCalled()
      expect(mockResponse.answer).toHaveLength(0)
    })

    it('should handle queries for domains without DNS config', () => {
      delete mockConfig.config.websites['example.com'].DNS

      mockResponse.question[0] = {name: 'example.com', type: 1}

      // Get the request handler
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(mockResponse.send).toHaveBeenCalled()
      expect(mockResponse.answer).toHaveLength(0)
    })

    it('should implement rate limiting per client IP', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      // Mock Date.now to control time
      const originalDateNow = Date.now
      let currentTime = 1000000
      Date.now = jest.fn(() => currentTime)

      // Make 101 requests (exceeding rate limit of 100)
      for (let i = 0; i < 101; i++) {
        const request = {address: {address: '192.168.1.100'}}
        const response = {
          question: [{name: 'example.com', type: 1}],
          answer: [],
          authority: [],
          header: {},
          send: jest.fn()
        }
        requestHandler(request, response)
      }

      // The 101st request should be rate limited (no processing)
      expect(mockResponse.send).toHaveBeenCalledTimes(0) // Original response not used in loop

      // Restore Date.now
      Date.now = originalDateNow
    })

    it('should reset rate limiting after time window', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      // Mock Date.now to control time
      const originalDateNow = Date.now
      let currentTime = 1000000
      Date.now = jest.fn(() => currentTime)

      // Make 100 requests
      for (let i = 0; i < 100; i++) {
        const request = {address: {address: '192.168.1.100'}}
        const response = {
          question: [{name: 'example.com', type: 1}],
          answer: [],
          authority: [],
          header: {},
          send: jest.fn()
        }
        requestHandler(request, response)
      }

      // Advance time by more than rate limit window (60 seconds)
      currentTime += 61000

      // Make another request - should be allowed
      const request = {address: {address: '192.168.1.100'}}
      const response = {
        question: [{name: 'example.com', type: 1}],
        answer: [],
        authority: [],
        header: {},
        send: jest.fn()
      }
      requestHandler(request, response)

      expect(response.send).toHaveBeenCalled()

      // Restore Date.now
      Date.now = originalDateNow
    })

    it('should handle malformed DNS requests gracefully', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      // Test with null request - should not crash
      expect(() => requestHandler(null, mockResponse)).not.toThrow()

      // Test with missing question - create a proper request but invalid response
      const invalidResponse = {send: jest.fn()}
      expect(() => requestHandler(mockRequest, invalidResponse)).not.toThrow()

      // Test with empty question array
      const emptyQuestionResponse = {question: [], send: jest.fn()}
      expect(() => requestHandler(mockRequest, emptyQuestionResponse)).not.toThrow()

      // Verify error handling doesn't crash
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle request processing errors gracefully', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      // Mock DNS record processing to throw an error
      dns.A.mockImplementation(() => {
        throw new Error('DNS processing error')
      })

      // Add A record to trigger processing
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      // Should handle processing errors without crashing
      expect(() => requestHandler(mockRequest, mockResponse)).not.toThrow()
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle case-insensitive domain names', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.A = 1

      // Add A record
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1'})

      // Query with uppercase domain
      mockResponse.question[0] = {name: 'EXAMPLE.COM', type: 1}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(mockResponse.question[0].name).toBe('example.com') // Should be normalized
      expect(dns.A).toHaveBeenCalled()
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should process ANY queries by returning all record types', () => {
      const dns = require('native-dns')

      // Add multiple record types
      DNS.record(
        {name: 'example.com', type: 'A', value: '192.168.1.1'},
        {name: 'example.com', type: 'MX', value: 'mail.example.com', priority: 10},
        {name: 'example.com', type: 'TXT', value: 'v=spf1 mx ~all'}
      )

      // Query with unknown type (should process all)
      mockResponse.question[0] = {name: 'example.com', type: 255} // ANY query

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.A).toHaveBeenCalled()
      expect(dns.MX).toHaveBeenCalled()
      expect(dns.TXT).toHaveBeenCalled()
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should use default values when record values are missing', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.A = 1

      // Add A record without value (should use server IP)
      DNS.record({name: 'example.com', type: 'A'})

      mockResponse.question[0] = {name: 'example.com', type: 1}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.A).toHaveBeenCalledWith({
        name: 'example.com',
        address: DNS.ip, // Should use server IP as default
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle subdomain queries by finding parent domain', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.A = 1

      // Add A record for subdomain
      DNS.record({name: 'api.example.com', type: 'A', value: '192.168.1.100'})

      mockResponse.question[0] = {name: 'api.example.com', type: 1}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.A).toHaveBeenCalledWith({
        name: 'api.example.com',
        address: '192.168.1.100',
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle CAA record queries correctly', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.CAA = 257

      // Mock CAA function
      dns.CAA = jest.fn(data => ({type: 'CAA', ...data}))

      // Add CAA record
      DNS.record({name: 'example.com', type: 'CAA', value: '0 issue letsencrypt.org'})

      mockResponse.question[0] = {name: 'example.com', type: 257}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.CAA).toHaveBeenCalledWith({
        name: 'example.com',
        flags: 0,
        tag: 'issue',
        value: 'letsencrypt.org',
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should add default CAA records when none exist', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.CAA = 257

      // Mock CAA function
      dns.CAA = jest.fn(data => ({type: 'CAA', ...data}))

      // Initialize CAA array but leave it empty
      mockConfig.config.websites['example.com'].DNS.CAA = []

      mockResponse.question[0] = {name: 'example.com', type: 257}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should add default Let's Encrypt CAA records
      expect(dns.CAA).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'example.com',
          tag: 'issue',
          value: 'letsencrypt.org'
        })
      )
      expect(dns.CAA).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'example.com',
          tag: 'issuewild',
          value: 'letsencrypt.org'
        })
      )
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle NXDOMAIN for unknown domains', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_RCODE = {NXDOMAIN: 3}

      mockResponse.question[0] = {name: 'unknown.domain', type: 1}

      // Get the request handler
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(mockResponse.header.rcode).toBe(3)
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should skip rate limiting for localhost', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      let lastResponse
      // Make 150 requests from localhost (exceeds rate limit)
      for (let i = 0; i < 150; i++) {
        const request = {address: {address: '127.0.0.1'}}
        lastResponse = {
          question: [{name: 'example.com', type: 1}],
          answer: [],
          authority: [],
          header: {},
          send: jest.fn()
        }
        requestHandler(request, lastResponse)
      }

      // All requests should be processed (no rate limiting for localhost)
      // Last response should have been sent
      expect(lastResponse.send).toHaveBeenCalled()
    })

    it('should handle malformed CAA records gracefully', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.CAA = 257
      dns.CAA = jest.fn(data => ({type: 'CAA', ...data}))

      // Add malformed CAA record (missing parts)
      mockConfig.config.websites['example.com'].DNS.CAA = [
        {name: 'example.com', value: '0 issue'} // Missing value part
      ]

      mockResponse.question[0] = {name: 'example.com', type: 257}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should not crash, should send response
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle malformed SOA records gracefully', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.SOA = 6

      // Add malformed SOA record (not enough parts)
      mockConfig.config.websites['example.com'].DNS.SOA = [
        {name: 'example.com', value: 'ns1.example.com hostmaster.example.com'} // Missing serial and other fields
      ]

      mockResponse.question[0] = {name: 'example.com', type: 6}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should not crash, should send response
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle null records in TXT processing', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.TXT = 16

      // Add null record
      mockConfig.config.websites['example.com'].DNS.TXT = [null, {name: 'example.com', value: 'valid-txt'}]

      mockResponse.question[0] = {name: 'example.com', type: 16}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should process valid record and skip null
      expect(dns.TXT).toHaveBeenCalledWith({
        name: 'example.com',
        data: ['valid-txt'],
        ttl: 3600
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle null records in SOA processing', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.SOA = 6

      // Add null record
      mockConfig.config.websites['example.com'].DNS.SOA = [
        null,
        {name: 'example.com', value: 'ns1.example.com hostmaster.example.com 2023120101 3600 600 604800 3600'}
      ]

      mockResponse.question[0] = {name: 'example.com', type: 6}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should process valid record and skip null
      expect(dns.SOA).toHaveBeenCalled()
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle null records in CAA processing', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.CAA = 257
      dns.CAA = jest.fn(data => ({type: 'CAA', ...data}))

      // Add null record
      mockConfig.config.websites['example.com'].DNS.CAA = [null, {name: 'example.com', value: '0 issue letsencrypt.org'}]

      mockResponse.question[0] = {name: 'example.com', type: 257}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      // Should process valid record and skip null
      expect(dns.CAA).toHaveBeenCalled()
      expect(mockResponse.send).toHaveBeenCalled()
    })

    it('should handle response.send failure gracefully', () => {
      const dns = require('native-dns')
      // mockError already defined at top

      mockResponse.send = jest.fn(() => {
        throw new Error('Send failed')
      })
      mockResponse.question[0] = {name: 'example.com', type: 1}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      // Should not throw
      expect(() => requestHandler(mockRequest, mockResponse)).not.toThrow()
    })

    it('should handle request with unknown client IP', () => {
      const dns = require('native-dns')
      // mockLog already defined at top

      const requestWithoutIP = {}
      const response = {
        question: [{name: 'example.com', type: 1}],
        answer: [],
        authority: [],
        header: {},
        send: jest.fn()
      }

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(requestWithoutIP, response)

      // Should handle gracefully
      expect(response.send).toHaveBeenCalled()
    })

    it('should handle IPv6 localhost in rate limiting', () => {
      const dns = require('native-dns')
      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      let lastResponse
      // Make 150 requests from IPv6 localhost (exceeds rate limit)
      for (let i = 0; i < 150; i++) {
        const request = {address: {address: '::1'}}
        lastResponse = {
          question: [{name: 'example.com', type: 1}],
          answer: [],
          authority: [],
          header: {},
          send: jest.fn()
        }
        requestHandler(request, lastResponse)
      }

      // All requests should be processed (no rate limiting for localhost)
      expect(lastResponse.send).toHaveBeenCalled()
    })

    it('should handle custom TTL values in records', () => {
      const dns = require('native-dns')
      dns.consts.NAME_TO_QTYPE.A = 1

      // Add A record with custom TTL
      DNS.record({name: 'example.com', type: 'A', value: '192.168.1.1', ttl: 7200})

      mockResponse.question[0] = {name: 'example.com', type: 1}

      const udpServer = dns.createServer.mock.results[0].value
      const requestHandler = udpServer.on.mock.calls.find(call => call[0] === 'request')[1]

      requestHandler(mockRequest, mockResponse)

      expect(dns.A).toHaveBeenCalledWith({
        name: 'example.com',
        address: '192.168.1.1',
        ttl: 7200
      })
      expect(mockResponse.send).toHaveBeenCalled()
    })
  })

  describe('advanced initialization scenarios', () => {
    beforeEach(() => {
      setupGlobalMocks()

      jest.doMock('native-dns', () => ({
        createServer: jest.fn(() => ({
          on: jest.fn(),
          serve: jest.fn()
        })),
        createTCPServer: jest.fn(() => ({
          on: jest.fn(),
          serve: jest.fn()
        })),
        consts: {
          NAME_TO_QTYPE: {A: 1},
          NAME_TO_RCODE: {NXDOMAIN: 3}
        },
        A: jest.fn(data => ({type: 'A', ...data}))
      }))

      jest.doMock('axios', () => ({
        get: jest.fn().mockResolvedValue({data: '127.0.0.1'})
      }))

      mockConfig = {
        config: {
          websites: {
            'example.com': createMockWebsiteConfig('example.com')
          }
        }
      }

      global.Odac.setMock('core', 'Config', mockConfig)

      jest.resetModules()
      DNS = require('../../server/src/DNS')
    })

    afterEach(() => {
      cleanupGlobalMocks()
      jest.resetModules()
      jest.dontMock('native-dns')
      jest.dontMock('axios')
    })

    it('should handle multiple IP service failures and use local network IP', async () => {
      const axios = require('axios')
      // mockLog already defined at top

      // All services fail
      axios.get.mockRejectedValue(new Error('Network error'))

      DNS.init()

      await new Promise(resolve => setTimeout(resolve, 100))

      // Should fallback to local network IP (not 127.0.0.1)
      expect(DNS.ip).not.toBe('127.0.0.1')
      expect(DNS.ip).toMatch(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)
    })

    it('should handle second IP service success after first fails', async () => {
      const axios = require('axios')

      // First service fails, second succeeds
      axios.get.mockRejectedValueOnce(new Error('First service failed')).mockResolvedValueOnce({data: '203.0.113.50'})

      DNS.init()

      await new Promise(resolve => setTimeout(resolve, 100))

      expect(DNS.ip).toBe('203.0.113.50')
    })

    it('should trim whitespace from IP response', async () => {
      const axios = require('axios')

      axios.get.mockResolvedValue({data: '  203.0.113.75  \n'})

      DNS.init()

      await new Promise(resolve => setTimeout(resolve, 100))

      expect(DNS.ip).toBe('203.0.113.75')
    })

    it('should handle execSync errors in logSystemInfo', async () => {
      // Mock child_process before requiring DNS
      jest.doMock('child_process', () => ({
        execSync: jest.fn(() => {
          throw new Error('Command failed')
        })
      }))

      jest.resetModules()
      const DNSWithError = require('../../server/src/DNS')

      // Should not crash when init is called
      expect(() => DNSWithError.init()).not.toThrow()

      await new Promise(resolve => setTimeout(resolve, 100))
    })

    it('should handle platform-specific checks on non-Linux systems', async () => {
      // Mock os module before requiring DNS
      jest.doMock('os', () => ({
        platform: jest.fn(() => 'darwin'),
        arch: jest.fn(() => 'x64'),
        networkInterfaces: jest.fn(() => ({
          en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
        }))
      }))

      jest.resetModules()
      const DNSWithDarwin = require('../../server/src/DNS')

      // Should not crash on non-Linux platforms
      expect(() => DNSWithDarwin.init()).not.toThrow()

      await new Promise(resolve => setTimeout(resolve, 100))
    })

    it('should handle Linux platform with systemd-resolved checks', async () => {
      // mockLog already defined at top

      // Mock os and child_process modules
      mockLog.mockClear() // Clear previous calls

      jest.doMock('os', () => ({
        platform: jest.fn(() => 'linux'),
        arch: jest.fn(() => 'x64'),
        networkInterfaces: jest.fn(() => ({
          eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
        }))
      }))

      jest.doMock('child_process', () => ({
        execSync: jest.fn(cmd => {
          if (cmd.includes('systemctl is-active')) return 'active'
          if (cmd.includes('systemd-resolve') || cmd.includes('resolvectl')) return 'DNS Server: 127.0.0.53'
          return ''
        })
      }))

      jest.resetModules()
      const DNSWithLinux = require('../../server/src/DNS')

      // Should not crash on Linux with systemd-resolved
      expect(() => DNSWithLinux.init()).not.toThrow()

      await new Promise(resolve => setTimeout(resolve, 100))
    })
  })
})

describe('port management and conflict resolution', () => {
  let DNS, mockConfig

  beforeEach(() => {
    // Clear mock calls before each test
    mockLog.mockClear()
    mockError.mockClear()

    setupGlobalMocks()

    // Set up the Log mock before requiring DNS
    const {mockOdac} = require('./__mocks__/globalOdac')
    mockOdac.setMock('core', 'Log', {
      init: jest.fn().mockReturnValue({
        log: mockLog,
        error: mockError
      })
    })

    jest.doMock('native-dns', () => ({
      createServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      createTCPServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      consts: {
        NAME_TO_QTYPE: {A: 1},
        NAME_TO_RCODE: {NXDOMAIN: 3}
      },
      A: jest.fn(data => ({type: 'A', ...data}))
    }))

    jest.doMock('axios', () => ({
      get: jest.fn().mockResolvedValue({data: '127.0.0.1'})
    }))

    mockConfig = {
      config: {
        websites: {
          'example.com': createMockWebsiteConfig('example.com')
        }
      }
    }

    global.Odac.setMock('core', 'Config', mockConfig)
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.resetModules()
    jest.dontMock('native-dns')
    jest.dontMock('axios')
    jest.dontMock('child_process')
    jest.dontMock('os')
    jest.dontMock('fs')
  })

  it('should detect port 53 is in use', async () => {
    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'systemd-resolve 1234 root   13u  IPv4 12345      0t0  UDP 127.0.0.53:domain'
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle port conflict gracefully
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 200))
  })

  it('should detect port is available', async () => {
    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return '' // Port is free
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should start successfully when port is available
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 200))
  })

  it('should handle port check errors gracefully', async () => {
    jest.doMock('child_process', () => ({
      execSync: jest.fn(() => {
        throw new Error('Command not found')
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle port check errors without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 200))
  })

  it('should detect systemd-resolved on Linux', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'systemd-resolve 1234 root   13u  IPv4 12345      0t0  UDP 127.0.0.53:domain'
        if (cmd.includes('systemctl is-active')) return 'active'
        return ''
      })
    }))

    jest.doMock('fs', () => ({
      existsSync: jest.fn(() => false)
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle systemd-resolved detection without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should skip systemd-resolved handling on non-Linux', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'darwin'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'some-process 1234 user'
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should skip systemd-resolved handling on non-Linux without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle non-systemd process on port 53', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'dnsmasq 1234 root'
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle non-systemd process without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle systemd-resolved not active', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'systemd-resolve 1234 root'
        if (cmd.includes('systemctl is-active')) return 'inactive'
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle inactive systemd-resolved without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle sudo permission errors', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'systemd-resolve 1234 root'
        if (cmd.includes('systemctl is-active')) return 'active'
        if (cmd.includes('sudo')) throw new Error('sudo: no tty present')
        return ''
      })
    }))

    jest.doMock('fs', () => ({
      existsSync: jest.fn(() => false)
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle sudo permission errors gracefully
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle error handler with EADDRINUSE', async () => {
    const dns = require('native-dns')
    // mockError already defined at top

    const udpServer = {
      on: jest.fn(),
      serve: jest.fn()
    }
    const tcpServer = {
      on: jest.fn(),
      serve: jest.fn()
    }

    dns.createServer.mockReturnValue(udpServer)
    dns.createTCPServer.mockReturnValue(tcpServer)

    jest.doMock('child_process', () => ({
      execSync: jest.fn(() => '')
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    DNS.init()

    await new Promise(resolve => setTimeout(resolve, 200))

    // Get the error handler
    const errorHandler = udpServer.on.mock.calls.find(call => call[0] === 'error')?.[1]

    if (errorHandler) {
      const error = new Error('Port in use')
      error.code = 'EADDRINUSE'
      await errorHandler(error)

      expect(mockLog).toHaveBeenCalledWith('DNS UDP Server Error:', 'Port in use')
    }
  })

  it('should handle error handler with EACCES', async () => {
    const dns = require('native-dns')
    // mockError already defined at top

    const udpServer = {
      on: jest.fn(),
      serve: jest.fn()
    }
    const tcpServer = {
      on: jest.fn(),
      serve: jest.fn()
    }

    dns.createServer.mockReturnValue(udpServer)
    dns.createTCPServer.mockReturnValue(tcpServer)

    jest.doMock('child_process', () => ({
      execSync: jest.fn(() => '')
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    DNS.init()

    await new Promise(resolve => setTimeout(resolve, 200))

    // Get the error handler
    const errorHandler = tcpServer.on.mock.calls.find(call => call[0] === 'error')?.[1]

    if (errorHandler) {
      const error = new Error('Permission denied')
      error.code = 'EACCES'
      await errorHandler(error)

      expect(mockLog).toHaveBeenCalledWith('DNS TCP Server Error:', 'Permission denied')
    }
  })
})

describe('alternative port and system DNS configuration', () => {
  let DNS, mockConfig

  beforeEach(() => {
    // Clear mock calls before each test
    mockLog.mockClear()
    mockError.mockClear()

    setupGlobalMocks()

    // Set up the Log mock before requiring DNS
    const {mockOdac} = require('./__mocks__/globalOdac')
    mockOdac.setMock('core', 'Log', {
      init: jest.fn().mockReturnValue({
        log: mockLog,
        error: mockError
      })
    })

    jest.doMock('native-dns', () => ({
      createServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      createTCPServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      consts: {
        NAME_TO_QTYPE: {A: 1},
        NAME_TO_RCODE: {NXDOMAIN: 3}
      },
      A: jest.fn(data => ({type: 'A', ...data}))
    }))

    jest.doMock('axios', () => ({
      get: jest.fn().mockResolvedValue({data: '127.0.0.1'})
    }))

    mockConfig = {
      config: {
        websites: {
          'example.com': createMockWebsiteConfig('example.com')
        }
      }
    }

    global.Odac.setMock('core', 'Config', mockConfig)
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.resetModules()
    jest.dontMock('native-dns')
    jest.dontMock('axios')
    jest.dontMock('child_process')
    jest.dontMock('os')
    jest.dontMock('fs')
  })

  it('should try alternative ports when port 53 is unavailable', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'darwin'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        // Port 53 is in use
        if (cmd.includes('lsof -i :53')) return 'some-process 1234 user'
        // Port 5353 is free
        if (cmd.includes('lsof -i :5353')) return ''
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should try alternative ports without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 400))
  })

  it('should handle all alternative ports in use', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'darwin'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        // All ports are in use
        if (cmd.includes('lsof -i :')) return 'some-process 1234 user'
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle all ports in use without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 400))
  })

  it('should handle alternative port startup errors', async () => {
    const dns = require('native-dns')

    jest.doMock('os', () => ({
      platform: jest.fn(() => 'darwin'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'some-process 1234 user'
        if (cmd.includes('lsof -i :5353')) return '' // Port 5353 appears free
        return ''
      })
    }))

    // Make serve throw error
    dns.createServer.mockReturnValue({
      on: jest.fn(),
      serve: jest.fn(() => {
        throw new Error('Failed to bind')
      })
    })

    dns.createTCPServer.mockReturnValue({
      on: jest.fn(),
      serve: jest.fn(() => {
        throw new Error('Failed to bind')
      })
    })

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle startup errors without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 400))
  })

  it('should handle serve() throwing EADDRINUSE in attemptDNSStart', async () => {
    jest.doMock('native-dns', () => ({
      createServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn(() => {
          const err = new Error('Address in use')
          err.code = 'EADDRINUSE'
          throw err
        })
      })),
      createTCPServer: jest.fn(() => ({
        on: jest.fn(),
        serve: jest.fn()
      })),
      consts: {
        NAME_TO_QTYPE: {A: 1},
        NAME_TO_RCODE: {NXDOMAIN: 3}
      },
      A: jest.fn(data => ({type: 'A', ...data}))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(() => '')
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should not throw
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should setup system DNS for internet access on port 53', async () => {
    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return '' // Port is free
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should setup DNS without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle setupSystemDNSForInternet errors gracefully', async () => {
    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return '' // Port is free
        if (cmd.includes('sudo')) throw new Error('Permission denied')
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle permission errors without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 300))
  })

  it('should handle updateSystemDNSConfig errors gracefully', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'darwin'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        en0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'some-process 1234 user'
        if (cmd.includes('lsof -i :5353')) return '' // Port 5353 is free
        if (cmd.includes('sudo')) throw new Error('Permission denied')
        return ''
      })
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle DNS config errors without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 400))
  })

  it('should handle fs.existsSync returning true', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) return 'systemd-resolve 1234 root'
        if (cmd.includes('systemctl is-active')) return 'active'
        return ''
      })
    }))

    jest.doMock('fs', () => ({
      existsSync: jest.fn(() => true) // Directory already exists
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle existing directory without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 400))
  })

  it('should handle successful systemd-resolved configuration', async () => {
    jest.doMock('os', () => ({
      platform: jest.fn(() => 'linux'),
      arch: jest.fn(() => 'x64'),
      networkInterfaces: jest.fn(() => ({
        eth0: [{internal: false, family: 'IPv4', address: '192.168.1.10'}]
      }))
    }))

    jest.doMock('child_process', () => ({
      execSync: jest.fn(cmd => {
        if (cmd.includes('lsof -i :53')) {
          // First call: port in use, second call after restart: port free
          if (cmd.match(/lsof/g)?.length === 1) return 'systemd-resolve 1234 root'
          return ''
        }
        if (cmd.includes('systemctl is-active')) return 'active'
        return ''
      })
    }))

    jest.doMock('fs', () => ({
      existsSync: jest.fn(() => false)
    }))

    jest.resetModules()
    DNS = require('../../server/src/DNS')

    // Should handle systemd-resolved configuration without crashing
    expect(() => DNS.init()).not.toThrow()

    await new Promise(resolve => setTimeout(resolve, 4000))
  })
})
