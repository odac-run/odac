/**
 * Unit tests for Web/Proxy.js module
 * Tests custom HTTP proxy implementation for both HTTP/1 and HTTP/2
 */

jest.mock('http')

const http = require('http')
const WebProxy = require('../../../server/src/Web/Proxy.js')

describe('WebProxy', () => {
  let proxy
  let mockLog
  let mockReq
  let mockRes
  let mockWebsite

  beforeEach(() => {
    jest.clearAllMocks()

    mockLog = jest.fn()
    proxy = new WebProxy(mockLog)

    mockReq = {
      url: '/test',
      method: 'GET',
      headers: {
        host: 'example.com',
        'user-agent': 'test-agent'
      },
      socket: {
        remoteAddress: '192.168.1.100'
      },
      setTimeout: jest.fn(),
      on: jest.fn(),
      pipe: jest.fn()
    }

    mockRes = {
      writeHead: jest.fn(),
      end: jest.fn(),
      setTimeout: jest.fn(),
      on: jest.fn(),
      headersSent: false,
      writeEarlyHints: jest.fn()
    }

    mockWebsite = {
      port: 3000,
      domain: 'example.com'
    }
  })

  describe('http1 proxy', () => {
    test('should create HTTP request with correct options', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      expect(http.request).toHaveBeenCalledWith(
        expect.objectContaining({
          hostname: '127.0.0.1',
          port: 3000,
          path: '/test',
          method: 'GET',
          timeout: 0
        }),
        expect.any(Function)
      )
    })

    test('should add custom headers to proxy request', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      const options = http.request.mock.calls[0][0]
      expect(options.headers).toMatchObject({
        host: 'example.com',
        'user-agent': 'test-agent',
        'x-odac-connection-remoteaddress': '192.168.1.100',
        'x-odac-connection-ssl': 'true'
      })
    })

    test('should handle proxy connection errors', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      const errorHandler = mockProxyReq.on.mock.calls.find(call => call[0] === 'error')[1]
      const error = new Error('Connection refused')
      error.code = 'ECONNREFUSED'

      errorHandler(error)

      expect(mockLog).toHaveBeenCalledWith('Proxy error for example.com: Connection refused')
      expect(mockRes.writeHead).toHaveBeenCalledWith(502)
      expect(mockRes.end).toHaveBeenCalledWith('Bad Gateway')
    })

    test('should not send error response if headers already sent', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)
      mockRes.headersSent = true

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      const errorHandler = mockProxyReq.on.mock.calls.find(call => call[0] === 'error')[1]
      const error = new Error('Connection timeout')

      errorHandler(error)

      expect(mockLog).toHaveBeenCalledWith('Proxy error for example.com: Connection timeout')
      expect(mockRes.writeHead).not.toHaveBeenCalled()
      expect(mockRes.end).not.toHaveBeenCalledWith('Bad Gateway')
    })

    test('should ignore ECONNRESET errors', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      const errorHandler = mockProxyReq.on.mock.calls.find(call => call[0] === 'error')[1]
      const error = new Error('Connection reset')
      error.code = 'ECONNRESET'

      errorHandler(error)

      expect(mockLog).not.toHaveBeenCalled()
      expect(mockRes.writeHead).not.toHaveBeenCalled()
    })

    test('should filter forbidden response headers', done => {
      const mockProxyRes = {
        statusCode: 200,
        headers: {
          'content-type': 'application/json',
          connection: 'keep-alive',
          'keep-alive': 'timeout=5',
          'transfer-encoding': 'chunked',
          upgrade: 'websocket',
          'proxy-connection': 'keep-alive',
          'x-custom-header': 'value'
        },
        pipe: jest.fn()
      }

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockImplementation((options, callback) => {
        setTimeout(() => callback(mockProxyRes), 0)
        return mockProxyReq
      })

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      setTimeout(() => {
        expect(mockRes.writeHead).toHaveBeenCalledWith(
          200,
          expect.objectContaining({
            'content-type': 'application/json',
            'x-custom-header': 'value'
          })
        )

        const headers = mockRes.writeHead.mock.calls[0][1]
        expect(headers).not.toHaveProperty('connection')
        expect(headers).not.toHaveProperty('keep-alive')
        expect(headers).not.toHaveProperty('transfer-encoding')
        expect(headers).not.toHaveProperty('upgrade')
        expect(headers).not.toHaveProperty('proxy-connection')
        done()
      }, 10)
    })

    test('should handle Server-Sent Events connections', done => {
      const mockProxyRes = {
        statusCode: 200,
        headers: {
          'content-type': 'text/event-stream'
        },
        pipe: jest.fn()
      }

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn(),
        destroy: jest.fn()
      }

      http.request.mockImplementation((options, callback) => {
        setTimeout(() => callback(mockProxyRes), 0)
        return mockProxyReq
      })

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      setTimeout(() => {
        expect(mockReq.setTimeout).toHaveBeenCalledWith(0)
        expect(mockRes.setTimeout).toHaveBeenCalledWith(0)
        expect(mockReq.on).toHaveBeenCalledWith('close', expect.any(Function))
        expect(mockReq.on).toHaveBeenCalledWith('aborted', expect.any(Function))
        expect(mockRes.on).toHaveBeenCalledWith('close', expect.any(Function))
        done()
      }, 10)
    })

    test('should handle early hints', done => {
      const mockProxyRes = {
        statusCode: 200,
        headers: {
          'content-type': 'text/html',
          'x-odac-early-hints': JSON.stringify(['</style.css>; rel=preload; as=style'])
        },
        pipe: jest.fn()
      }

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockImplementation((options, callback) => {
        setTimeout(() => callback(mockProxyRes), 0)
        return mockProxyReq
      })

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      setTimeout(() => {
        expect(mockRes.writeEarlyHints).toHaveBeenCalledWith({
          link: ['</style.css>; rel=preload; as=style']
        })

        const headers = mockRes.writeHead.mock.calls[0][1]
        expect(headers).not.toHaveProperty('x-odac-early-hints')
        done()
      }, 10)
    })

    test('should ignore invalid early hints JSON', done => {
      const mockProxyRes = {
        statusCode: 200,
        headers: {
          'content-type': 'text/html',
          'x-odac-early-hints': 'invalid-json'
        },
        pipe: jest.fn()
      }

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockImplementation((options, callback) => {
        setTimeout(() => callback(mockProxyRes), 0)
        return mockProxyReq
      })

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      setTimeout(() => {
        expect(mockRes.writeEarlyHints).not.toHaveBeenCalled()
        expect(mockRes.writeHead).toHaveBeenCalled()
        done()
      }, 10)
    })

    test('should pipe request to proxy', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      expect(mockReq.pipe).toHaveBeenCalledWith(mockProxyReq)
    })

    test('should pipe proxy response to client', done => {
      const mockProxyRes = {
        statusCode: 200,
        headers: {'content-type': 'text/plain'},
        pipe: jest.fn()
      }

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockImplementation((options, callback) => {
        setTimeout(() => callback(mockProxyRes), 0)
        return mockProxyReq
      })

      proxy.http1(mockReq, mockRes, mockWebsite, 'example.com')

      setTimeout(() => {
        expect(mockProxyRes.pipe).toHaveBeenCalledWith(mockRes)
        done()
      }, 10)
    })
  })

  describe('http2 proxy', () => {
    test('should filter HTTP/2 pseudo-headers', () => {
      mockReq.headers[':method'] = 'GET'
      mockReq.headers[':path'] = '/test'
      mockReq.headers[':scheme'] = 'https'
      mockReq.headers[':authority'] = 'example.com'

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http2(mockReq, mockRes, mockWebsite, 'example.com')

      const options = http.request.mock.calls[0][0]
      expect(options.headers).not.toHaveProperty(':method')
      expect(options.headers).not.toHaveProperty(':path')
      expect(options.headers).not.toHaveProperty(':scheme')
      expect(options.headers).not.toHaveProperty(':authority')
      expect(options.headers).toHaveProperty('user-agent')
    })

    test('should handle HTTP/2 proxy errors', () => {
      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http2(mockReq, mockRes, mockWebsite, 'example.com')

      const errorHandler = mockProxyReq.on.mock.calls.find(call => call[0] === 'error')[1]
      const error = new Error('Connection failed')

      errorHandler(error)

      expect(mockLog).toHaveBeenCalledWith('Proxy error for example.com: Connection failed')
      expect(mockRes.writeHead).toHaveBeenCalledWith(502)
      expect(mockRes.end).toHaveBeenCalledWith('Bad Gateway')
    })

    test('should handle missing remote address', () => {
      mockReq.socket = {}

      const mockProxyReq = {
        on: jest.fn(),
        pipe: jest.fn()
      }

      http.request.mockReturnValue(mockProxyReq)

      proxy.http2(mockReq, mockRes, mockWebsite, 'example.com')

      const options = http.request.mock.calls[0][0]
      expect(options.headers['x-odac-connection-remoteaddress']).toBe('')
    })
  })
})
