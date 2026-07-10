/**
 * Unit tests for Proxy.js module
 * Tests Go Proxy process management and configuration synchronization
 */

// Define mocks before requiring anything
const mockFs = {
  existsSync: jest.fn().mockReturnValue(true),
  readFileSync: jest.fn().mockReturnValue('12345'),
  mkdirSync: jest.fn(),
  openSync: jest.fn().mockReturnValue(1),
  writeFileSync: jest.fn(),
  unlinkSync: jest.fn()
}

const mockChildProcess = {
  spawn: jest.fn().mockReturnValue({
    pid: 12345,
    unref: jest.fn(),
    on: jest.fn(),
    kill: jest.fn()
  })
}

const mockOs = {
  freemem: jest.fn().mockReturnValue(4 * 1024 * 1024 * 1024),
  platform: jest.fn().mockReturnValue('linux'),
  homedir: jest.fn().mockReturnValue('/home/user'),
  totalmem: jest.fn().mockReturnValue(8 * 1024 * 1024 * 1024)
}

// Apply mocks
jest.mock('fs', () => mockFs)
jest.mock('child_process', () => mockChildProcess)
jest.mock('os', () => mockOs)
const {mockOdac} = require('./__mocks__/globalOdac')

describe('Proxy', () => {
  let ProxyService
  let mockConfig
  let mockLog

  beforeEach(() => {
    jest.clearAllMocks()
    mockOdac.resetMocks()
    global.Odac = mockOdac

    mockConfig = mockOdac.core('Config')
    mockConfig.config = {
      domains: {},
      app: {path: '/var/odac'},
      firewall: {enabled: true},
      ssl: null
    }

    mockLog = jest.fn()
    mockOdac.setMock('core', 'Http', {
      delete: jest.fn().mockResolvedValue({status: 200, data: {}}),
      get: jest.fn(),
      post: jest.fn().mockResolvedValue({status: 200, data: {}})
    })
    mockOdac.setMock('core', 'Log', {
      init: () => ({log: mockLog, error: mockLog})
    })
    mockOdac.setMock('core', 'Process', {
      stop: jest.fn()
    })

    global.__ = jest.fn(s => s)

    // Default return for fs
    mockFs.existsSync.mockReturnValue(true)
    mockFs.readFileSync.mockReturnValue('12345')

    jest.isolateModules(() => {
      ProxyService = require('../../server/src/Proxy')
    })
  })

  afterEach(() => {
    if (ProxyService) ProxyService.stop()
  })

  describe('proxy management', () => {
    test('should spawn proxy process if not running', () => {
      mockFs.readFileSync.mockImplementation(() => {
        throw {code: 'ENOENT'}
      })

      ProxyService.spawnProxy()
      expect(mockChildProcess.spawn).toHaveBeenCalled()
    })

    test('should sync config to proxy', async () => {
      // Sync requires proxy process to exist and bypass socket check or provide socket
      mockFs.readFileSync.mockImplementation(() => {
        throw {code: 'ENOENT'}
      })
      ProxyService.spawnProxy()

      await ProxyService.syncConfig()
      expect(mockOdac.core('Http').post).toHaveBeenCalled()
    })

    test('should stop proxy process', () => {
      // Create a specific child mock to check kill
      const currentChild = {pid: 12345, unref: jest.fn(), on: jest.fn(), kill: jest.fn()}
      mockChildProcess.spawn.mockReturnValue(currentChild)

      mockFs.readFileSync.mockImplementation(() => {
        throw {code: 'ENOENT'}
      })
      ProxyService.spawnProxy()
      ProxyService.stop()

      expect(currentChild.kill).toHaveBeenCalled()
    })
  })

  describe('backend resolution', () => {
    /** Syncs one domain bound to `app` and returns the routing the proxy was handed. */
    const routingFor = async ports => {
      mockConfig.config.apps = [{id: 1, name: 'web', ports}]
      mockConfig.config.domains = {'a.com': {appId: 'web'}}
      mockOdac.setMock('server', 'Container', {available: true, getIP: jest.fn().mockResolvedValue('172.17.0.5')})

      mockFs.readFileSync.mockImplementation(() => {
        throw {code: 'ENOENT'}
      })
      ProxyService.spawnProxy()
      await ProxyService.syncConfig()

      const post = mockOdac.core('Http').post.mock.calls.find(c => c[0].includes('/config'))
      return post[1].domains['a.com']
    }

    test('routes a proxy entry over the container network', async () => {
      const routing = await routingFor([{host: 'proxy', container: 3000}])

      expect(routing.port).toBe(3000)
      expect(routing.containerIP).toBe('172.17.0.5')
    })

    test('routes a published-only app over the host network', async () => {
      const routing = await routingFor([{host: 8080, container: 3000}])

      expect(routing.port).toBe(8080)
      expect(routing.containerIP).toBe('127.0.0.1')
    })

    test('prefers the proxy entry over a published one that precedes it', async () => {
      // The sentinel declares who owns routing; list order must not decide it.
      const routing = await routingFor([
        {host: 8080, container: 3000},
        {host: 'proxy', container: 3000}
      ])

      expect(routing.port).toBe(3000)
      expect(routing.containerIP).toBe('172.17.0.5')
    })
  })

  describe('lifecycle', () => {
    test('should spawn proxy in check if active', async () => {
      ProxyService.start()
      mockFs.readFileSync.mockImplementation(() => {
        throw {code: 'ENOENT'}
      })

      ProxyService.check()
      expect(mockChildProcess.spawn).toHaveBeenCalled()
    })

    test('should not spawn proxy in check if not active', () => {
      ProxyService.check()
      expect(mockChildProcess.spawn).not.toHaveBeenCalled()
    })
  })
})
