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
  platform: jest.fn().mockReturnValue('linux'),
  homedir: jest.fn().mockReturnValue('/home/user')
}

const mockAxios = {
  post: jest.fn().mockResolvedValue({status: 200, data: {}})
}

// Apply mocks
jest.mock('fs', () => mockFs)
jest.mock('child_process', () => mockChildProcess)
jest.mock('os', () => mockOs)
jest.mock('axios', () => mockAxios)
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
    delete global.Odac
    delete global.__
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
      expect(mockAxios.post).toHaveBeenCalled()
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
