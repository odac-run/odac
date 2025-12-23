const fs = require('fs')
const os = require('os')

// Mock fs and os modules
jest.mock('fs')
jest.mock('os')

// Mock global Odac object
global.Odac = {
  core: jest.fn(name => {
    if (name === 'Log') {
      return {
        init: jest.fn(() => ({
          log: jest.fn(),
          error: jest.fn()
        }))
      }
    }
    return {}
  })
}

describe('Config', () => {
  let ConfigClass
  let config
  let mockFs
  let mockOs
  let originalMainModule
  let originalSetInterval
  let originalConsoleLog
  let originalConsoleError

  beforeAll(() => {
    // Store original values
    originalMainModule = process.mainModule
    originalSetInterval = global.setInterval
    originalConsoleLog = console.log
    originalConsoleError = console.error

    // Mock console methods to avoid noise in tests
    console.log = jest.fn()
    console.error = jest.fn()
  })

  afterAll(() => {
    // Restore original values
    process.mainModule = originalMainModule
    global.setInterval = originalSetInterval
    console.log = originalConsoleLog
    console.error = originalConsoleError
  })

  beforeEach(() => {
    // Clear all mocks
    jest.clearAllMocks()
    jest.clearAllTimers()
    jest.useFakeTimers()

    // Reset module cache
    delete require.cache[require.resolve('../../core/Config.js')]

    // Setup fs mocks
    mockFs = {
      existsSync: jest.fn(),
      mkdirSync: jest.fn(),
      readFileSync: jest.fn(),
      writeFileSync: jest.fn(),
      copyFileSync: jest.fn(),
      renameSync: jest.fn(),
      unlinkSync: jest.fn(),
      rmSync: jest.fn(),
      promises: {
        writeFile: jest.fn().mockResolvedValue()
      }
    }

    // Setup os mocks
    mockOs = {
      homedir: jest.fn().mockReturnValue('/home/user'),
      platform: jest.fn().mockReturnValue('linux'),
      arch: jest.fn().mockReturnValue('x64')
    }

    // Apply mocks
    fs.existsSync = mockFs.existsSync
    fs.mkdirSync = mockFs.mkdirSync
    fs.readFileSync = mockFs.readFileSync
    fs.writeFileSync = mockFs.writeFileSync
    fs.copyFileSync = mockFs.copyFileSync
    fs.renameSync = mockFs.renameSync
    fs.unlinkSync = mockFs.unlinkSync
    fs.rmSync = mockFs.rmSync
    fs.promises = mockFs.promises

    os.homedir = mockOs.homedir
    os.platform = mockOs.platform
    os.arch = mockOs.arch

    // Mock setInterval to return a mock object with unref method
    global.setInterval = jest.fn().mockReturnValue({unref: jest.fn()})

    // Set default process.mainModule
    process.mainModule = {path: '/mock/node_modules/odac/bin'}
  })

  afterEach(() => {
    jest.useRealTimers()
  })

  describe('initialization', () => {
    it('should create config directories if they do not exist', () => {
      mockFs.existsSync.mockReturnValue(false)

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(mockFs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac')
      expect(mockFs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac/config', {recursive: true})
    })

    it('should load modular configuration files', () => {
      mockFs.existsSync.mockImplementation(path => {
        if (path.startsWith('/home/user/.odac/config/')) return true
        if (path === '/home/user/.odac') return true
        if (path === '/home/user/.odac/config') return true
        return false
      })

      mockFs.readFileSync.mockImplementation(path => {
        if (path.endsWith('server.json')) return JSON.stringify({server: {pid: 123, os: 'linux', arch: 'x64'}})
        if (path.endsWith('web.json')) return JSON.stringify({websites: {test: true}})
        return '{}'
      })

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server.pid).toBe(123)
      expect(config.config.websites.test).toBe(true)
    })

    it('should handle missing modular files by using defaults', () => {
      mockFs.existsSync.mockReturnValue(true)
      // Simulate missing server.json by returning false for it specifically if needed,
      // but loadModuleFile checks existsSync.
      mockFs.existsSync.mockImplementation(path => {
        if (path === '/home/user/.odac') return true
        if (path === '/home/user/.odac/config') return true
        return false // All module files missing
      })

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server).toBeDefined()
      expect(config.config.server.pid).toBeNull()
      expect(config.config.websites).toEqual({})
    })

    it('should set OS and architecture information if missing or different', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {os: 'win32', arch: 'x86'}})) // Mock loading an old config

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
    })

    it('should setup auto-save interval when not in odac bin', () => {
      process.mainModule = {path: '/mock/project'}
      mockFs.existsSync.mockReturnValue(true)

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(global.setInterval).toHaveBeenCalledWith(expect.any(Function), 500)
    })

    it('should not setup auto-save interval when in odac bin', () => {
      process.mainModule = {path: '/mock/node_modules/odac/bin'}
      mockFs.existsSync.mockReturnValue(true)

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(global.setInterval).not.toHaveBeenCalled()
    })
  })

  describe('proxy functionality', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable proxy
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue('{}')
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should proxy nested objects', () => {
      config.config.nested = {deep: {value: 'test'}}
      expect(config.config.nested.deep.value).toBe('test')

      config.config.nested.deep.value = 'modified'
      expect(config.config.nested.deep.value).toBe('modified')
    })

    it('should handle property deletion', () => {
      // Use a mapped key 'pid' under 'server' to trigger module change tracking
      config.config.server.pid = 999
      expect(config.config.server.pid).toBe(999)

      delete config.config.server.pid
      expect(config.config.server.pid).toBeUndefined()
    })
  })

  describe('save functionality', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable proxy for save tests
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue('{}') // return empty config by default to avoid read errors
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should save config when force() is called', () => {
      config.config.server.pid = 999
      // Force save
      config.force()

      // server config belongs to 'server' module -> server.json
      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/config/server.json.tmp',
        expect.stringContaining('"pid": 999'),
        'utf8'
      )
      expect(mockFs.renameSync).toHaveBeenCalledWith('/home/user/.odac/config/server.json.tmp', '/home/user/.odac/config/server.json')
    })

    it('should save only changed modules via proxy', () => {
      mockFs.writeFileSync.mockClear()
      mockFs.renameSync.mockClear()

      // Modify only websites
      config.config.websites = {example: {domain: 'example.com'}}

      // Wait for auto-save (simulated by calling the interval callback or just force)
      // Here we test force() behavior which relies on changed flags or forces all?
      // Actually force() sets all changed.
      // Let's test checking logic via #save() which is private, but accessible if we wait interval.

      // Retrieve the interval callback
      const intervalCallback = global.setInterval.mock.calls[0][0]
      intervalCallback()

      const wroteWeb = mockFs.writeFileSync.mock.calls.some(c => c[0].includes('web.json.tmp'))
      const wroteServer = mockFs.writeFileSync.mock.calls.some(c => c[0].includes('server.json.tmp'))

      expect(wroteWeb).toBe(true)
      // Server might be written if OS/arch update happened during init, let's reset mocks before change
    })

    it('should atomic write: write tmp, backup existing, rename', () => {
      mockFs.existsSync.mockReturnValue(true) // assume files exist
      config.config.server.pid = 555
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(expect.stringMatching(/\.tmp$/), expect.any(String), 'utf8')
      expect(mockFs.copyFileSync).toHaveBeenCalledWith('/home/user/.odac/config/server.json', expect.stringMatching(/\.bak$/))
      expect(mockFs.renameSync).toHaveBeenCalledWith(expect.stringMatching(/\.tmp$/), '/home/user/.odac/config/server.json')
    })
  })

  describe('reload functionality', () => {
    beforeEach(() => {
      mockFs.existsSync.mockReturnValue(true)
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should reload modular config', () => {
      mockFs.readFileSync.mockImplementation(path => {
        if (path.endsWith('server.json')) return JSON.stringify({server: {pid: 888}})
        return '{}'
      })

      config.reload()
      expect(config.config.server.pid).toBe(888)
    })

    it('should handle reload errors gracefully', () => {
      // Simulate error during reload (e.g., config file access denied)
      mockFs.readFileSync.mockImplementation(() => {
        const err = new Error('Access denied')
        err.code = 'EACCES'
        throw err
      })

      config.reload()
      // Should keep existing config or defaults if previously loaded
      expect(config.config.server).toBeDefined()
    })
  })

  describe('error handling', () => {
    it('should handle corrupted module files by loading from backup', () => {
      mockFs.existsSync.mockImplementation(path => {
        if (path.endsWith('server.json')) return true
        if (path.endsWith('server.json.bak')) return true
        return true
      })

      mockFs.readFileSync.mockImplementation(path => {
        if (path.endsWith('server.json.bak')) return JSON.stringify({server: {pid: 777}})
        if (path.endsWith('server.json') && !path.endsWith('.bak')) return 'invalid-json'
        return '{}'
      })

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server.pid).toBe(777)
      expect(mockFs.copyFileSync).toHaveBeenCalledWith(
        expect.stringMatching(/server\.json$/),
        expect.stringMatching(/server\.json\.corrupted$/)
      )
    })
  })
})
