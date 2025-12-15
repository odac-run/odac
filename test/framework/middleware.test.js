const Route = require('../../framework/src/Route.js')

describe('Middleware System', () => {
  let route

  beforeEach(() => {
    route = new Route()
    global.Candy = {Route: {buff: 'test'}}
    global.__dir = __dirname
  })

  test('use() should set pending middlewares', () => {
    route.use('auth', 'logger')
    expect(route._pendingMiddlewares).toEqual(['auth', 'logger'])
  })

  test('use() should support chaining', () => {
    const result = route.use('auth')
    expect(result).toBe(route)
  })

  test('use() with no args should reset middlewares', () => {
    route.use('auth', 'logger')
    route.use()
    expect(route._pendingMiddlewares).toEqual([])
  })

  test('page() should return this for chaining', () => {
    const result = route.page('/', 'index')
    expect(result).toBe(route)
  })

  test('post() should return this for chaining', () => {
    const result = route.post('/api', 'api')
    expect(result).toBe(route)
  })

  test('get() should return this for chaining', () => {
    const result = route.get('/api', 'api')
    expect(result).toBe(route)
  })

  test('auth.use() should work', () => {
    const result = route.auth.use('admin')
    expect(result).toBe(route)
    expect(route._pendingMiddlewares).toEqual(['admin'])
  })

  test('chaining should work: use().page().page()', () => {
    route
      .use('auth')
      .page('/profile', () => {})
      .page('/settings', () => {})
    expect(route._pendingMiddlewares).toEqual(['auth'])
  })

  test('chaining should work: auth.use().page()', () => {
    route.auth.use('admin').page('/admin', () => {})
    expect(route._pendingMiddlewares).toEqual(['admin'])
  })
})
