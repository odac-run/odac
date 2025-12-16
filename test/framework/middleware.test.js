const Route = require('../../framework/src/Route.js')

describe('Middleware System', () => {
  let route

  beforeEach(() => {
    route = new Route()
    global.Candy = {Route: {buff: 'test'}}
    global.__dir = __dirname
  })

  test('use() should return MiddlewareChain', () => {
    const chain = route.use('auth', 'logger')
    expect(chain).not.toBe(route)
    expect(chain._middlewares).toEqual(['auth', 'logger'])
  })

  test('use() should support chaining with more middlewares', () => {
    const chain = route.use('auth').use('logger')
    expect(chain._middlewares).toEqual(['auth', 'logger'])
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

  test('auth.use() should return MiddlewareChain', () => {
    const chain = route.auth.use('admin')
    expect(chain).not.toBe(route)
    expect(chain._middlewares).toEqual(['admin'])
  })

  test('chaining should work: use().page().page()', () => {
    route
      .use('auth')
      .page('/profile', () => {})
      .page('/settings', () => {})
    expect(route.routes.test.page['/profile'].middlewares).toEqual(['auth'])
    expect(route.routes.test.page['/settings'].middlewares).toEqual(['auth'])
  })

  test('chaining should work: auth.use().page()', () => {
    route.auth.use('admin').page('/admin', () => {})
    expect(route.routes.test.page['/admin'].middlewares).toEqual(['admin'])
  })

  test('middlewares should be attached to routes', () => {
    route
      .use('auth')
      .page('/profile', () => {})
      .page('/settings', () => {})
    expect(route.routes.test.page['/profile'].middlewares).toEqual(['auth'])
    expect(route.routes.test.page['/settings'].middlewares).toEqual(['auth'])
  })

  test('routes without use() should have no middlewares', () => {
    route.use('auth').page('/profile', () => {})
    route.page('/public', () => {})
    expect(route.routes.test.page['/profile'].middlewares).toEqual(['auth'])
    expect(route.routes.test.page['/public'].middlewares).toBeUndefined()
  })

  test('multiple middlewares should be attached', () => {
    route.use('cors', 'rateLimit').post('/api/upload', () => {})
    expect(route.routes.test.post['/api/upload'].middlewares).toEqual(['cors', 'rateLimit'])
  })

  test('separate use() chains should be independent', () => {
    route.use('auth').page('/profile', () => {})
    route.use('cors').page('/api', () => {})
    expect(route.routes.test.page['/profile'].middlewares).toEqual(['auth'])
    expect(route.routes.test.page['/api'].middlewares).toEqual(['cors'])
  })
})
