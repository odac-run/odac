const App = require('../../server/src/App')

describe('App', () => {
  let mockConfig

  beforeEach(() => {
    // Reset config for each test
    mockConfig = {}

    // Mock global Odac
    global.Odac = {
      core: jest.fn(module => {
        if (module === 'Log') {
          return {
            init: () => ({
              log: jest.fn(),
              error: jest.fn()
            })
          }
        }
        if (module === 'Config') {
          return {config: mockConfig}
        }
        return {}
      }),
      server: jest.fn(() => ({
        result: jest.fn((success, message) => ({success, message})),
        isRunning: jest.fn(() => false) // Mock Container service
      }))
    }
  })

  afterEach(() => {
    delete global.Odac
  })

  describe('configuration handling', () => {
    test('should handle undefined apps config gracefully', async () => {
      mockConfig.apps = undefined

      // Should not throw
      await expect(App.init()).resolves.not.toThrow()
      await expect(App.check()).resolves.not.toThrow()

      // Should treat as empty array
      const apps = await App.status()
      expect(apps).toEqual([])
    })

    test('should handle null apps config gracefully', async () => {
      mockConfig.apps = null

      await expect(App.init()).resolves.not.toThrow()
      await expect(App.check()).resolves.not.toThrow()

      const apps = await App.status()
      expect(apps).toEqual([])
    })

    test('should handle object (non-iterable) apps config gracefully', async () => {
      // Simulate the structure that might have caused "is not iterable" if treated as array
      mockConfig.apps = {some: 'object'}

      await expect(App.init()).resolves.not.toThrow()
      await expect(App.check()).resolves.not.toThrow()

      const apps = await App.status()
      expect(apps).toEqual([])
    })

    test('should valid apps array normally', async () => {
      mockConfig.apps = [{id: 1, name: 'test-app', active: true, type: 'container'}]

      await expect(App.init()).resolves.not.toThrow()

      const apps = await App.status()
      expect(apps).toHaveLength(1)
      expect(apps[0].name).toBe('test-app')
    })
  })
})
