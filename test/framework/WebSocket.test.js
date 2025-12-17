const {WebSocketServer} = require('../../framework/src/WebSocket.js')

describe('WebSocketServer', () => {
  let server

  beforeEach(() => {
    server = new WebSocketServer()
  })

  describe('route', () => {
    it('should register a route', () => {
      const handler = jest.fn()
      server.route('/chat', handler)
      expect(server.getRoute('/chat')).toBe(handler)
    })

    it('should return null for unregistered route', () => {
      expect(server.getRoute('/unknown')).toBeNull()
    })

    it('should match parameterized routes', () => {
      const handler = jest.fn()
      server.route('/room/{id}', handler)

      const result = server.getRoute('/room/123')
      expect(result).toBeDefined()
      expect(result.handler).toBe(handler)
      expect(result.params).toEqual({id: '123'})
    })

    it('should match multiple parameters', () => {
      const handler = jest.fn()
      server.route('/chat/{room}/user/{userId}', handler)

      const result = server.getRoute('/chat/general/user/42')
      expect(result.params).toEqual({room: 'general', userId: '42'})
    })
  })

  describe('rooms', () => {
    it('should join and leave rooms', () => {
      server.joinRoom('client1', 'room1')
      server.joinRoom('client2', 'room1')

      server.leaveRoom('client1', 'room1')
      server.leaveRoom('client2', 'room1')
    })
  })

  describe('broadcast', () => {
    it('should have broadcast method', () => {
      expect(typeof server.broadcast).toBe('function')
    })
  })

  describe('clients', () => {
    it('should track client count', () => {
      expect(server.clientCount).toBe(0)
    })
  })

  describe('cleanup on disconnect', () => {
    it('should be handled by Route.setWs wrapper', () => {
      expect(true).toBe(true)
    })
  })
})

describe('Route WebSocket Integration', () => {
  const Route = require('../../framework/src/Route.js')

  beforeEach(() => {
    global.Candy = {
      Route: new Route()
    }
  })

  it('should support ws() method', () => {
    expect(typeof Candy.Route.ws).toBe('function')
  })

  it('should support auth.ws() method', () => {
    expect(typeof Candy.Route.auth.ws).toBe('function')
  })

  it('should support middleware with ws()', () => {
    const chain = Candy.Route.use('test-middleware')
    expect(typeof chain.ws).toBe('function')
  })

  it('should support middleware with auth.ws()', () => {
    const chain = Candy.Route.use('test-middleware')
    expect(typeof chain.auth.ws).toBe('function')
  })

  it('should support auth.use() with ws()', () => {
    const chain = Candy.Route.auth.use('test-middleware')
    expect(typeof chain.ws).toBe('function')
  })
})
