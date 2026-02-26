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
            registerBuildLogger: jest.fn(),
            unregisterBuildLogger: jest.fn(),
            build: jest.fn(),
            isRunning: jest.fn(() => false),
            stop: jest.fn(),
            list: jest.fn(() => []),
            getStats: jest.fn(),
            getStatus: jest.fn(() => Promise.resolve({running: false, restarts: 0})),
            getIP: jest.fn(() => '10.0.0.5'), // Mock IP to prevent infinite loop
            getListeningPorts: jest.fn(() => [3000]), // Mock to pass readiness probe
            remove: jest.fn(),
            fetchRepo: jest.fn(),
            getImageExposedPorts: jest.fn(() => []),
            logs: jest.fn().mockResolvedValue({}),
            docker: {
              getContainer: jest.fn(() => ({
                modem: {
                  demuxStream: jest.fn()
                },
                rename: jest.fn(() => Promise.resolve()) // Ensure rename returns a promise
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

  describe('Blue-Green deploy API token identity', () => {
    test('should use _appIdentity for API token instead of container name when present', async () => {
      // Simulate a Blue-Green scenario: app.name is the ephemeral green container name,
      // but _appIdentity preserves the canonical app name for token generation.
      const mockGenerateAppToken = jest.fn(() => 'mock-app-token')

      const originalServer = global.Odac.server
      global.Odac.server = jest.fn(module => {
        if (module === 'Api') {
          return {
            result: jest.fn((result, message, data) => {
              if (typeof message === 'object') {
                data = message
                message = undefined
              }
              return {result, success: result, message, data}
            }),
            generateAppToken: mockGenerateAppToken,
            hostSocketDir: '/tmp/odac-socket'
          }
        }
        return originalServer(module)
      })

      // App with _appIdentity simulates a green container object during ZDD.
      // name = green container name, _appIdentity = original canonical name.
      mockConfig.apps = [
        {
          id: 201,
          name: 'zdd-app-green-build_12345',
          _appIdentity: 'zdd-app',
          active: true,
          type: 'container',
          image: 'test:latest',
          api: ['app.list']
        }
      ]
      mockConfig.app = {path: '/tmp/odac-test'}

      mockRunApp.mockResolvedValue(true)

      // check() triggers #run() -> #runContainer() for the active app
      await App.check()

      // Token MUST be generated with the canonical name, NOT the green container name
      expect(mockGenerateAppToken).toHaveBeenCalled()
      const [tokenAppName, tokenPerms] = mockGenerateAppToken.mock.calls[0]
      expect(tokenAppName).toBe('zdd-app')
      expect(tokenAppName).not.toMatch(/green/)
      expect(tokenPerms).toEqual(['app.list'])
    })

    test('should use app.name for API token when _appIdentity is absent (normal flow)', async () => {
      // Normal flow: no _appIdentity, token should use app.name
      const mockGenerateAppToken = jest.fn(() => 'mock-app-token')

      const originalServer = global.Odac.server
      global.Odac.server = jest.fn(module => {
        if (module === 'Api') {
          return {
            result: jest.fn((result, message, data) => {
              if (typeof message === 'object') {
                data = message
                message = undefined
              }
              return {result, success: result, message, data}
            }),
            generateAppToken: mockGenerateAppToken,
            hostSocketDir: '/tmp/odac-socket'
          }
        }
        return originalServer(module)
      })

      mockConfig.apps = [
        {
          id: 301,
          name: 'normal-app',
          active: true,
          type: 'container',
          image: 'test:latest',
          api: true
        }
      ]
      mockConfig.app = {path: '/tmp/odac-test'}

      mockRunApp.mockResolvedValue(true)

      await App.check()

      expect(mockGenerateAppToken).toHaveBeenCalled()
      const [tokenAppName] = mockGenerateAppToken.mock.calls[0]
      expect(tokenAppName).toBe('normal-app')
    })

    test('should strip _appIdentity from saved config', async () => {
      mockConfig.apps = [
        {
          id: 401,
          name: 'saved-app',
          _appIdentity: 'should-be-removed',
          active: true,
          type: 'container',
          image: 'test:latest'
        }
      ]
      mockConfig.app = {path: '/tmp/odac-test'}

      mockRunApp.mockResolvedValue(true)

      // Trigger a save via check -> run -> set
      await App.check()

      // Verify _appIdentity is NOT persisted in saved config
      const savedApp = mockConfig.apps.find(a => a.name === 'saved-app')
      expect(savedApp._appIdentity).toBeUndefined()
    })
  })

  describe('template deployment', () => {
    let mockGetApp

    beforeEach(() => {
      mockConfig.app = {path: '/tmp/odac-test'}
      mockConfig.apps = []
      mockRunApp.mockResolvedValue(true)

      mockGetApp = jest.fn()

      // Override Odac.server to add Hub.getApp mock
      const originalServer = global.Odac.server
      global.Odac.server = jest.fn(module => {
        if (module === 'Hub') {
          return {
            getApp: mockGetApp,
            trigger: jest.fn()
          }
        }
        return originalServer(module)
      })
    })

    test('should detect template recipe and deploy multi-app stack', async () => {
      mockGetApp.mockResolvedValue({
        name: 'wordpress',
        apps: {
          db: {
            image: 'mariadb:10.6',
            volumes: [{host: 'data', container: '/var/lib/mysql'}],
            env: {
              MYSQL_DATABASE: 'wordpress',
              MYSQL_PASSWORD: {generate: true, length: 16},
              MYSQL_ROOT_PASSWORD: {generate: true, length: 24},
              MYSQL_USER: 'wordpress'
            }
          },
          web: {
            image: 'wordpress:latest',
            ports: [{container: 80, host: 'auto'}],
            volumes: [{host: 'data', container: '/var/www/html'}],
            linked: ['db'],
            env: {
              WORDPRESS_DB_HOST: '${db.name}',
              WORDPRESS_DB_NAME: '${db.env.MYSQL_DATABASE}',
              WORDPRESS_DB_PASSWORD: '${db.env.MYSQL_PASSWORD}',
              WORDPRESS_DB_USER: '${db.env.MYSQL_USER}'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'wordpress', name: 'myblog'})
      expect(result.success).toBe(true)

      // Should have created 2 apps
      const {data: apps} = await App.list(true)
      expect(apps).toHaveLength(2)

      // Check DB app was created with template metadata
      const dbApp = apps.find(a => a.name === 'myblog-db')
      expect(dbApp).toBeDefined()
      expect(dbApp.image).toBe('mariadb:10.6')
      expect(dbApp.template).toEqual({group: 'myblog', name: 'wordpress', role: 'db'})

      // Check Web app was created with template metadata
      const webApp = apps.find(a => a.name === 'myblog-web')
      expect(webApp).toBeDefined()
      expect(webApp.image).toBe('wordpress:latest')
      expect(webApp.template).toEqual({group: 'myblog', name: 'wordpress', role: 'web'})

      // Web should be linked to DB
      expect(webApp.env.linked).toContain('myblog-db')

      // Container.runApp should have been called twice (db first, then web)
      expect(mockRunApp).toHaveBeenCalledTimes(2)
    })

    test('should interpolate template variables correctly', async () => {
      mockGetApp.mockResolvedValue({
        name: 'test-stack',
        apps: {
          service: {
            image: 'test:latest',
            env: {
              SERVICE_NAME: 'myservice',
              SERVICE_SECRET: {generate: true, length: 8}
            }
          },
          client: {
            image: 'client:latest',
            linked: ['service'],
            env: {
              BACKEND_HOST: '${service.name}',
              BACKEND_SECRET: '${service.env.SERVICE_SECRET}'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'test-stack', name: 'mystack'})
      expect(result.success).toBe(true)

      // Verify the interpolated env was passed to Container.runApp
      // Second call = client (dependencies first: service, then client)
      const clientCall = mockRunApp.mock.calls[1][1]
      expect(clientCall.env.BACKEND_HOST).toBe('mystack-service')
      // BACKEND_SECRET should be a generated password, not the template string
      expect(clientCall.env.BACKEND_SECRET).not.toContain('${')
      expect(clientCall.env.BACKEND_SECRET.length).toBe(8)
    })

    test('should resolve dependency order correctly (3-tier stack)', async () => {
      mockGetApp.mockResolvedValue({
        name: 'three-tier',
        apps: {
          cache: {
            image: 'redis:alpine',
            env: {}
          },
          db: {
            image: 'postgres:alpine',
            env: {POSTGRES_PASSWORD: {generate: true}}
          },
          web: {
            image: 'node:lts',
            linked: ['db', 'cache'],
            env: {
              CACHE_HOST: '${cache.name}',
              DB_HOST: '${db.name}'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'three-tier', name: 'myapp'})
      expect(result.success).toBe(true)

      // Container.runApp should be called 3 times
      expect(mockRunApp).toHaveBeenCalledTimes(3)

      // Web must be started LAST (depends on both db and cache)
      const lastCallName = mockRunApp.mock.calls[2][0]
      expect(lastCallName).toBe('myapp-web')
    })

    test('should rollback all apps on partial failure', async () => {
      let callCount = 0
      mockRunApp.mockImplementation(() => {
        callCount++
        // Fail on the second container start (web)
        if (callCount >= 2) throw new Error('Container start failed')
        return Promise.resolve(true)
      })

      mockGetApp.mockResolvedValue({
        name: 'fail-stack',
        apps: {
          db: {
            image: 'db:latest',
            env: {DB_PASS: {generate: true}}
          },
          web: {
            image: 'web:latest',
            linked: ['db'],
            env: {DB_HOST: '${db.name}'}
          }
        }
      })

      const result = await App.create({type: 'app', app: 'fail-stack', name: 'mybad'})
      expect(result.success).toBe(false)

      // All apps should be rolled back — empty list
      const {data: apps} = await App.list(true)
      expect(apps).toHaveLength(0)
    })

    test('should detect circular dependencies', async () => {
      mockGetApp.mockResolvedValue({
        name: 'circular',
        apps: {
          a: {image: 'a:latest', linked: ['b'], env: {}},
          b: {image: 'b:latest', linked: ['a'], env: {}}
        }
      })

      const result = await App.create({type: 'app', app: 'circular', name: 'loop'})
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Circular dependency/)
    })

    test('should detect undefined dependencies', async () => {
      mockGetApp.mockResolvedValue({
        name: 'broken',
        apps: {
          web: {image: 'web:latest', linked: ['nonexistent'], env: {}}
        }
      })

      const result = await App.create({type: 'app', app: 'broken', name: 'nope'})
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/not defined/)
    })

    test('should handle single-app recipe (non-template) normally', async () => {
      // A regular recipe without apps property should NOT trigger template handler
      mockGetApp.mockResolvedValue({
        name: 'redis',
        image: 'redis:alpine',
        ports: [{container: 6379, host: 'auto'}],
        env: {}
      })

      const result = await App.create({type: 'app', app: 'redis', name: 'myredis'})
      expect(result.success).toBe(true)

      const {data: apps} = await App.list(true)
      expect(apps).toHaveLength(1)
      expect(apps[0].name).toBe('myredis')
      expect(apps[0].template).toBeUndefined()
    })

    test('should leave unresolvable template variables as-is', async () => {
      mockGetApp.mockResolvedValue({
        name: 'partial',
        apps: {
          app: {
            image: 'test:latest',
            env: {
              VALID: 'hello',
              BROKEN_REF: '${nonexistent.env.FOO}',
              BROKEN_PATH: '${app.env.NOPE}'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'partial', name: 'mypartial'})
      expect(result.success).toBe(true)

      const call = mockRunApp.mock.calls[0][1]
      expect(call.env.VALID).toBe('hello')
      // Unresolvable vars should remain as-is (no crash)
      expect(call.env.BROKEN_REF).toBe('${nonexistent.env.FOO}')
    })

    test('should apply user env overrides per template member', async () => {
      mockGetApp.mockResolvedValue({
        name: 'override-test',
        apps: {
          db: {
            image: 'db:latest',
            env: {DB_NAME: 'default_db'}
          }
        }
      })

      const result = await App.create({
        type: 'app',
        app: 'override-test',
        name: 'myoverride',
        env: {db: {DB_NAME: 'custom_db'}}
      })
      expect(result.success).toBe(true)

      const call = mockRunApp.mock.calls[0][1]
      expect(call.env.DB_NAME).toBe('custom_db')
    })

    test('should handle direct template payload (type: template) from Hub', async () => {
      // Hub sends the full template data inline with Cloud-provided container names
      const result = await App.create({
        type: 'template',
        name: 'wordpress',
        apps: {
          db: {
            container: 'wordpress-db-a3f2c1',
            image: 'mariadb:10.6',
            env: {
              MYSQL_DATABASE: 'wordpress',
              MYSQL_PASSWORD: {generate: true, length: 16},
              MYSQL_ROOT_PASSWORD: {generate: true, length: 24},
              MYSQL_USER: 'wordpress'
            },
            ports: [],
            volumes: [{host: 'data', container: '/var/lib/mysql'}],
            linked: []
          },
          web: {
            container: 'wordpress-web-a3f2c1',
            image: 'wordpress:latest',
            env: {
              WORDPRESS_DB_HOST: '${db.name}',
              WORDPRESS_DB_NAME: '${db.env.MYSQL_DATABASE}',
              WORDPRESS_DB_PASSWORD: '${db.env.MYSQL_PASSWORD}',
              WORDPRESS_DB_USER: '${db.env.MYSQL_USER}'
            },
            ports: [{container: 80, host: 'auto'}],
            volumes: [{host: 'data', container: '/var/www/html'}],
            linked: ['db']
          }
        }
      })

      expect(result.success).toBe(true)

      // Hub.getApp should NOT have been called — data was inline
      expect(mockGetApp).not.toHaveBeenCalled()

      // Verify both apps were created with exact Cloud-provided names
      const {data: apps} = await App.list(true)
      expect(apps).toHaveLength(2)

      const dbApp = apps.find(a => a.name === 'wordpress-db-a3f2c1')
      const webApp = apps.find(a => a.name === 'wordpress-web-a3f2c1')
      expect(dbApp).toBeDefined()
      expect(webApp).toBeDefined()

      // Web env should have interpolated DB container name
      const webCall = mockRunApp.mock.calls[1][1]
      expect(webCall.env.WORDPRESS_DB_HOST).toBe('wordpress-db-a3f2c1')
      expect(webCall.env.WORDPRESS_DB_NAME).toBe('wordpress')
      expect(webCall.env.WORDPRESS_DB_USER).toBe('wordpress')
      // Password should be generated, not a template string
      expect(webCall.env.WORDPRESS_DB_PASSWORD).not.toContain('${')
      expect(webCall.env.WORDPRESS_DB_PASSWORD.length).toBe(16)
    })

    test('should reject direct template payload with missing apps', async () => {
      const result = await App.create({type: 'template', name: 'empty'})
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/no apps defined/)
    })

    test('should reject direct template payload with missing name', async () => {
      const result = await App.create({
        type: 'template',
        apps: {db: {image: 'db:latest', env: {}}}
      })
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Missing template name/)
    })
  })
})
