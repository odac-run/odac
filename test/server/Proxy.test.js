/**
 * Unit tests for Proxy.js module
 * Tests web hosting, proxy functionality, and website management
 */

// Mock all required modules before importing Web
jest.mock('child_process')
jest.mock('fs')
jest.mock('http')
jest.mock('https')
jest.mock('net')
jest.mock('os')
jest.mock('path')
jest.mock('tls')

const childProcess = require('child_process')
const fs = require('fs')
const http = require('http')
const https = require('https')
const net = require('net')
const os = require('os')
const path = require('path')
const tls = require('tls')

// Import test utilities
const {mockOdac} = require('./__mocks__/globalOdac')
const {createMockRequest, createMockResponse} = require('./__mocks__/testFactories')
const {createMockWebsiteConfig} = require('./__mocks__/testFactories')

describe('Proxy', () => {
  let ProxyService
  let mockConfig
  let mockLog
  let mockHttpServer
  let mockHttpsServer

  beforeEach(() => {
    // Reset all mocks
    jest.clearAllMocks()

    // Setup global Odac mock
    mockOdac.resetMocks()
    mockConfig = mockOdac.core('Config')

    // Initialize config structure
    mockConfig.config = {
      websites: {},
      web: {path: '/var/odac'},
      ssl: null
    }

    // Setup Log mock
    const mockLogInstance = {
      log: jest.fn(),
      error: jest.fn(),
      info: jest.fn(),
      debug: jest.fn()
    }
    mockOdac.setMock('server', 'Log', {
      init: jest.fn().mockReturnValue(mockLogInstance)
    })
    mockLog = mockLogInstance.log

    // Setup Api mock
    mockOdac.setMock('server', 'Api', {
      addToken: jest.fn(),
      removeToken: jest.fn(),
      generateToken: jest.fn(() => 'mock-token'),
      result: jest.fn((success, message) => ({success, message}))
    })

    // Setup DNS mock with default methods
    mockOdac.setMock('server', 'DNS', {
      record: jest.fn(),
      ip: '127.0.0.1'
    })

    // Setup Process mock
    mockOdac.setMock('core', 'Process', {
      stop: jest.fn()
    })

    global.Odac = mockOdac
    global.__ = jest.fn((key, ...args) => {
      // Simple mock translation function
      let result = key
      args.forEach((arg, index) => {
        result = result.replace(`%s${index + 1}`, arg).replace('%s', arg)
      })
      return result
    })

    // Setup mock servers
    mockHttpServer = {
      listen: jest.fn(),
      on: jest.fn(),
      close: jest.fn()
    }

    mockHttpsServer = {
      listen: jest.fn(),
      on: jest.fn(),
      close: jest.fn()
    }

    // Setup module mocks
    http.createServer.mockReturnValue(mockHttpServer)
    https.createServer.mockReturnValue(mockHttpsServer)

    // Setup http.request mock for proxy tests
    const mockProxyReq = {
      on: jest.fn(),
      pipe: jest.fn(),
      destroy: jest.fn()
    }
    http.request.mockReturnValue(mockProxyReq)

    // Setup file system mocks
    fs.existsSync.mockReturnValue(true)
    fs.mkdirSync.mockImplementation(() => {})
    fs.cpSync.mockImplementation(() => {})
    fs.rmSync.mockImplementation(() => {})
    fs.readFileSync.mockReturnValue('mock-file-content')
    fs.writeFile.mockImplementation((path, data, callback) => {
      if (callback) callback(null)
    })

    // Setup OS mocks
    os.homedir.mockReturnValue('/home/user')
    os.platform.mockReturnValue('linux')

    // Setup path mocks
    path.join.mockImplementation((...args) => args.join('/'))

    // Setup child process mocks
    const mockChild = {
      pid: 12345,
      stdout: {on: jest.fn()},
      stderr: {on: jest.fn()},
      on: jest.fn()
    }
    childProcess.spawn.mockReturnValue(mockChild)
    childProcess.execSync.mockImplementation(() => {})

    // Setup net mocks for port checking
    const mockNetServer = {
      once: jest.fn(),
      listen: jest.fn(),
      close: jest.fn()
    }
    net.createServer.mockReturnValue(mockNetServer)

    // Setup TLS mocks
    const mockSecureContext = {context: 'mock-context'}
    tls.createSecureContext.mockReturnValue(mockSecureContext)

    // Import Proxy after mocks are set up
    ProxyService = require('../../server/src/Proxy')
  })

  afterEach(() => {
    delete global.Odac
    delete global.__
  })

  describe('initialization', () => {
    test('should initialize with default configuration', async () => {
      await ProxyService.init()

      // ProxyService.server is no longer exposed as the architecture changed to use Go Proxy
      // We verify initialization by ensuring no error is thrown and config is set
      expect(mockConfig.config.web).toBeDefined()
    })

    test('should set default web path based on platform', async () => {
      // Test Linux/Unix platform
      os.platform.mockReturnValue('linux')
      mockConfig.config.web = undefined

      await ProxyService.init()

      expect(mockConfig.config.web.path).toBe('/var/odac/')

      // Test macOS platform
      os.platform.mockReturnValue('darwin')
      mockConfig.config.web = undefined

      await ProxyService.init()

      expect(mockConfig.config.web.path).toBe('/home/user/Odac/')

      // Test Windows platform
      os.platform.mockReturnValue('win32')
      mockConfig.config.web = undefined

      await ProxyService.init()

      expect(mockConfig.config.web.path).toBe('/home/user/Odac/')
    })

    test('should create web directory if it does not exist', async () => {
      fs.existsSync.mockReturnValue(false)
      mockConfig.config.web = {path: '/custom/path'}

      await ProxyService.init()

      expect(fs.existsSync).toHaveBeenCalledWith('/custom/path')
    })
  })

  describe.skip('server creation', () => {
    beforeEach(async () => {
      await ProxyService.init()
      mockConfig.config.websites = {'example.com': createMockWebsiteConfig()}
    })

    test('should create HTTP server on port 80', () => {
      ProxyService.server()

      expect(http.createServer).toHaveBeenCalledWith(expect.any(Function))
      expect(mockHttpServer.listen).toHaveBeenCalledWith(80)
      expect(mockHttpServer.on).toHaveBeenCalledWith('error', expect.any(Function))
    })

    test('should handle HTTP server errors', () => {
      // Create a fresh mock server for this test
      const freshMockHttpServer = {
        listen: jest.fn(),
        on: jest.fn(),
        close: jest.fn()
      }
      http.createServer.mockReturnValue(freshMockHttpServer)

      // Reset the ProxyService module's server instances to force recreation
      ProxyService['_OdacProxy__server_http'] = null
      ProxyService['_OdacProxy__server_https'] = null
      ProxyService['_OdacProxy__loaded'] = true // Ensure ProxyService module is marked as loaded

      // Ensure we have websites configured (required for server creation)
      mockConfig.config.websites = {'example.com': createMockWebsiteConfig()}

      ProxyService.server()

      // Verify HTTP server was created
      expect(http.createServer).toHaveBeenCalled()

      // Verify the error handler was attached
      expect(freshMockHttpServer.on).toHaveBeenCalledWith('error', expect.any(Function))

      // Get the error handler function
      const errorCall = freshMockHttpServer.on.mock.calls.find(call => call[0] === 'error')
      const errorHandler = errorCall[1]

      const mockError = new Error('EADDRINUSE')
      mockError.code = 'EADDRINUSE'

      expect(() => errorHandler(mockError)).not.toThrow()
      expect(mockLog).toHaveBeenCalledWith('HTTP server error: EADDRINUSE')
      expect(mockLog).toHaveBeenCalledWith('Port 80 is already in use')
    })

    test('should create HTTPS server on port 443 with SSL configuration', () => {
      mockConfig.config.ssl = {
        key: '/path/to/key.pem',
        cert: '/path/to/cert.pem'
      }

      ProxyService.server()

      expect(https.createServer).toHaveBeenCalledWith(
        expect.objectContaining({
          SNICallback: expect.any(Function),
          key: 'mock-file-content',
          cert: 'mock-file-content'
        }),
        expect.any(Function)
      )
      expect(mockHttpsServer.listen).toHaveBeenCalledWith(443)
      expect(mockHttpsServer.on).toHaveBeenCalledWith('error', expect.any(Function))
    })

    test('should handle HTTPS server errors', () => {
      mockConfig.config.ssl = {
        key: '/path/to/key.pem',
        cert: '/path/to/cert.pem'
      }

      // Create a fresh mock server for this test
      const freshMockHttpsServer = {
        listen: jest.fn(),
        on: jest.fn(),
        close: jest.fn()
      }
      https.createServer.mockReturnValue(freshMockHttpsServer)

      // Reset the ProxyService module's server instances to force recreation
      ProxyService['_OdacProxy__server_http'] = null
      ProxyService['_OdacProxy__server_https'] = null
      ProxyService['_OdacProxy__loaded'] = true // Ensure ProxyService module is marked as loaded

      // Ensure we have websites configured (required for server creation)
      mockConfig.config.websites = {'example.com': createMockWebsiteConfig()}

      ProxyService.server()

      // Verify HTTPS server was created
      expect(https.createServer).toHaveBeenCalled()

      // Verify the error handler was attached
      expect(freshMockHttpsServer.on).toHaveBeenCalledWith('error', expect.any(Function))

      // Get the error handler function
      const errorCall = freshMockHttpsServer.on.mock.calls.find(call => call[0] === 'error')
      const errorHandler = errorCall[1]

      const mockError = new Error('EADDRINUSE')
      mockError.code = 'EADDRINUSE'

      expect(() => errorHandler(mockError)).not.toThrow()
      expect(mockLog).toHaveBeenCalledWith('HTTPS server error: EADDRINUSE')
      expect(mockLog).toHaveBeenCalledWith('Port 443 is already in use')
    })

    test('should not create HTTPS server without SSL configuration', () => {
      mockConfig.config.ssl = undefined

      ProxyService.server()

      expect(https.createServer).not.toHaveBeenCalled()
    })

    test('should not create HTTPS server with missing SSL files', () => {
      mockConfig.config.ssl = {
        key: '/path/to/key.pem',
        cert: '/path/to/cert.pem'
      }
      fs.existsSync.mockImplementation(path => !path.includes('key.pem') && !path.includes('cert.pem'))

      ProxyService.server()

      expect(https.createServer).not.toHaveBeenCalled()
    })
  })

  describe('website creation', () => {
    beforeEach(async () => {
      await ProxyService.init()
      mockConfig.config.web = {path: '/var/odac'}
    })

    test('should create website with valid domain', async () => {
      const mockProgress = jest.fn()
      const domain = 'example.com'

      const result = await ProxyService.create(domain, mockProgress)

      expect(result.success).toBe(true)
      expect(result.message).toContain('Website example.com created')
      expect(mockProgress).toHaveBeenCalledWith('domain', 'progress', expect.stringContaining('Setting up domain'))
      expect(mockProgress).toHaveBeenCalledWith('domain', 'success', expect.stringContaining('Domain example.com set'))
    })

    test('should reject invalid domain names', async () => {
      const mockProgress = jest.fn()

      // Test short domain
      let result = await ProxyService.create('ab', mockProgress)
      expect(result.success).toBe(false)
      expect(result.message).toBe('Invalid domain.')

      // Test domain without dot (except localhost)
      result = await ProxyService.create('invalid', mockProgress)
      expect(result.success).toBe(false)
      expect(result.message).toBe('Invalid domain.')
    })

    test('should allow localhost as valid domain', async () => {
      const mockProgress = jest.fn()

      const result = await ProxyService.create('localhost', mockProgress)

      expect(result.success).toBe(true)
      expect(result.message).toContain('Website localhost created')
    })

    test('should strip protocol prefixes from domain', async () => {
      const mockProgress = jest.fn()

      await ProxyService.create('https://example.com', mockProgress)

      expect(mockConfig.config.websites['example.com']).toBeDefined()
      expect(mockConfig.config.websites['https://example.com']).toBeUndefined()
    })

    test('should reject existing domain', async () => {
      const mockProgress = jest.fn()
      mockConfig.config.websites = {'example.com': {}}

      const result = await ProxyService.create('example.com', mockProgress)

      expect(result.success).toBe(false)
      expect(result.message).toBe('Website example.com already exists.')
    })

    test('should create website directory and initialize project', async () => {
      const mockProgress = jest.fn()
      const domain = 'example.com'

      // Mock fs.existsSync to return false for the website directory so it gets created
      fs.existsSync.mockImplementation(path => {
        if (path === '/var/odac/example.com') return false
        return true
      })

      await ProxyService.create(domain, mockProgress)

      expect(fs.mkdirSync).toHaveBeenCalledWith('/var/odac/example.com', {recursive: true})

      // Verify initialization commands
      expect(childProcess.execSync).toHaveBeenCalledWith('npm init -y', {cwd: '/var/odac/example.com'})
      expect(childProcess.execSync).toHaveBeenCalledWith('npm i odac', {cwd: '/var/odac/example.com'})
      expect(childProcess.execSync).toHaveBeenCalledWith(expect.stringContaining('odac init'), {
        cwd: '/var/odac/example.com'
      })
    })

    test('should setup DNS records for non-localhost domains', async () => {
      const mockProgress = jest.fn()
      const domain = 'example.com'
      const mockDNS = {
        record: jest.fn(),
        ip: '192.168.1.1',
        ips: {ipv4: [], ipv6: []}
      }
      mockOdac.setMock('server', 'DNS', mockDNS)
      mockOdac.setMock('server', 'Api', {
        addToken: jest.fn(),
        removeToken: jest.fn(),
        generateToken: jest.fn(() => 'mock-token'),
        result: jest.fn((success, message) => ({success, message}))
      })

      await ProxyService.create(domain, mockProgress)

      // Verify DNS.record was called with spread arguments
      expect(mockDNS.record).toHaveBeenCalled()
      const recordArgs = mockDNS.record.mock.calls[0]

      // Check A record (no value - dynamic resolution)
      expect(recordArgs).toContainEqual({name: 'example.com', type: 'A'})
      // Check AAAA record (no value - dynamic resolution)
      expect(recordArgs).toContainEqual({name: 'example.com', type: 'AAAA'})
      // Check CNAME record
      expect(recordArgs).toContainEqual({name: 'www.example.com', type: 'CNAME', value: 'example.com'})
      // Check MX record
      expect(recordArgs).toContainEqual({name: 'example.com', type: 'MX', value: 'example.com'})
      // Check SPF TXT record
      expect(recordArgs).toContainEqual({name: 'example.com', type: 'TXT', value: 'v=spf1 a mx ip4:192.168.1.1 ~all'})
      // Check DMARC record
      expect(recordArgs).toContainEqual({
        name: '_dmarc.example.com',
        type: 'TXT',
        value: 'v=DMARC1; p=reject; rua=mailto:postmaster@example.com'
      })

      expect(mockProgress).toHaveBeenCalledWith('dns', 'progress', expect.stringContaining('Setting up DNS records'))
      expect(mockProgress).toHaveBeenCalledWith('dns', 'success', expect.stringContaining('DNS records for example.com set'))
    })

    test('should not setup DNS records for localhost', async () => {
      const mockProgress = jest.fn()
      const mockDNS = {record: jest.fn()}
      mockOdac.setMock('server', 'DNS', mockDNS)
      mockOdac.setMock('server', 'Api', {
        addToken: jest.fn(),
        removeToken: jest.fn(),
        generateToken: jest.fn(() => 'mock-token'),
        result: jest.fn((success, message) => ({success, message}))
      })

      await ProxyService.create('localhost', mockProgress)

      expect(mockDNS.record).not.toHaveBeenCalled()
    })

    test('should not setup DNS records for IP addresses', async () => {
      const mockProgress = jest.fn()
      const mockDNS = {record: jest.fn()}
      mockOdac.setMock('server', 'DNS', mockDNS)
      mockOdac.setMock('server', 'Api', {
        addToken: jest.fn(),
        removeToken: jest.fn(),
        generateToken: jest.fn(() => 'mock-token'),
        result: jest.fn((success, message) => ({success, message}))
      })

      await ProxyService.create('192.168.1.1', mockProgress)

      expect(mockDNS.record).not.toHaveBeenCalled()
    })
  })

  describe.skip('request handling and proxy functionality', () => {
    let mockReq, mockRes

    beforeEach(async () => {
      await ProxyService.init()
      mockReq = createMockRequest()
      mockRes = createMockResponse()

      // Setup a test website
      mockConfig.config.websites = {
        'example.com': {
          domain: 'example.com',
          path: '/var/odac/example.com',
          pid: 12345,
          port: 3000,
          cert: {
            ssl: {
              key: '/path/to/example.key',
              cert: '/path/to/example.cert'
            }
          }
        }
      }

      // Mock watcher to indicate process is running
      ProxyService['_OdacProxy__watcher'] = {12345: true}
    })

    test('should redirect HTTP requests to HTTPS', () => {
      mockReq.headers.host = 'example.com'
      mockReq.url = '/test-path'

      ProxyService.request(mockReq, mockRes, false)

      expect(mockRes.writeHead).toHaveBeenCalled()
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should serve default index for requests without host header', () => {
      mockReq.headers = {}

      ProxyService.request(mockReq, mockRes, true)

      expect(mockRes.write).toHaveBeenCalledWith('Odac Server')
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should serve default index for unknown hosts', () => {
      mockReq.headers.host = 'unknown.com'

      ProxyService.request(mockReq, mockRes, true)

      expect(mockRes.write).toHaveBeenCalledWith('Odac Server')
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should resolve subdomain to parent domain', () => {
      mockReq.headers.host = 'www.example.com'
      mockReq.url = '/test'

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          hostname: '127.0.0.1',
          port: 3000,
          path: '/test',
          method: 'GET'
        }),
        expect.any(Function)
      )
    })

    test('should proxy HTTPS requests to website process', () => {
      mockReq.headers.host = 'example.com'
      mockReq.url = '/api/test'

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          hostname: '127.0.0.1',
          port: 3000,
          path: '/api/test',
          method: 'GET'
        }),
        expect.any(Function)
      )
    })

    test('should serve default index when website process is not running', () => {
      mockConfig.config.websites['example.com'].pid = null
      mockReq.headers.host = 'example.com'

      ProxyService.request(mockReq, mockRes, true)

      expect(mockRes.write).toHaveBeenCalledWith('Odac Server')
      expect(mockRes.end).toHaveBeenCalled()
      expect(http.request).not.toHaveBeenCalled()
    })

    test('should serve default index when watcher indicates process is not running', () => {
      ProxyService['_OdacProxy__watcher'] = {12345: false}
      mockReq.headers.host = 'example.com'

      ProxyService.request(mockReq, mockRes, true)

      expect(mockRes.write).toHaveBeenCalledWith('Odac Server')
      expect(mockRes.end).toHaveBeenCalled()
      expect(http.request).not.toHaveBeenCalled()
    })

    test('should add custom headers to proxied requests', () => {
      mockReq.headers.host = 'example.com'
      mockReq.socket = {remoteAddress: '192.168.1.100'}

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          headers: expect.objectContaining({
            'x-odac-connection-remoteaddress': '192.168.1.100',
            'x-odac-connection-ssl': 'true'
          })
        }),
        expect.any(Function)
      )
    })

    test('should handle exceptions in request processing', () => {
      mockReq.headers.host = 'example.com'
      http.request.mockImplementation(() => {
        throw new Error('Request creation failed')
      })

      ProxyService.request(mockReq, mockRes, true)

      expect(mockLog).toHaveBeenCalledWith(expect.any(Error))
      expect(mockRes.write).toHaveBeenCalledWith('Odac Server')
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should handle HTTP requests with query parameters in redirection', () => {
      mockReq.headers.host = 'example.com'
      mockReq.url = '/test-path?param=value&other=123'

      ProxyService.request(mockReq, mockRes, false)

      expect(mockRes.writeHead).toHaveBeenCalledWith(301, {
        Location: 'https://example.com/test-path?param=value&other=123'
      })
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should handle HTTP requests with fragments in redirection', () => {
      mockReq.headers.host = 'example.com'
      mockReq.url = '/test-path#section'

      ProxyService.request(mockReq, mockRes, false)

      expect(mockRes.writeHead).toHaveBeenCalledWith(301, {
        Location: 'https://example.com/test-path#section'
      })
      expect(mockRes.end).toHaveBeenCalled()
    })

    test('should handle multi-level subdomain resolution', () => {
      // Setup a multi-level subdomain scenario
      mockConfig.config.websites = {
        'example.com': {
          domain: 'example.com',
          path: '/var/odac/example.com',
          pid: 12345,
          port: 3000
        }
      }
      ProxyService['_OdacProxy__watcher'] = {12345: true}

      mockReq.headers.host = 'api.staging.example.com'
      mockReq.url = '/test'

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          hostname: '127.0.0.1',
          port: 3000
        }),
        expect.any(Function)
      )
    })

    test('should handle requests with port numbers in host header', () => {
      mockReq.headers.host = 'example.com:8080'
      mockReq.url = '/test'

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          hostname: '127.0.0.1',
          port: 3000
        }),
        expect.any(Function)
      )
    })

    test('should set correct SSL header for HTTPS requests', () => {
      mockReq.headers.host = 'example.com'
      mockReq.socket = {remoteAddress: '192.168.1.100'}

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          headers: expect.objectContaining({
            'x-odac-connection-ssl': 'true'
          })
        }),
        expect.any(Function)
      )
    })

    test('should handle missing remote address in proxy headers', () => {
      mockReq.headers.host = 'example.com'
      mockReq.socket = {}

      ProxyService.request(mockReq, mockRes, true)

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          headers: expect.objectContaining({
            'x-odac-connection-remoteaddress': '',
            'x-odac-connection-ssl': 'true'
          })
        }),
        expect.any(Function)
      )
    })
  })

  describe.skip('process management and monitoring', () => {
    let mockChild

    beforeEach(async () => {
      await ProxyService.init()
      mockConfig.config.web = {path: '/var/odac'}

      // Setup mock child process
      mockChild = {
        pid: 12345,
        stdout: {on: jest.fn()},
        stderr: {on: jest.fn()},
        on: jest.fn()
      }
      childProcess.spawn.mockReturnValue(mockChild)

      // Initialize ProxyService module's private properties
      ProxyService['_OdacProxy__active'] = {}
      ProxyService['_OdacProxy__error_counts'] = {}
      ProxyService['_OdacProxy__logs'] = {log: {}, err: {}}
      ProxyService['_OdacProxy__ports'] = {}
      ProxyService['_OdacProxy__started'] = {}
      ProxyService['_OdacProxy__watcher'] = {}
    })

    test('should test port checking functionality', async () => {
      const mockNetServer = {
        once: jest.fn((event, callback) => {
          if (event === 'listening') {
            setTimeout(() => callback(), 0)
          }
        }),
        listen: jest.fn(),
        close: jest.fn()
      }
      net.createServer.mockReturnValue(mockNetServer)

      const result = await ProxyService.checkPort(3000)

      expect(result).toBe(true)
      expect(mockNetServer.listen).toHaveBeenCalledWith(3000, '127.0.0.1')
      expect(mockNetServer.close).toHaveBeenCalled()
    })

    test('should detect port conflicts', async () => {
      const mockNetServer = {
        once: jest.fn((event, callback) => {
          if (event === 'error') {
            setTimeout(() => callback(), 0)
          }
        }),
        listen: jest.fn(),
        close: jest.fn()
      }
      net.createServer.mockReturnValue(mockNetServer)

      const result = await ProxyService.checkPort(3000)

      expect(result).toBe(false)
    })

    test('should not start process if already active', async () => {
      const domain = 'example.com'
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com'
        }
      }

      // Mark domain as active
      ProxyService['_OdacProxy__active'][domain] = true

      await ProxyService.start(domain)

      expect(childProcess.spawn).not.toHaveBeenCalled()
    })

    test('should not start process if website does not exist', async () => {
      await ProxyService.start('nonexistent.com')

      expect(childProcess.spawn).not.toHaveBeenCalled()
    })

    test('should respect error cooldown period', async () => {
      const domain = 'example.com'
      const now = Date.now()
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          status: 'errored',
          updated: now - 500 // 500ms ago
        }
      }

      // Set error count to 2 (should wait 2 seconds)
      ProxyService['_OdacProxy__error_counts'][domain] = 2

      await ProxyService.start(domain)

      expect(childProcess.spawn).not.toHaveBeenCalled()
    })

    test('should not start process without index.js file', async () => {
      const domain = 'example.com'
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com'
        }
      }

      // Mock index.js file as missing
      fs.existsSync.mockImplementation(path => !path.includes('index.js'))

      await ProxyService.start(domain)

      expect(childProcess.spawn).not.toHaveBeenCalled()
      expect(mockLog).toHaveBeenCalledWith("Website example.com doesn't have index.js file.")
    })

    test('should automatically restart crashed processes via check method', async () => {
      const domain = 'example.com'
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid: null // No process running
        }
      }

      // Spy on the start method
      const startSpy = jest.spyOn(ProxyService, 'start')

      ProxyService.check()

      expect(startSpy).toHaveBeenCalledWith(domain)
    })

    test('should restart processes when watcher indicates they are not running', async () => {
      const domain = 'example.com'
      const pid = 12345
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid
        }
      }

      // Mark process as not running in watcher
      ProxyService['_OdacProxy__watcher'][pid] = false

      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      const startSpy = jest.spyOn(ProxyService, 'start')

      ProxyService.check()

      expect(mockProcess.stop).toHaveBeenCalledWith(pid)
      expect(mockConfig.config.websites[domain].pid).toBeNull()
      expect(startSpy).toHaveBeenCalledWith(domain)
    })

    test('should write logs to files during check', async () => {
      const domain = 'example.com'
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid: 12345
        }
      }

      // Setup logs
      ProxyService['_OdacProxy__logs'].log[domain] = 'Test log content'
      ProxyService['_OdacProxy__logs'].err[domain] = 'Test error content'
      ProxyService['_OdacProxy__watcher'][12345] = true

      os.homedir.mockReturnValue('/home/user')

      ProxyService.check()

      expect(fs.writeFile).toHaveBeenCalledWith('/home/user/.odac/logs/example.com.log', 'Test log content', expect.any(Function))
      expect(fs.writeFile).toHaveBeenCalledWith('/var/odac/example.com/error.log', 'Test error content', expect.any(Function))
    })

    test('should handle log file write errors gracefully', async () => {
      const domain = 'example.com'
      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid: 12345
        }
      }

      ProxyService['_OdacProxy__logs'].log[domain] = 'Test log content'
      ProxyService['_OdacProxy__watcher'][12345] = true

      // Mock fs.writeFile to call callback with error
      fs.writeFile.mockImplementation((path, data, callback) => {
        callback(new Error('Write failed'))
      })

      ProxyService.check()

      // Should not throw, error should be logged
      expect(mockLog).toHaveBeenCalledWith(expect.any(Error))
    })
  })

  describe.skip('website deletion and resource cleanup', () => {
    beforeEach(async () => {
      await ProxyService.init()
    })

    test('should delete website and cleanup all resources', async () => {
      const domain = 'example.com'
      const pid = 12345
      const port = 60000

      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid,
          port
        }
      }

      // Setup internal state
      ProxyService['_OdacProxy__watcher'][pid] = true
      ProxyService['_OdacProxy__ports'][port] = true
      ProxyService['_OdacProxy__logs'].log[domain] = 'log content'
      ProxyService['_OdacProxy__logs'].err[domain] = 'error content'
      ProxyService['_OdacProxy__error_counts'][domain] = 2
      ProxyService['_OdacProxy__active'][domain] = false
      ProxyService['_OdacProxy__started'][domain] = Date.now()

      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      const result = await ProxyService.delete(domain)

      expect(result.success).toBe(true)
      expect(result.message).toContain('Website example.com deleted')
      expect(mockConfig.config.websites[domain]).toBeUndefined()
      expect(mockProcess.stop).toHaveBeenCalledWith(pid)
      expect(ProxyService['_OdacProxy__watcher'][pid]).toBeUndefined()
      expect(ProxyService['_OdacProxy__ports'][port]).toBeUndefined()
      expect(ProxyService['_OdacProxy__logs'].log[domain]).toBeUndefined()
      expect(ProxyService['_OdacProxy__logs'].err[domain]).toBeUndefined()
      expect(ProxyService['_OdacProxy__error_counts'][domain]).toBeUndefined()
      expect(ProxyService['_OdacProxy__active'][domain]).toBeUndefined()
      expect(ProxyService['_OdacProxy__started'][domain]).toBeUndefined()
    })

    test('should handle deletion of non-existent website', async () => {
      const result = await ProxyService.delete('nonexistent.com')

      expect(result.success).toBe(false)
      expect(result.message).toContain('Website nonexistent.com not found')
    })

    test('should handle deletion of website without running process', async () => {
      const domain = 'example.com'

      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com',
          pid: null // No process running
        }
      }

      const result = await ProxyService.delete(domain)

      expect(result.success).toBe(true)
      expect(result.message).toContain('Website example.com deleted')
      expect(mockConfig.config.websites[domain]).toBeUndefined()
    })

    test('should strip protocol prefixes from domain in deletion', async () => {
      const domain = 'example.com'

      mockConfig.config.websites = {
        [domain]: {
          domain,
          path: '/var/odac/example.com'
        }
      }

      const result = await ProxyService.delete('https://example.com')

      expect(result.success).toBe(true)
      expect(mockConfig.config.websites[domain]).toBeUndefined()
    })

    test('should stop all website processes via stopAll method', () => {
      const domain1 = 'example.com'
      const domain2 = 'test.com'
      const pid1 = 12345
      const pid2 = 67890

      mockConfig.config.websites = {
        [domain1]: {domain: domain1, pid: pid1},
        [domain2]: {domain: domain2, pid: pid2}
      }

      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      ProxyService.stopAll()

      expect(mockProcess.stop).toHaveBeenCalledWith(pid1)
      expect(mockProcess.stop).toHaveBeenCalledWith(pid2)
      expect(mockConfig.config.websites[domain1].pid).toBeNull()
      expect(mockConfig.config.websites[domain2].pid).toBeNull()
    })

    test('should handle stopAll with no websites', () => {
      mockConfig.config.websites = {}

      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      expect(() => ProxyService.stopAll()).not.toThrow()
      expect(mockProcess.stop).not.toHaveBeenCalled()
    })

    test('should handle stopAll with websites that have no running processes', () => {
      const domain = 'example.com'

      mockConfig.config.websites = {
        [domain]: {domain, pid: null}
      }

      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      ProxyService.stopAll()

      expect(mockProcess.stop).not.toHaveBeenCalled()
    })
  })

  describe.skip('SSL certificate handling and SNI', () => {
    beforeEach(async () => {
      await ProxyService.init()
      mockConfig.config.ssl = {
        key: '/path/to/default.key',
        cert: '/path/to/default.cert'
      }
      mockConfig.config.websites = {
        'example.com': {
          domain: 'example.com',
          cert: {
            ssl: {
              key: '/path/to/example.key',
              cert: '/path/to/example.cert'
            }
          }
        },
        'test.com': {
          domain: 'test.com',
          cert: false
        }
      }
    })

    test('should use website-specific SSL certificate via SNI', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.key')
      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.cert')
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should fall back to default SSL certificate for websites without specific certs', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('test.com', mockCallback)

      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should resolve subdomain to parent domain for SSL certificate', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('www.example.com', mockCallback)

      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.key')
      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.cert')
    })

    test('should use default certificate for unknown domains', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('unknown.com', mockCallback)

      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle SSL certificate file read errors', () => {
      fs.readFileSync.mockImplementation(path => {
        if (path.includes('example.key')) {
          throw new Error('File not found')
        }
        return 'mock-file-content'
      })

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      expect(mockLog).toHaveBeenCalledWith('SSL certificate error for example.com: File not found')
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
    })

    test('should handle missing SSL certificate files gracefully', () => {
      fs.existsSync.mockImplementation(path => !path.includes('example.key') && !path.includes('example.cert'))

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      // Should fall back to default certificate
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle multi-level subdomain SSL certificate resolution', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      // Test multi-level subdomain resolution (api.staging.example.com -> example.com)
      sniCallback('api.staging.example.com', mockCallback)

      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.key')
      expect(fs.readFileSync).toHaveBeenCalledWith('/path/to/example.cert')
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle SSL certificate with missing cert property', () => {
      mockConfig.config.websites['example.com'].cert = {
        ssl: {
          key: '/path/to/example.key'
          // Missing cert property
        }
      }

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      // Should fall back to default certificate
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle SSL certificate with missing key property', () => {
      mockConfig.config.websites['example.com'].cert = {
        ssl: {
          cert: '/path/to/example.cert'
          // Missing key property
        }
      }

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      // Should fall back to default certificate
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle SSL certificate with missing ssl property', () => {
      mockConfig.config.websites['example.com'].cert = {
        // Missing ssl property
      }

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      // Should fall back to default certificate
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle hostname without dots in SNI callback', () => {
      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('localhost', mockCallback)

      // Should use default certificate for localhost
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-file-content',
        cert: 'mock-file-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should handle tls.createSecureContext errors', () => {
      tls.createSecureContext.mockImplementation(() => {
        throw new Error('Invalid certificate format')
      })

      ProxyService.server()

      const httpsOptions = https.createServer.mock.calls[0][0]
      const sniCallback = httpsOptions.SNICallback
      const mockCallback = jest.fn()

      sniCallback('example.com', mockCallback)

      expect(mockLog).toHaveBeenCalledWith('SSL certificate error for example.com: Invalid certificate format')
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
    })
  })

  describe('website deletion', () => {
    beforeEach(async () => {
      await ProxyService.init()
      mockConfig.config.websites = {
        'example.com': {
          domain: 'example.com',
          path: '/var/odac/example.com',
          pid: 12345,
          port: 3000
        }
      }
      ProxyService['_OdacProxy__watcher'] = {12345: true}
      ProxyService['_OdacProxy__ports'] = {3000: true}
      ProxyService['_OdacProxy__logs'] = {
        log: {'example.com': 'log content'},
        err: {'example.com': 'error content'}
      }
      ProxyService['_OdacProxy__error_counts'] = {'example.com': 2}
      ProxyService['_OdacProxy__active'] = {'example.com': false}
      ProxyService['_OdacProxy__started'] = {'example.com': Date.now()}
    })

    test('should delete website and cleanup all resources', async () => {
      const mockProcess = {
        stop: jest.fn()
      }
      mockOdac.setMock('core', 'Process', mockProcess)

      const result = await ProxyService.delete('example.com')

      expect(result.success).toBe(true)
      expect(result.message).toBe('Website example.com deleted.')
      expect(mockConfig.config.websites['example.com']).toBeUndefined()
      expect(mockProcess.stop).toHaveBeenCalledWith(12345)
    })

    test('should strip protocol prefixes before deletion', async () => {
      const result = await ProxyService.delete('https://example.com')

      expect(result.success).toBe(true)
      expect(mockConfig.config.websites['example.com']).toBeUndefined()
    })

    test('should return error for non-existent website', async () => {
      const result = await ProxyService.delete('nonexistent.com')

      expect(result.success).toBe(false)
      expect(result.message).toBe('Website nonexistent.com not found.')
    })

    test('should handle deletion of website without running process', async () => {
      mockConfig.config.websites['example.com'].pid = null

      const result = await ProxyService.delete('example.com')

      expect(result.success).toBe(true)
      expect(mockConfig.config.websites['example.com']).toBeUndefined()
    })
  })

  describe('utility methods', () => {
    beforeEach(async () => {
      await ProxyService.init()
    })

    test('should list all websites', async () => {
      mockConfig.config.websites = {
        'example.com': {},
        'test.com': {}
      }

      const result = await ProxyService.list()

      expect(result.success).toBe(true)
      expect(result.message).toContain('example.com')
      expect(result.message).toContain('test.com')
    })

    test('should return error when no websites exist', async () => {
      mockConfig.config.websites = {}

      const result = await ProxyService.list()

      expect(result.success).toBe(false)
      expect(result.message).toBe('No websites found.')
    })

    test('should return website status', async () => {
      mockConfig.config.websites = {'example.com': {}}

      const result = await ProxyService.status()

      expect(result).toEqual({'example.com': {}})
    })

    test('should set website configuration', () => {
      const data = {domain: 'new.com'}
      ProxyService.set('new.com', data)
      expect(mockConfig.config.websites['new.com']).toEqual(data)
    })

    test('should stop all websites', () => {
      // Setup mock container availability
      // Since Container is mocked globally via Odac.server('Container'), we need to handle that if used.
      // But stopAll checks Container availability.
      // Let's assume non-container environment for simplicity or mock it if needed.
      // In beforeEach we set available=false (actually we didn't explicitly set Container mock).
      // Let's rely on Process.stop being called if standard logic applies.

      const mockContainer = {
        available: false,
        stop: jest.fn()
      }
      mockOdac.setMock('server', 'Container', mockContainer)

      mockConfig.config.websites = {
        'site1.com': {pid: 1, domain: 'site1.com'},
        'site2.com': {pid: 2, domain: 'site2.com'}
      }

      ProxyService.stopAll()

      expect(mockOdac.core('Process').stop).toHaveBeenCalledWith(1)
      expect(mockOdac.core('Process').stop).toHaveBeenCalledWith(2)
      expect(mockConfig.config.websites['site1.com'].pid).toBeNull()
      expect(mockConfig.config.websites['site2.com'].pid).toBeNull()
    })

    test('should serve default index page', () => {
      const req = {}
      const res = {write: jest.fn(), end: jest.fn()}

      ProxyService.index(req, res)

      expect(res.write).toHaveBeenCalledWith('ODAC Server')
      expect(res.end).toHaveBeenCalled()
    })
  })
})
