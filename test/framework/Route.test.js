const Route = require('../../framework/src/Route')

describe('Route', () => {
  let route

  beforeEach(() => {
    route = new Route()
    global.Odac = {
      Route: {},
      Config: {}
    }
    global.__dir = process.cwd()
  })

  afterEach(() => {
    delete global.Odac
    delete global.__dir
  })

  describe('check - token request', () => {
    it('should handle token request with undefined route gracefully', async () => {
      const mockOdac = {
        Request: {
          url: '/',
          method: 'get',
          route: 'undefined_route',
          ssl: false,
          host: 'example.com',
          header: jest.fn(key => {
            const headers = {
              'X-Odac': 'token',
              Referer: 'http://example.com/',
              'X-Odac-Client': 'test-client'
            }
            return headers[key]
          }),
          cookie: jest.fn(key => {
            if (key === 'odac_client') return 'test-client'
            return null
          }),
          abort: jest.fn()
        },
        token: jest.fn(() => 'test-token')
      }

      route.routes = {}

      const result = await route.check(mockOdac)

      expect(result).toBeDefined()
      expect(result.token).toBe('test-token')
      expect(result.page).toBeUndefined()
      expect(mockOdac.Request.header).toHaveBeenCalledWith('Access-Control-Allow-Origin', 'http://example.com')
      expect(mockOdac.Request.header).toHaveBeenCalledWith('Cache-Control', 'no-store, no-cache, must-revalidate, max-age=0')
    })

    it('should handle token request with route but no page defined', async () => {
      const mockOdac = {
        Request: {
          url: '/',
          method: 'get',
          route: 'test_route',
          ssl: true,
          host: 'example.com',
          header: jest.fn(key => {
            const headers = {
              'X-Odac': 'token',
              Referer: 'https://example.com/',
              'X-Odac-Client': 'test-client'
            }
            return headers[key]
          }),
          cookie: jest.fn(key => {
            if (key === 'odac_client') return 'test-client'
            return null
          }),
          abort: jest.fn()
        },
        token: jest.fn(() => 'test-token-2')
      }

      route.routes = {
        test_route: {}
      }

      const result = await route.check(mockOdac)

      expect(result).toBeDefined()
      expect(result.token).toBe('test-token-2')
      expect(result.page).toBeUndefined()
    })

    it('should handle token request with route and page but no url match', async () => {
      const mockOdac = {
        Request: {
          url: '/',
          method: 'get',
          route: 'test_route',
          ssl: false,
          host: 'example.com',
          header: jest.fn(key => {
            const headers = {
              'X-Odac': 'token',
              Referer: 'http://example.com/',
              'X-Odac-Client': 'test-client'
            }
            return headers[key]
          }),
          cookie: jest.fn(key => {
            if (key === 'odac_client') return 'test-client'
            return null
          }),
          abort: jest.fn()
        },
        token: jest.fn(() => 'test-token-3')
      }

      route.routes = {
        test_route: {
          page: {
            '/other': {file: 'other.js'}
          }
        }
      }

      const result = await route.check(mockOdac)

      expect(result).toBeDefined()
      expect(result.token).toBe('test-token-3')
      expect(result.page).toBeUndefined()
    })

    it('should not return token when referer does not match', async () => {
      const mockOdac = {
        Request: {
          url: '/',
          method: 'get',
          route: 'test_route',
          ssl: false,
          host: 'example.com',
          header: jest.fn(key => {
            const headers = {
              'X-Odac': 'token',
              Referer: 'http://malicious.com/',
              'X-Odac-Client': 'test-client'
            }
            return headers[key]
          }),
          cookie: jest.fn(key => {
            if (key === 'odac_client') return 'test-client'
            return null
          }),
          abort: jest.fn()
        },
        Config: {},
        token: jest.fn(() => 'test-token')
      }

      route.routes = {
        test_route: {
          page: {}
        }
      }

      await route.check(mockOdac)

      expect(mockOdac.Request.header).not.toHaveBeenCalledWith('Access-Control-Allow-Origin', expect.any(String))
    })

    it('should not return token when client cookie does not match', async () => {
      const mockOdac = {
        Request: {
          url: '/',
          method: 'get',
          route: 'test_route',
          ssl: false,
          host: 'example.com',
          header: jest.fn(key => {
            const headers = {
              'X-Odac': 'token',
              Referer: 'http://example.com/',
              'X-Odac-Client': 'test-client'
            }
            return headers[key]
          }),
          cookie: jest.fn(key => {
            if (key === 'odac_client') return 'different-client'
            return null
          }),
          abort: jest.fn()
        },
        Config: {},
        token: jest.fn(() => 'test-token')
      }

      route.routes = {
        test_route: {
          page: {}
        }
      }

      await route.check(mockOdac)

      expect(mockOdac.Request.header).not.toHaveBeenCalledWith('Access-Control-Allow-Origin', expect.any(String))
    })
  })

  describe('set', () => {
    it('should register a route with function handler', () => {
      global.Odac.Route.buff = 'test_route'
      const handler = jest.fn()

      route.set('get', '/test', handler)

      expect(route.routes.test_route).toBeDefined()
      expect(route.routes.test_route.get).toBeDefined()
      expect(route.routes.test_route.get['/test']).toBeDefined()
      expect(route.routes.test_route.get['/test'].cache).toBe(handler)
      expect(route.routes.test_route.get['/test'].type).toBe('function')
    })

    it('should handle array of methods', () => {
      global.Odac.Route.buff = 'test_route'
      const handler = jest.fn()

      route.set(['get', 'post'], '/test', handler)

      expect(route.routes.test_route.get['/test']).toBeDefined()
      expect(route.routes.test_route.post['/test']).toBeDefined()
    })

    it('should strip trailing slash from url', () => {
      global.Odac.Route.buff = 'test_route'
      const handler = jest.fn()

      route.set('get', '/test/', handler)

      expect(route.routes.test_route.get['/test']).toBeDefined()
      expect(route.routes.test_route.get['/test/']).toBeUndefined()
    })
  })

  describe('WebSocket cleanup', () => {
    it('should call ws() method successfully', () => {
      const handler = jest.fn()
      expect(() => {
        route.ws('/test', handler, {token: false})
      }).not.toThrow()
    })

    it('should call auth.ws() method successfully', () => {
      const handler = jest.fn()
      expect(() => {
        route.auth.ws('/test', handler)
      }).not.toThrow()
    })
  })
})
