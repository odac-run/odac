const fs = require('fs')
jest.mock('fs', () => ({
  existsSync: jest.fn(() => true),
  mkdirSync: jest.fn(),
  rmSync: jest.fn(),
  promises: {
    mkdir: jest.fn().mockResolvedValue(true),
    rm: jest.fn().mockResolvedValue(true),
    access: jest.fn().mockResolvedValue(true)
  },
  createWriteStream: jest.fn(() => ({
    write: jest.fn(),
    end: jest.fn(),
    on: jest.fn()
  }))
}))

let App

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
            remove: jest.fn(),
            fetchRepo: jest.fn(),
            getImageExposedPorts: jest.fn(() => [])
          }
        }
        if (module === 'Api') {
          return {
            result: jest.fn((result, message, data) => {
              if (typeof message === 'object') {
                data = message
                message = undefined
              }
              return {result, success: result, message, data}
            }),
            generateAppToken: jest.fn(() => 'mock-app-token'),
            hostSocketDir: '/tmp/odac-socket'
          }
        }
        return {
          result: jest.fn((result, message, data) => {
            if (typeof message === 'object') {
              data = message
              message = undefined
            }
            return {result, success: result, message, data}
          }),
          isRunning: jest.fn(() => false),
          stop: jest.fn(),
          trigger: jest.fn(),
          syncConfig: jest.fn()
        }
      })
    }

    jest.isolateModules(() => {
      App = require('../../server/src/App')
    })
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
      const {data: apps} = await App.list(true)
      expect(apps).toEqual([])
    })

    test('should handle null apps config gracefully', async () => {
      mockConfig.apps = null

      await expect(App.init()).resolves.not.toThrow()
      await expect(App.check()).resolves.not.toThrow()

      const {data: apps} = await App.list(true)
      expect(apps).toEqual([])
    })

    test('should handle object (non-iterable) apps config gracefully', async () => {
      // Simulate the structure that might have caused "is not iterable" if treated as array
      mockConfig.apps = {some: 'object'}

      await expect(App.init()).resolves.not.toThrow()
      await expect(App.check()).resolves.not.toThrow()

      const {data: apps} = await App.list(true)
      expect(apps).toEqual([])
    })

    test('should valid apps array normally', async () => {
      mockConfig.apps = [{id: 1, name: 'test-app', active: true, type: 'container'}]

      await expect(App.init()).resolves.not.toThrow()

      const {data: apps} = await App.list(true)
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

  describe('api permission handling', () => {
    test('should inject API token and socket when app has api permission', async () => {
      mockConfig.apps = [
        {
          id: 101,
          name: 'api-aware-app',
          active: true,
          type: 'container',
          image: 'test:latest',
          api: true
        }
      ]
      mockConfig.app = {path: '/tmp/odac-test'}

      mockRunApp.mockResolvedValue(true)

      // Use check() to trigger run for the existing active app
      await App.check()

      // Check the arguments passed to Container.runApp
      expect(mockRunApp).toHaveBeenCalled()
      const args = mockRunApp.mock.calls[0][1] // Second arg is options object

      // Verify Env Injection
      expect(args.env).toBeDefined()
      expect(args.env.ODAC_API_KEY).toBe('mock-app-token')
      expect(args.env.ODAC_API_SOCKET).toBe('/odac/api.sock')

      // Verify Volume Mount
      expect(args.volumes).toBeDefined()
      const socketMount = args.volumes.find(v => v.container === '/odac:ro')
      expect(socketMount).toBeDefined()
      expect(socketMount.host).toBe('/tmp/odac-socket')
    })
  })

  describe('delete()', () => {
    test('should call Domain.deleteByApp when an app is deleted', async () => {
      const mockDeleteByApp = jest.fn()
      // Setup Odac.server mock for Domain
      const originalServer = global.Odac.server
      global.Odac.server = jest.fn(module => {
        if (module === 'Domain') {
          return {deleteByApp: mockDeleteByApp}
        }
        return originalServer(module)
      })

      mockConfig.apps = [{id: 1, name: 'delete-me', active: true, type: 'container'}]
      await App.init()

      const result = await App.delete(1)
      expect(result.success).toBe(true)
      expect(mockDeleteByApp).toHaveBeenCalledWith('delete-me')
    })
  })

  describe('git configuration', () => {
    test('should create the git object with repo, branch, and provider', async () => {
      mockConfig.apps = []
      mockConfig.app = {path: '/tmp/odac-test'}

      const config = {
        type: 'git',
        url: 'https://github.com/user/my-repo.git',
        name: 'my-git-app',
        branch: 'develop'
      }

      await App.create(config)

      const {data: apps} = await App.list(true)
      const app = apps.find(a => a.name === 'my-git-app')

      expect(app.git).toBeDefined()
      expect(app.git.repo).toBe('user/my-repo')
      expect(app.git.branch).toBe('develop')
      expect(app.git.provider).toBe('github')
    })

    test('should update the git object during redeploy', async () => {
      const appRecord = {
        id: 1,
        name: 'redeploy-app',
        type: 'git',
        url: 'https://gitlab.com/user/repo.git',
        branch: 'main',
        git: {
          repo: 'user/repo',
          branch: 'main',
          provider: 'gitlab'
        }
      }
      mockConfig.apps = [appRecord]
      mockConfig.app = {path: '/tmp/odac-test'}

      await App.init()

      // Redeploy with new branch
      await App.redeploy({
        container: 'redeploy-app',
        branch: 'feature-abc'
      })

      const {data: apps} = await App.list(true)
      const app = apps.find(a => a.name === 'redeploy-app')

      expect(app.branch).toBe('feature-abc')
      expect(app.git.branch).toBe('feature-abc')
      expect(app.git.repo).toBe('user/repo')
      expect(app.git.provider).toBe('gitlab')
    })
  })
})
