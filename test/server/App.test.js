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

// Auto-discovery probes container ports over HTTP. `httpPorts` names the ports
// that answer; every other port refuses the connection, the way a database does.
jest.mock('http', () => {
  const {EventEmitter} = require('events')

  const mock = {
    httpPorts: new Set(),
    request(options, onResponse) {
      const req = new EventEmitter()
      req.destroy = jest.fn()
      req.end = () => {
        // nextTick, not setImmediate: the discovery tests run on fake timers, which
        // flush microtasks between ticks but never turn the event loop's check phase.
        process.nextTick(() => {
          if (mock.httpPorts.has(options.port)) {
            onResponse({destroy: jest.fn()})
          } else {
            req.emit('error', new Error('ECONNREFUSED'))
          }
        })
      }

      return req
    }
  }

  return mock
})

const http = require('http')

let App

describe('App', () => {
  let mockConfig
  let mockRunApp
  let mockCloneRepo
  let mockGetListeningPorts
  let mockGetIP
  let mockGetImageExposedPorts

  beforeEach(() => {
    // Reset config for each test
    mockConfig = {}
    mockRunApp = jest.fn()
    mockCloneRepo = jest.fn()
    mockGetListeningPorts = jest.fn(() => [3000]) // Default: pass the readiness probe
    mockGetIP = jest.fn(() => '10.0.0.5') // Mock IP to prevent infinite loop
    mockGetImageExposedPorts = jest.fn(() => [])
    http.httpPorts = new Set()

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
        if (module === 'Ports') {
          return require('../../server/src/Ports')
        }
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
            getIP: mockGetIP,
            getListeningPorts: mockGetListeningPorts,
            remove: jest.fn(),
            fetchRepo: jest.fn(),
            getImageExposedPorts: mockGetImageExposedPorts,
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
          syncConfig: jest.fn(),
          purgeCacheForApp: jest.fn()
        }
      })
    }

    jest.isolateModules(() => {
      App = require('../../server/src/App')
    })
  })

  afterEach(() => {
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

  describe('privileged access', () => {
    test('setPrivileged defaults to root mode and persists the flag', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-app', type: 'container'}]
      await App.init()

      const result = App.setPrivileged('priv-app')
      expect(result.success).toBe(true)
      expect(mockConfig.apps[0].privileged).toBe('root')
    })

    test('setPrivileged accepts full mode', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-app', type: 'container'}]
      await App.init()

      App.setPrivileged('priv-app', 'full')
      expect(mockConfig.apps[0].privileged).toBe('full')
    })

    test('setPrivileged off removes the flag', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-app', type: 'container', privileged: 'full'}]
      await App.init()

      const result = App.setPrivileged('priv-app', 'off')
      expect(result.success).toBe(true)
      expect(mockConfig.apps[0].privileged).toBeUndefined()
    })

    test('setPrivileged rejects an invalid mode without touching the app', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-app', type: 'container'}]
      await App.init()

      const result = App.setPrivileged('priv-app', 'superuser')
      expect(result.success).toBe(false)
      expect(mockConfig.apps[0].privileged).toBeUndefined()
    })

    test('setPrivileged returns an error for an unknown app', async () => {
      mockConfig.apps = []
      await App.init()

      const result = App.setPrivileged('ghost')
      expect(result.success).toBe(false)
    })

    test('full privileged app runs with Docker Privileged mode and root user', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-full', active: true, type: 'container', image: 'test:latest', privileged: 'full'}]
      mockConfig.app = {path: '/tmp/odac-test'}
      mockRunApp.mockResolvedValue(true)

      await App.check()

      expect(mockRunApp).toHaveBeenCalled()
      const opts = mockRunApp.mock.calls[0][1]
      expect(opts.privileged).toBe(true)
      expect(opts.user).toBe('root')
    })

    test('root privileged app runs as root without Docker Privileged mode', async () => {
      mockConfig.apps = [{id: 1, name: 'priv-root', active: true, type: 'container', image: 'test:latest', privileged: 'root'}]
      mockConfig.app = {path: '/tmp/odac-test'}
      mockRunApp.mockResolvedValue(true)

      await App.check()

      const opts = mockRunApp.mock.calls[0][1]
      expect(opts.user).toBe('root')
      expect(opts.privileged).toBeUndefined()
    })

    test('non-privileged app runs without any elevation', async () => {
      mockConfig.apps = [{id: 1, name: 'plain-app', active: true, type: 'container', image: 'test:latest'}]
      mockConfig.app = {path: '/tmp/odac-test'}
      mockRunApp.mockResolvedValue(true)

      await App.check()

      const opts = mockRunApp.mock.calls[0][1]
      expect(opts.user).toBeUndefined()
      expect(opts.privileged).toBeUndefined()
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
      expect(result.data.manual.DB_PASS).toBe('password123') // pass is not masked

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
      expect(resolvedRes.data.linked[0].env.POSTGRES_PASSWORD).toBe('db-secret-password')
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

    test('should resolve container directive in env to container name', async () => {
      mockGetApp.mockResolvedValue({
        name: 'container-ref',
        apps: {
          db: {
            image: 'postgres:alpine',
            env: {
              POSTGRES_PASSWORD: {generate: true},
              SERVICE_NAME: {type: 'container'}
            }
          },
          web: {
            image: 'node:lts',
            linked: ['db'],
            env: {
              APP_NAME: {type: 'container'},
              DB_HOST: '${db.name}'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'container-ref', name: 'mystack'})
      expect(result.success).toBe(true)

      // DB container: SERVICE_NAME should resolve to the DB container name
      const dbCall = mockRunApp.mock.calls[0][1]
      expect(dbCall.env.SERVICE_NAME).toBe('mystack-db')

      // Web container: APP_NAME should resolve to the web container name
      const webCall = mockRunApp.mock.calls[1][1]
      expect(webCall.env.APP_NAME).toBe('mystack-web')
    })

    test('should resolve container directive in single-app recipe', async () => {
      mockGetApp.mockResolvedValue({
        name: 'redis',
        image: 'redis:alpine',
        env: {
          HOSTNAME: {type: 'container'},
          MODE: 'standalone'
        }
      })

      const result = await App.create({type: 'app', app: 'redis', name: 'myredis'})
      expect(result.success).toBe(true)

      const runCall = mockRunApp.mock.calls[0][1]
      expect(runCall.env.HOSTNAME).toBe('myredis')
      expect(runCall.env.MODE).toBe('standalone')
    })

    test('should support explicit type field in env directives', async () => {
      mockGetApp.mockResolvedValue({
        name: 'typed-stack',
        apps: {
          db: {
            image: 'postgres:alpine',
            env: {
              CONTAINER_NAME: {type: 'container'},
              DB_PASS: {type: 'generate', length: 24},
              LEGACY_PASS: {generate: true, length: 12},
              STATIC_VAR: 'hello'
            }
          }
        }
      })

      const result = await App.create({type: 'app', app: 'typed-stack', name: 'mytyped'})
      expect(result.success).toBe(true)

      const dbCall = mockRunApp.mock.calls[0][1]
      expect(dbCall.env.CONTAINER_NAME).toBe('mytyped-db')
      expect(dbCall.env.DB_PASS).toHaveLength(24)
      expect(dbCall.env.LEGACY_PASS).toHaveLength(12)
      expect(dbCall.env.STATIC_VAR).toBe('hello')
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

    test('should not let an echoed-back generate directive overwrite the generated password (single-app recipe)', async () => {
      // The UI receives the recipe's raw env shape and may send it straight back
      // as config.env when creating the app. If that raw directive object isn't
      // resolved before merging, it clobbers the already-generated password with
      // "[object Object]" instead of a real value.
      const recipeEnv = {
        MARIADB_DATABASE: 'odac',
        MARIADB_HOST: {type: 'container'},
        MARIADB_ROOT_PASSWORD: {length: 16, generate: true},
        MARIADB_USER: 'root'
      }
      mockGetApp.mockResolvedValue({
        name: 'mariadb',
        image: 'mariadb:latest',
        env: recipeEnv
      })

      const result = await App.create({
        type: 'app',
        app: 'mariadb',
        name: 'mymariadb',
        env: recipeEnv
      })
      expect(result.success).toBe(true)

      const call = mockRunApp.mock.calls[0][1]
      expect(call.env.MARIADB_ROOT_PASSWORD).not.toBe('[object Object]')
      expect(typeof call.env.MARIADB_ROOT_PASSWORD).toBe('string')
      expect(call.env.MARIADB_ROOT_PASSWORD).toHaveLength(16)
      expect(call.env.MARIADB_HOST).toBe('mymariadb')
    })

    test('should not let an echoed-back generate directive overwrite the generated password (template)', async () => {
      mockGetApp.mockResolvedValue({
        name: 'stack',
        apps: {
          db: {
            image: 'mariadb:latest',
            env: {
              DB_PASSWORD: {generate: true, length: 16}
            }
          }
        }
      })

      const result = await App.create({
        type: 'app',
        app: 'stack',
        name: 'mystack',
        env: {db: {DB_PASSWORD: {generate: true, length: 16}}}
      })
      expect(result.success).toBe(true)

      const call = mockRunApp.mock.calls[0][1]
      expect(call.env.DB_PASSWORD).not.toBe('[object Object]')
      expect(typeof call.env.DB_PASSWORD).toBe('string')
      expect(call.env.DB_PASSWORD).toHaveLength(16)
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

  describe('setPorts()', () => {
    beforeEach(() => {
      mockConfig.apps = [{id: 1, name: 'web', type: 'container', image: 'nginx', ports: [{host: 'proxy', container: 3000}]}]
    })

    const ports = () => mockConfig.apps[0].ports

    test('should reject an unknown app', async () => {
      const result = await App.setPorts('ghost', [{host: 'proxy', container: 3000}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/not found/)
    })

    test('should reject a non-array payload', async () => {
      const result = await App.setPorts('web', {host: 'proxy', container: 3000})
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Expected an array/)
    })

    test('should reject an entry with a missing container port', async () => {
      const result = await App.setPorts('web', [{host: 8080}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/must have a container port/)
    })

    test('should reject an entry with an empty container port', async () => {
      const result = await App.setPorts('web', [{host: 8080, container: ''}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/must have a container port/)
    })

    test('should reject an entry with a missing host', async () => {
      const result = await App.setPorts('web', [{container: 3000}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/must have a host port/)
    })

    test('should reject an entry with an empty host', async () => {
      const result = await App.setPorts('web', [{host: '', container: 3000}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/must have a host port/)
    })

    test('should reject an out-of-range host port', async () => {
      const result = await App.setPorts('web', [{host: 70000, container: 3000}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Invalid host port/)
    })

    test('should reject an out-of-range container port', async () => {
      const result = await App.setPorts('web', [{host: 8080, container: 0}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Invalid container port/)
    })

    test('should reject a garbage host value', async () => {
      const result = await App.setPorts('web', [{host: 'internal', container: 3000}])
      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Invalid host port/)
    })

    test('should not mutate the app when validation fails', async () => {
      await App.setPorts('web', [{container: 3000}])
      expect(ports()).toEqual([{host: 'proxy', container: 3000}])
    })

    test('should accept the proxy sentinel and keep the port off Docker', async () => {
      const result = await App.setPorts('web', [{host: 'proxy', container: 8000}])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([{host: 'proxy', container: 8000}])
    })

    test('should accept a published host binding', async () => {
      const result = await App.setPorts('web', [{host: 8080, container: 3000}])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([{host: 8080, container: 3000}])
    })

    test('should coerce string ports to numbers so readiness probes compare correctly', async () => {
      await App.setPorts('web', [{host: '8080', container: '3000'}])

      expect(ports()).toEqual([{host: 8080, container: 3000}])
    })

    test('should resolve an auto host port', async () => {
      await App.setPorts('web', [{host: 'auto', container: 3000}])

      expect(ports()[0].host).toBeGreaterThanOrEqual(30000)
      expect(ports()[0].container).toBe(3000)
    })

    test('should support mixing published and proxy-routed entries', async () => {
      const result = await App.setPorts('web', [
        {host: 'proxy', container: 3000},
        {host: 9000, container: 9000}
      ])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([
        {host: 'proxy', container: 3000},
        {host: 9000, container: 9000}
      ])
    })

    test('should reject a second proxy-routed entry', async () => {
      const result = await App.setPorts('web', [
        {host: 'proxy', container: 3000},
        {host: 'proxy', container: 4000}
      ])

      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Only one port may be routed by the proxy/)
    })

    test('should accept one container port published on two host ports', async () => {
      const result = await App.setPorts('web', [
        {host: 8080, container: 3000},
        {host: 9090, container: 3000}
      ])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([
        {host: 8080, container: 3000},
        {host: 9090, container: 3000}
      ])
    })

    test('should accept the same container port routed by the proxy and published', async () => {
      // The proxy reaches the container over the docker network; publishing DNATs
      // a host port to the same listener. The two paths never collide.
      const result = await App.setPorts('web', [
        {host: 'proxy', container: 3000},
        {host: 8080, container: 3000}
      ])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([
        {host: 'proxy', container: 3000},
        {host: 8080, container: 3000}
      ])
    })

    test('should reject duplicate host ports', async () => {
      const result = await App.setPorts('web', [
        {host: 8080, container: 3000},
        {host: 8080, container: 4000}
      ])

      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Duplicate host port/)
    })

    test('should resolve two auto host ports to distinct ports', async () => {
      const result = await App.setPorts('web', [
        {host: 'auto', container: 3000},
        {host: 'auto', container: 4000}
      ])

      expect(result.success).toBe(true)
      expect(ports()[0].host).not.toBe(ports()[1].host)
    })

    test('should not leave the app mutated when a collision is rejected', async () => {
      await App.setPorts('web', [
        {host: 'proxy', container: 3000},
        {host: 'proxy', container: 4000}
      ])

      expect(ports()).toEqual([{host: 'proxy', container: 3000}])
    })

    // Hub delivers commands as JSON over WebSocket, so every value arrives as a
    // string. app.port.set forwards payload.ports to setPorts verbatim.
    test('should accept a Hub-shaped payload where every value is a string', async () => {
      const payload = JSON.parse('{"name":"web","ports":[{"host":"proxy","container":"3000"},{"host":"8080","container":"8080"}]}')

      const result = await App.setPorts(payload.name, payload.ports)

      expect(result.success).toBe(true)
      expect(ports()).toEqual([
        {host: 'proxy', container: 3000},
        {host: 8080, container: 8080}
      ])
    })

    test('should reject a Hub payload with a missing ports key rather than throwing', async () => {
      const payload = JSON.parse('{"name":"web"}')

      const result = await App.setPorts(payload.name, payload.ports)

      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Expected an array/)
    })

    test('should persist the public flag on a published port', async () => {
      const result = await App.setPorts('web', [{host: 3307, container: 3306, public: true}])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([{host: 3307, container: 3306, public: true}])
    })

    test('should omit the public flag when the port stays on loopback', async () => {
      // An absent flag already means loopback; writing `public: false` into every
      // stored config would churn them for nothing.
      const result = await App.setPorts('web', [
        {host: 3307, container: 3306, public: false},
        {host: 8080, container: 8080}
      ])

      expect(result.success).toBe(true)
      expect(ports()).toEqual([
        {host: 3307, container: 3306},
        {host: 8080, container: 8080}
      ])
    })

    test('should mark an auto-assigned host port public', async () => {
      const result = await App.setPorts('web', [{host: 'auto', container: 3306, public: true}])

      expect(result.success).toBe(true)
      expect(ports()[0].public).toBe(true)
      expect(typeof ports()[0].host).toBe('number')
    })

    test('should reject a public proxy port', async () => {
      const result = await App.setPorts('web', [{host: 'proxy', container: 3000, public: true}])

      expect(result.success).toBe(false)
      expect(result.message).toMatch(/cannot be public/)
    })

    test('should reject a public flag that is not a boolean', async () => {
      const result = await App.setPorts('web', [{host: 3307, container: 3306, public: 'yes'}])

      expect(result.success).toBe(false)
      expect(result.message).toMatch(/Invalid public flag/)
    })

    test('should not leave the app mutated when the public flag is rejected', async () => {
      await App.setPorts('web', [{host: 3307, container: 3306, public: 1}])

      expect(ports()).toEqual([{host: 'proxy', container: 3000}])
    })

    // The dashboard checkbox may serialize as a string on its way through Hub.
    test('should accept a Hub-shaped public flag sent as a string', async () => {
      const payload = JSON.parse('{"name":"web","ports":[{"host":"3307","container":"3306","public":"true"}]}')

      const result = await App.setPorts(payload.name, payload.ports)

      expect(result.success).toBe(true)
      expect(ports()).toEqual([{host: 3307, container: 3306, public: true}])
    })

    test('should not read a stringified false as public', async () => {
      const payload = JSON.parse('{"name":"web","ports":[{"host":"3307","container":"3306","public":"false"}]}')

      const result = await App.setPorts(payload.name, payload.ports)

      expect(result.success).toBe(true)
      expect(ports()[0].public).toBeUndefined()
    })
  })

  describe('legacy port migration', () => {
    test('should stamp the proxy sentinel onto host-less entries on load', async () => {
      mockConfig.apps = [{id: 1, name: 'web', type: 'container', ports: [{container: 3000}]}]

      await App.init()

      const {data: apps} = await App.list(true)
      expect(apps[0].ports[0]).toEqual({host: 'proxy', container: 3000, auto: true})
    })

    test('should let auto-discovery correct a migrated legacy entry', async () => {
      // Before the marker existed these were correctable; migration must not
      // quietly freeze them on a port the app never binds.
      mockConfig.apps = [{id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{container: 3000}]}]

      await App.init()
      // Name-scoped for the same reason as the auto-discovery runApp helper:
      // a stale poller from an earlier test must not see an HTTP-answering port.
      mockGetListeningPorts.mockImplementation(async name => (name === 'web' ? [5678] : []))
      http.httpPorts = new Set([5678])

      jest.useFakeTimers({doNotFake: ['setImmediate', 'nextTick']})
      try {
        await App.check()
        await jest.advanceTimersByTimeAsync(7000)
      } finally {
        jest.useRealTimers()
      }

      expect(mockConfig.apps[0].ports[0]).toEqual({host: 'proxy', container: 5678, auto: true})
    })

    test('should leave published entries alone on load', async () => {
      mockConfig.apps = [{id: 1, name: 'web', type: 'container', ports: [{host: 8080, container: 3000}]}]

      await App.init()

      const {data: apps} = await App.list(true)
      expect(apps[0].ports[0]).toEqual({host: 8080, container: 3000})
    })

    test('should tolerate apps with no ports at all', async () => {
      mockConfig.apps = [{id: 1, name: 'web', type: 'script'}]

      await expect(App.init()).resolves.not.toThrow()
    })
  })

  describe('runtime port auto-discovery', () => {
    // #pollForPort waits 5 attempts (1s apart) before rewriting the config.
    const POLL_SETTLE_MS = 7000

    // A listening port answers the HTTP probe unless the test says otherwise.
    const runApp = async (app, listeningPorts, httpPorts = listeningPorts) => {
      mockConfig.apps = [app]
      // Scoped to this app's name: earlier tests leave #pollForPort chains armed
      // on real 1s timers (apps stay portless until the probe confirms, so their
      // pollers survive 5+ attempts), and under full-suite load one can fire
      // mid-test. Handing it listening ports would let it adopt one and clobber
      // this test's config via #saveApps; an empty answer keeps it idle.
      mockGetListeningPorts.mockImplementation(async name => (name === app.name ? listeningPorts : []))
      http.httpPorts = new Set(httpPorts)

      // #pollForPort re-arms itself with setTimeout; fake them so the 5-attempt
      // grace period is instant. setImmediate stays real — check() awaits one.
      jest.useFakeTimers({doNotFake: ['setImmediate', 'nextTick']})
      try {
        await App.check()
        await jest.advanceTimersByTimeAsync(POLL_SETTLE_MS)
      } finally {
        jest.useRealTimers()
      }

      return mockConfig.apps[0].ports
    }

    test('should adopt the observed port for a guessed proxy entry', async () => {
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 3000, auto: true}]},
        [5678]
      )

      expect(ports).toEqual([{host: 'proxy', container: 5678, auto: true}])
    })

    test('should keep correcting a guess it already corrected once', async () => {
      // The entry stays a guess after a correction, or a wrong first guess would
      // freeze the app on a port it never binds.
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 5678, auto: true}]},
        [8080]
      )

      expect(ports).toEqual([{host: 'proxy', container: 8080, auto: true}])
    })

    test('should not clobber a published host binding when the app listens elsewhere', async () => {
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 8080, container: 3000}]},
        [5678]
      )

      expect(ports).toEqual([{host: 8080, container: 3000}])
    })

    test('should preserve secondary mappings when correcting the primary entry', async () => {
      const ports = await runApp(
        {
          id: 1,
          name: 'web',
          type: 'container',
          image: 'n8n',
          active: true,
          ports: [
            {host: 'proxy', container: 3000, auto: true},
            {host: 9000, container: 9000}
          ]
        },
        [5678]
      )

      expect(ports).toEqual([
        {host: 'proxy', container: 5678, auto: true},
        {host: 9000, container: 9000}
      ])
    })

    test('should correct the proxy entry wherever it sits in the list', async () => {
      const ports = await runApp(
        {
          id: 1,
          name: 'web',
          type: 'container',
          image: 'n8n',
          active: true,
          ports: [
            {host: 9000, container: 9000},
            {host: 'proxy', container: 3000, auto: true}
          ]
        },
        [5678]
      )

      expect(ports).toEqual([
        {host: 9000, container: 9000},
        {host: 'proxy', container: 5678, auto: true}
      ])
    })

    test('should leave the config alone when the app listens where expected', async () => {
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 3000}]},
        [3000]
      )

      expect(ports).toEqual([{host: 'proxy', container: 3000}])
    })

    test('should honour a user-chosen proxy port the app actually listens on', async () => {
      // User moved the proxy port 3000 -> 5000 and restarted; the app binds both.
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 5000}]},
        [3000, 5000]
      )

      expect(ports).toEqual([{host: 'proxy', container: 5000}])
    })

    test('should not overwrite a user-chosen proxy port the app never binds', async () => {
      // The choice is unroutable, but it is the user's. Silently healing it would
      // undo the port they just saved from the dashboard on the next restart.
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 5000}]},
        [3000]
      )

      expect(ports).toEqual([{host: 'proxy', container: 5000}])
    })

    test('should not adopt a port for an entry a recipe declared', async () => {
      // Recipe ports go through #preparePorts, which never stamps `auto`.
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'n8n', active: true, ports: [{host: 'proxy', container: 3000}]},
        [5678]
      )

      expect(ports).toEqual([{host: 'proxy', container: 3000}])
    })

    test('should not adopt a lone port that does not speak HTTP', async () => {
      // A database app binds 5432 and nothing else. Adopting it would hand the
      // proxy a backend that can never serve a request, so the guess stands.
      const ports = await runApp(
        {id: 1, name: 'db', type: 'container', image: 'postgres', active: true, ports: [{host: 'proxy', container: 3000, auto: true}]},
        [5432],
        []
      )

      expect(ports).toEqual([{host: 'proxy', container: 3000, auto: true}])
    })

    test('should adopt the HTTP port when the app also listens on a database port', async () => {
      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'app', active: true, ports: [{host: 'proxy', container: 3000, auto: true}]},
        [5432, 8000],
        [8000]
      )

      expect(ports).toEqual([{host: 'proxy', container: 8000, auto: true}])
    })

    test('should keep polling until a listening port starts answering HTTP', async () => {
      // The app binds its socket before it serves, so the first probes fail.
      mockConfig.apps = [
        {id: 1, name: 'web', type: 'container', image: 'app', active: true, ports: [{host: 'proxy', container: 3000, auto: true}]}
      ]
      mockGetListeningPorts.mockResolvedValue([5678])
      http.httpPorts = new Set()

      jest.useFakeTimers({doNotFake: ['setImmediate', 'nextTick']})
      try {
        await App.check()
        await jest.advanceTimersByTimeAsync(POLL_SETTLE_MS)

        expect(mockConfig.apps[0].ports).toEqual([{host: 'proxy', container: 3000, auto: true}])

        http.httpPorts = new Set([5678])
        await jest.advanceTimersByTimeAsync(POLL_SETTLE_MS)
      } finally {
        jest.useRealTimers()
      }

      expect(mockConfig.apps[0].ports).toEqual([{host: 'proxy', container: 5678, auto: true}])
    })

    test('should fall back to a well-known port when the container IP is unknown', async () => {
      // No IP means no probe. Guessing beats leaving the app unroutable.
      mockGetIP.mockResolvedValue(null)

      const ports = await runApp(
        {id: 1, name: 'web', type: 'container', image: 'app', active: true, ports: [{host: 'proxy', container: 3000, auto: true}]},
        [5432, 8080],
        []
      )

      expect(ports).toEqual([{host: 'proxy', container: 8080, auto: true}])
    })

    test('should give a database app no port mapping at all', async () => {
      // A recipe that ships no ports leaves the app with none. MariaDB EXPOSEs 3306
      // and binds it, but nothing there speaks HTTP, so the proxy has no business
      // claiming it — and a `proxy: 3306` row the proxy can never route is worse
      // than no row.
      mockGetImageExposedPorts.mockResolvedValue([3306])

      const ports = await runApp({id: 1, name: 'db', type: 'container', image: 'mariadb', active: true}, [3306], [])

      expect(ports).toBeUndefined()
      expect(mockRunApp.mock.calls[0][1].env.PORT).toBe('3306')
    })

    test('should write the proxy mapping for a portless app once a port answers HTTP', async () => {
      const ports = await runApp({id: 1, name: 'web', type: 'container', image: 'app', active: true}, [8080], [8080])

      expect(ports).toEqual([{host: 'proxy', container: 8080, auto: true}])
    })

    test('should not write a mapping from the image EXPOSE alone', async () => {
      // EXPOSE is metadata, not evidence: the mapping waits for the probe.
      mockGetImageExposedPorts.mockResolvedValue([8080])

      const ports = await runApp({id: 1, name: 'web', type: 'container', image: 'app', active: true}, [], [])

      expect(ports).toBeUndefined()
    })

    test('should hand Docker only the published ports', async () => {
      await runApp(
        {
          id: 1,
          name: 'web',
          type: 'container',
          image: 'nginx',
          active: true,
          ports: [
            {host: 'proxy', container: 3000},
            {host: 9000, container: 9000}
          ]
        },
        [3000]
      )

      const runOptions = mockRunApp.mock.calls[0][1]
      expect(runOptions.ports).toEqual([{host: 9000, container: 9000}])
    })

    test('should carry the public flag through to Docker', async () => {
      await runApp(
        {
          id: 1,
          name: 'db',
          type: 'container',
          image: 'mysql',
          active: true,
          ports: [{host: 3307, container: 3306, public: true}]
        },
        [3306]
      )

      const runOptions = mockRunApp.mock.calls[0][1]
      expect(runOptions.ports).toEqual([{host: 3307, container: 3306, public: true}])
    })
  })
})
