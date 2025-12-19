const EarlyHints = require('../../../framework/src/View/EarlyHints')

describe('EarlyHints', () => {
  let earlyHints
  let mockConfig

  beforeEach(() => {
    mockConfig = {
      enabled: true,
      auto: true,
      maxResources: 5
    }
    earlyHints = new EarlyHints(mockConfig)
  })

  describe('initialization', () => {
    it('should create instance with default config', () => {
      const hints = new EarlyHints()
      expect(hints).toBeDefined()
    })

    it('should create instance with custom config', () => {
      const hints = new EarlyHints({enabled: false})
      expect(hints).toBeDefined()
    })
  })

  describe('extractFromHtml', () => {
    it('should extract CSS resources from head', () => {
      const html = `
        <html>
          <head>
            <link rel="stylesheet" href="/css/main.css">
            <link rel="stylesheet" href="/css/theme.css">
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(2)
      expect(resources[0]).toEqual({href: '/css/main.css', as: 'style'})
      expect(resources[1]).toEqual({href: '/css/theme.css', as: 'style'})
    })

    it('should extract JS resources from head', () => {
      const html = `
        <html>
          <head>
            <script src="/js/app.js"></script>
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(1)
      expect(resources[0]).toEqual({href: '/js/app.js', as: 'script'})
    })

    it('should not extract deferred JS', () => {
      const html = `
        <html>
          <head>
            <script src="/js/app.js" defer></script>
            <script src="/js/async.js" async></script>
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(0)
    })

    it('should extract font resources', () => {
      const html = `
        <html>
          <head>
            <link rel="preload" href="/fonts/main.woff2" as="font">
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(1)
      expect(resources[0]).toEqual({
        href: '/fonts/main.woff2',
        as: 'font',
        crossorigin: 'anonymous'
      })
    })

    it('should limit resources to maxResources', () => {
      const html = `
        <html>
          <head>
            <link rel="stylesheet" href="/css/1.css">
            <link rel="stylesheet" href="/css/2.css">
            <link rel="stylesheet" href="/css/3.css">
            <link rel="stylesheet" href="/css/4.css">
            <link rel="stylesheet" href="/css/5.css">
            <link rel="stylesheet" href="/css/6.css">
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(5)
    })

    it('should return empty array when no head tag', () => {
      const html = '<html><body></body></html>'
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toEqual([])
    })

    it('should return empty array when disabled', () => {
      const hints = new EarlyHints({enabled: false})
      const html = '<html><head><link rel="stylesheet" href="/css/main.css"></head></html>'
      const resources = hints.extractFromHtml(html)
      expect(resources).toEqual([])
    })

    it('should skip resources with defer attribute', () => {
      const html = `
        <html>
          <head>
            <link rel="stylesheet" href="/css/critical.css">
            <link rel="stylesheet" href="/css/non-critical.css" defer>
            <script src="/js/app.js"></script>
            <script src="/js/analytics.js" defer></script>
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(2)
      expect(resources[0]).toEqual({href: '/css/critical.css', as: 'style'})
      expect(resources[1]).toEqual({href: '/js/app.js', as: 'script'})
    })

    it('should only detect stylesheets with rel="stylesheet"', () => {
      const html = `
        <html>
          <head>
            <link rel="stylesheet" href="/css/main.css">
            <link rel="icon" href="/favicon.css">
            <link rel="preload" href="/data.css" as="fetch">
            <link href="/other.css">
          </head>
          <body></body>
        </html>
      `
      const resources = earlyHints.extractFromHtml(html)
      expect(resources).toHaveLength(1)
      expect(resources[0]).toEqual({href: '/css/main.css', as: 'style'})
    })
  })

  describe('formatLinkHeader', () => {
    it('should format basic resource', () => {
      const resource = {href: '/css/main.css', as: 'style'}
      const header = earlyHints.formatLinkHeader(resource)
      expect(header).toBe('</css/main.css>; rel=preload; as=style')
    })

    it('should format resource with crossorigin', () => {
      const resource = {href: '/font.woff2', as: 'font', crossorigin: 'anonymous'}
      const header = earlyHints.formatLinkHeader(resource)
      expect(header).toBe('</font.woff2>; rel=preload; as=font; crossorigin')
    })

    it('should format resource with type', () => {
      const resource = {href: '/data.json', as: 'fetch', type: 'application/json'}
      const header = earlyHints.formatLinkHeader(resource)
      expect(header).toBe('</data.json>; rel=preload; as=fetch; type=application/json')
    })
  })

  describe('caching', () => {
    it('should cache hints for route', () => {
      const resources = [{href: '/css/main.css', as: 'style'}]
      earlyHints.cacheHints('/home', resources)

      const cached = earlyHints.getHints(null, '/home')
      expect(cached).toEqual(resources)
    })

    it('should not cache when disabled', () => {
      const hints = new EarlyHints({enabled: false})
      const resources = [{href: '/css/main.css', as: 'style'}]
      hints.cacheHints('/home', resources)

      const cached = hints.getHints(null, '/home')
      expect(cached).toBeNull()
    })
  })

  describe('send', () => {
    it('should return false when disabled', () => {
      const hints = new EarlyHints({enabled: false})
      const mockRes = {
        headersSent: false,
        writableEnded: false,
        writeEarlyHints: jest.fn()
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = hints.send(mockRes, resources)
      expect(result).toBe(false)
      expect(mockRes.writeEarlyHints).not.toHaveBeenCalled()
    })

    it('should return false when headers already sent', () => {
      const mockRes = {
        headersSent: true,
        writableEnded: false,
        writeEarlyHints: jest.fn()
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = earlyHints.send(mockRes, resources)
      expect(result).toBe(false)
      expect(mockRes.writeEarlyHints).not.toHaveBeenCalled()
    })

    it('should return false when response ended', () => {
      const mockRes = {
        headersSent: false,
        writableEnded: true,
        writeEarlyHints: jest.fn()
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = earlyHints.send(mockRes, resources)
      expect(result).toBe(false)
      expect(mockRes.writeEarlyHints).not.toHaveBeenCalled()
    })

    it('should return true even when writeEarlyHints not available', () => {
      const mockRes = {
        headersSent: false,
        writableEnded: false,
        setHeader: jest.fn()
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = earlyHints.send(mockRes, resources)
      expect(result).toBe(true)
      expect(mockRes.setHeader).toHaveBeenCalledWith('X-Odac-Early-Hints', JSON.stringify(['</css/main.css>; rel=preload; as=style']))
    })

    it('should send early hints successfully', () => {
      const mockRes = {
        headersSent: false,
        writableEnded: false,
        writeEarlyHints: jest.fn(),
        setHeader: jest.fn()
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = earlyHints.send(mockRes, resources)
      expect(result).toBe(true)
      expect(mockRes.writeEarlyHints).toHaveBeenCalledWith({
        link: ['</css/main.css>; rel=preload; as=style']
      })
      expect(mockRes.setHeader).toHaveBeenCalledWith('X-Odac-Early-Hints', JSON.stringify(['</css/main.css>; rel=preload; as=style']))
    })

    it('should handle writeEarlyHints errors gracefully', () => {
      const mockRes = {
        headersSent: false,
        writableEnded: false,
        writeEarlyHints: jest.fn(() => {
          throw new Error('Write error')
        })
      }
      const resources = [{href: '/css/main.css', as: 'style'}]

      const result = earlyHints.send(mockRes, resources)
      expect(result).toBe(false)
    })
  })
})
