jest.mock('fs', () => ({
  existsSync: jest.fn(() => true),
  mkdirSync: jest.fn(),
  rmSync: jest.fn(),
  promises: {
    mkdir: jest.fn().mockResolvedValue(true),
    rm: jest.fn().mockResolvedValue(true),
    access: jest.fn().mockResolvedValue(true),
    readdir: jest.fn().mockResolvedValue([]),
    stat: jest.fn().mockResolvedValue({mtimeMs: 0})
  },
  createWriteStream: jest.fn(() => ({
    write: jest.fn(),
    end: jest.fn(),
    on: jest.fn(),
    once: jest.fn(),
    emit: jest.fn(),
    removeListener: jest.fn()
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
            getStatus: jest.fn(() => Promise.resolve({running: false, restarts: 0})),
            remove: jest.fn(),
            fetchRepo: jest.fn(),
            getImageExposedPorts: jest.fn(() => []),
            logs: jest.fn().mockResolvedValue({}),
            docker: {
              getContainer: jest.fn(() => ({
                modem: {
                  demuxStream: jest.fn()
                }
              }))
            }
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

      const result = await App.create(config)
      if (!result.success) {
        throw new Error(`App.create failed: ${result.message}`)
      }
      expect(result.success).toBe(true)

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

  describe('environment management', () => {
    beforeEach(() => {
      mockConfig.apps = [
        {
          id: 1,
          name: 'app-main',
          type: 'container',
          env: {
            manual: {
              NODE_ENV: 'production',
              API_KEY: 'secret-key-123',
              DB_PASS: 'password123'
            },
            linked: []
          }
        },
        {
          id: 2,
          name: 'app-db',
          type: 'container',
          env: {
            manual: {
              POSTGRES_USER: 'admin',
              POSTGRES_PASSWORD: 'db-secret-password'
            }
          }
        },
        {
          id: 3,
          name: 'app-legacy',
          type: 'container',
          env: {
            LEGACY_VAR: 'old-value'
          }
        }
      ]
      return App.init()
    })

    test('getEnv should return structured format with masked values', async () => {
      const result = App.getEnv('app-main')
      expect(result.success).toBe(true)

      // Manual envs
      expect(result.data.manual.NODE_ENV).toBe('production')
      expect(result.data.manual.API_KEY).toBe('***')
      expect(result.data.manual.DB_PASS).toBe('***')

      // Linked section (empty initially)
      expect(result.data.linked).toEqual([])
    })

    test('setEnv should merge new values and migrate legacy structure', async () => {
      // Test Legacy Migration
      const resLegacy = App.setEnv('app-legacy', {NEW_VAR: 'new-value'})
      expect(resLegacy.success).toBe(true)

      const appLegacy = mockConfig.apps.find(a => a.name === 'app-legacy')
      expect(appLegacy.env.manual).toBeDefined() // Migrated
      expect(appLegacy.env.manual.LEGACY_VAR).toBe('old-value')
      expect(appLegacy.env.manual.NEW_VAR).toBe('new-value')

      // Test Normal Merge
      const resMain = App.setEnv('app-main', {NODE_ENV: 'development', EXTRA: 'foo'})
      expect(resMain.success).toBe(true)

      const appMain = mockConfig.apps.find(a => a.name === 'app-main')
      expect(appMain.env.manual.NODE_ENV).toBe('development') // Updated
      expect(appMain.env.manual.API_KEY).toBe('secret-key-123') // Preserved
      expect(appMain.env.manual.EXTRA).toBe('foo') // Added
    })

    test('deleteEnv should remove specified keys', async () => {
      const result = App.deleteEnv('app-main', ['API_KEY', 'NON_EXISTENT'])
      expect(result.success).toBe(true)

      const app = mockConfig.apps.find(a => a.name === 'app-main')
      expect(app.env.manual.API_KEY).toBeUndefined()
      expect(app.env.manual.NODE_ENV).toBe('production')
    })

    test('linkEnv should validate and link apps', async () => {
      // Self-link fail
      const selfRes = App.linkEnv('app-main', 'app-main')
      expect(selfRes.success).toBe(false)
      expect(selfRes.message).toMatch(/itself/)

      // Convert legacy struct before linking? linkEnv handles it.
      // Link app-db to app-main
      const linkRes = App.linkEnv('app-main', 'app-db')
      expect(linkRes.success).toBe(true)

      const app = mockConfig.apps.find(a => a.name === 'app-main')
      expect(app.env.linked).toContain('app-db')

      // Resolve check: linked section should contain app-db's envs
      const resolvedRes = App.getEnv('app-main')
      expect(resolvedRes.data.linked).toHaveLength(1)
      expect(resolvedRes.data.linked[0].app).toBe('app-db')
      expect(resolvedRes.data.linked[0].env.POSTGRES_USER).toBe('admin')
      expect(resolvedRes.data.linked[0].env.POSTGRES_PASSWORD).toBe('***')
    })

    test('unlinkEnv should remove link', async () => {
      // Setup: Link first
      App.linkEnv('app-main', 'app-db')

      const unlinkRes = App.unlinkEnv('app-main', 'app-db')
      expect(unlinkRes.success).toBe(true)

      const app = mockConfig.apps.find(a => a.name === 'app-main')
      expect(app.env.linked).not.toContain('app-db')
    })
  })
})
