const App = require('../../server/src/App')

describe('App', () => {
  let mockConfig
  let mockRunApp
  let mockCloneRepo

  beforeEach(() => {
    // Reset config for each test
    mockConfig = {}
    mockRunApp = jest.fn()
    mockCloneRepo = jest.fn()

    // Mock global translation
    global.__ = msg => msg

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
      server: jest.fn(module => {
        // Return a combined object that satisfies both Container and Api calls if needed
        // But better to check the module name to return specific mocks
        if (module === 'Container') {
          return {
            available: true,
            runApp: mockRunApp,
            cloneRepo: mockCloneRepo,
            build: jest.fn(),
            isRunning: jest.fn(() => false),
            stop: jest.fn(),
            list: jest.fn(() => []),
            getStats: jest.fn(),
            remove: jest.fn()
          }
        }
        if (module === 'Api') {
          return {
            result: jest.fn((success, message) => ({success, message}))
          }
        }
        // Legacy fallback or other modules
        return {
          result: jest.fn((success, message) => ({success, message})),
          isRunning: jest.fn(() => false)
        }
      })
    }
  })

  afterEach(() => {
    delete global.Odac
    delete global.__
    jest.clearAllMocks()
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

  describe('concurrency control', () => {
    test('should prevent concurrent run calls for the same app from check()', async () => {
      // Setup specific config for this test
      mockConfig.apps = [{id: 999, name: 'concurrent-app', active: true, type: 'container', status: 'running'}]

      let resolveRun
      const runPromise = new Promise(r => {
        resolveRun = r
      })

      // Make execution slow
      mockRunApp.mockReturnValue(runPromise)

      // First check triggers the run
      await App.check()

      // Second check should see lock and skip
      await App.check()

      // Third check
      await App.check()

      // Should have only called the container run ONCE
      expect(mockRunApp).toHaveBeenCalledTimes(1)

      // Finish the run
      resolveRun()
    })

    test('should prevent concurrent createFromGit calls for the same app name', async () => {
      // Setup web path for create()
      mockConfig.app = {path: '/tmp/odac-test'}

      let resolveClone
      const clonePromise = new Promise(r => {
        resolveClone = r
      })

      // Mock slow git clone
      mockCloneRepo.mockImplementation(() => clonePromise)

      const config = {
        type: 'git',
        url: 'https://github.com/test/repo.git',
        name: 'concurrent-git-app'
      }

      // First call - starts cloning
      const promise1 = App.create(config)

      // Second call - should fail immediately due to lock
      const promise2 = App.create(config)

      const result2 = await promise2
      expect(result2.success).toBe(false)
      expect(result2.message).toMatch(/already being created/)

      // Finish the first clone
      resolveClone()

      // Wait for first creation to finish
      await promise1
    })
  })
})
