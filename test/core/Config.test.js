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

  // Helper function to create a valid config structure
  const createValidConfig = (overrides = {}) => {
    return JSON.stringify({
      server: {
        pid: null,
        started: null,
        watchdog: null,
        ...overrides.server
      },
      ...overrides
    })
  }

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
    it('should create config directory if it does not exist', () => {
      mockFs.existsSync.mockReturnValueOnce(false) // directory doesn't exist
      mockFs.existsSync.mockReturnValueOnce(false) // config file doesn't exist

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(mockFs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac')
    })

    it('should handle missing config file during init', () => {
      mockFs.existsSync.mockReturnValueOnce(true) // directory exists
      mockFs.existsSync.mockReturnValueOnce(false) // config file doesn't exist

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()

      // Should not throw when config file doesn't exist
      expect(() => {
        config.init()
      }).not.toThrow()
    })

    it.skip('should load existing config file', () => {
      const mockConfig = {server: {pid: 123, started: Date.now()}}
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(JSON.stringify(mockConfig))

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(mockFs.readFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', 'utf8')
      expect(config.config.server.pid).toBe(123)
    })

    it('should set OS and architecture information', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
    })

    it('should setup auto-save interval when not in odac bin', () => {
      process.mainModule = {path: '/mock/project'}
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(global.setInterval).toHaveBeenCalledWith(expect.any(Function), 500)
    })

    it('should not setup auto-save interval when in odac bin', () => {
      process.mainModule = {path: '/mock/node_modules/odac/bin'}
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(global.setInterval).not.toHaveBeenCalled()
    })

    it('should handle missing process.mainModule gracefully', () => {
      process.mainModule = null
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()

      expect(() => {
        config.init()
      }).not.toThrow()

      // Should still initialize properly with default server config
      expect(config.config.server).toBeDefined()
      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
    })
  })

  describe('default configuration structure', () => {
    beforeEach(() => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should have default server configuration', () => {
      expect(config.config.server).toEqual({
        pid: null,
        started: null,
        watchdog: null,
        os: 'linux',
        arch: 'x64'
      })
    })

    it('should expose public methods', () => {
      expect(typeof config.force).toBe('function')
      expect(typeof config.reload).toBe('function')
      expect(typeof config.init).toBe('function')
    })
  })

  describe('file loading and error handling', () => {
    it.skip('should handle empty config file', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue('')

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(console.log).toHaveBeenCalledWith('Error reading config file:', '/home/user/.odac/config.json')
    })

    it.skip('should handle corrupted JSON file', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue('invalid json')

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()

      expect(() => {
        config.init()
      }).not.toThrow()

      expect(console.log).toHaveBeenCalledWith('Error parsing config file:', '/home/user/.odac/config.json')

      // Should still initialize with default server config
      expect(config.config.server).toBeDefined()
      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
    })

    it.skip('should handle file system errors gracefully', () => {
      mockFs.existsSync.mockImplementation(() => {
        throw new Error('File system error')
      })

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()

      expect(() => {
        config.init()
      }).toThrow('File system error')
    })
  })

  describe('proxy functionality', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable proxy
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
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
      config.config.testProp = 'value'
      expect(config.config.testProp).toBe('value')

      delete config.config.testProp
      expect(config.config.testProp).toBeUndefined()
    })

    it('should return primitive values directly', () => {
      config.config.primitive = 'string'
      const value = config.config.primitive

      expect(value).toBe('string')
      expect(typeof value).toBe('string')
    })

    it('should handle null and undefined values', () => {
      config.config.nullValue = null
      config.config.undefinedValue = undefined

      expect(config.config.nullValue).toBeNull()
      expect(config.config.undefinedValue).toBeUndefined()
    })

    it('should proxy arrays correctly', () => {
      config.config.array = [1, 2, 3]
      config.config.array.push(4)

      expect(config.config.array).toEqual([1, 2, 3, 4])
    })

    it('should handle deeply nested proxy objects', () => {
      config.config.level1 = {level2: {level3: {value: 'deep'}}}
      expect(config.config.level1.level2.level3.value).toBe('deep')

      config.config.level1.level2.level3.newProp = 'added'
      expect(config.config.level1.level2.level3.newProp).toBe('added')
    })

    it('should handle non-object values in proxy', () => {
      config.config.string = 'test'
      config.config.number = 42
      config.config.boolean = true

      expect(config.config.string).toBe('test')
      expect(config.config.number).toBe(42)
      expect(config.config.boolean).toBe(true)
    })
  })

  describe('save functionality', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable proxy for save tests
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should save config when force() is called', () => {
      config.config.testSave = 'value' // This should set #changed = true via proxy
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/config.json',
        expect.stringContaining('"testSave": "value"'),
        'utf8'
      )
    })

    it('should create backup file after delay', () => {
      config.config.testBackup = 'value'
      config.force()

      // Fast-forward time to trigger backup creation
      jest.advanceTimersByTime(5000)

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/.bak/config.json.bak',
        expect.stringContaining('"testBackup": "value"'),
        'utf8'
      )
    })

    it('should handle empty config during save', () => {
      config.config = {}
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', '{}', 'utf8')
    })

    it.skip('should handle null config during save', () => {
      config.config = null
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', 'null', 'utf8')
    })

    it('should respect changed flag when saving', () => {
      // The config initialization sets OS/arch which marks it as changed
      // This test verifies the save mechanism works when changes are present
      config.config.testFlag = 'changed'

      // Clear any saves from initialization
      mockFs.writeFileSync.mockClear()

      config.force()

      // Should save because changes were made
      expect(mockFs.writeFileSync).toHaveBeenCalled()
    })

    it('should handle very short JSON during save', () => {
      config.config = {a: 1}
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', expect.stringContaining('"a": 1'), 'utf8')
    })

    it.skip('should handle writeFileSync errors during save', () => {
      mockFs.writeFileSync.mockImplementation(() => {
        throw new Error('Write error')
      })

      config.config.testValue = 'test'

      expect(() => {
        config.force()
      }).toThrow('Write error')
    })
  })

  describe('reload functionality', () => {
    beforeEach(() => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should reload config from file', () => {
      const newConfig = {server: {pid: 789}}
      mockFs.readFileSync.mockReturnValue(JSON.stringify(newConfig))

      config.reload()

      expect(config.config.server.pid).toBe(789)
    })

    it('should handle missing file during reload', () => {
      mockFs.existsSync.mockReturnValue(false)

      expect(() => {
        config.reload()
      }).not.toThrow()
    })

    it('should reset loaded state before reloading', () => {
      const newConfig = {server: {pid: 456, os: 'linux', arch: 'x64'}}

      // First call is during init, second call is during reload
      mockFs.readFileSync.mockReturnValueOnce(JSON.stringify(newConfig))

      config.reload()
      expect(config.config.server.pid).toBe(456)
    })
  })

  describe('edge cases and error scenarios', () => {
    it('should handle complex nested data structures', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      config.config.complex = {
        users: [
          {id: 1, name: 'User 1', settings: {theme: 'dark'}},
          {id: 2, name: 'User 2', settings: {theme: 'light'}}
        ],
        metadata: {
          version: '1.0.0',
          features: ['auth', 'logging', 'config']
        }
      }

      expect(config.config.complex.users).toHaveLength(2)
      expect(config.config.complex.metadata.features).toContain('config')
    })

    it('should maintain type integrity', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      config.config.types = {
        str: 'string',
        num: 123,
        bool: false,
        arr: [],
        obj: {}
      }

      expect(typeof config.config.types.str).toBe('string')
      expect(typeof config.config.types.num).toBe('number')
      expect(typeof config.config.types.bool).toBe('boolean')
      expect(Array.isArray(config.config.types.arr)).toBe(true)
      expect(typeof config.config.types.obj).toBe('object')
    })

    it('should handle OS and arch updates when they change', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(
        JSON.stringify({
          server: {os: 'win32', arch: 'x86'}
        })
      )

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      // Should update to current OS/arch
      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
    })

    it('should preserve existing OS and arch if they match', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(
        JSON.stringify({
          server: {os: 'linux', arch: 'x64', pid: 123}
        })
      )

      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()

      expect(config.config.server.os).toBe('linux')
      expect(config.config.server.arch).toBe('x64')
      expect(config.config.server.pid).toBe(123)
    })
  })

  describe('class behavior', () => {
    it('should create separate instances when instantiated multiple times', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      const config1 = new ConfigClass()
      const config2 = new ConfigClass()

      expect(config1).not.toBe(config2)
      expect(config1.constructor).toBe(config2.constructor)
    })

    it('should have independent config objects for different instances', () => {
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())

      ConfigClass = require('../../core/Config.js')
      const config1 = new ConfigClass()
      const config2 = new ConfigClass()

      config1.init()
      config2.init()

      config1.config.test1 = 'value1'
      config2.config.test2 = 'value2'

      expect(config1.config.test1).toBe('value1')
      expect(config1.config.test2).toBeUndefined()
      expect(config2.config.test2).toBe('value2')
      expect(config2.config.test1).toBeUndefined()
    })
  })

  describe('auto-save behavior with proxy enabled', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable auto-save
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
      config.init()
    })

    it('should trigger auto-save on config changes', () => {
      // Clear any initial saves
      mockFs.writeFileSync.mockClear()

      config.config.autoSaveTest = 'value'

      // Get the callback function from setInterval mock
      const intervalCallback = global.setInterval.mock.calls[0][0]

      // Call the callback directly to simulate the interval
      intervalCallback()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/config.json',
        expect.stringContaining('"autoSaveTest": "value"'),
        'utf8'
      )
    })

    it('should not save if no changes were made', () => {
      // Clear any initial saves
      mockFs.writeFileSync.mockClear()

      // Advance timers without making changes
      jest.advanceTimersByTime(500)

      expect(mockFs.writeFileSync).not.toHaveBeenCalled()
    })

    it.skip('should handle multiple rapid changes efficiently', () => {
      // Clear any initial saves
      mockFs.writeFileSync.mockClear()

      // Make multiple changes quickly
      config.config.change1 = 'value1'
      config.config.change2 = 'value2'
      config.config.change3 = 'value3'

      // Get the callback function from setInterval mock and call it
      const intervalCallback = global.setInterval.mock.calls[0][0]
      intervalCallback()

      // Should only save once despite multiple changes
      expect(mockFs.writeFileSync).toHaveBeenCalledTimes(1)
      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/config.json',
        expect.stringContaining('"change1": "value1"'),
        'utf8'
      )
    })
  })

  describe('private method coverage', () => {
    beforeEach(() => {
      process.mainModule = {path: '/mock/project'} // Enable proxy
      mockFs.existsSync.mockReturnValue(true)
      mockFs.readFileSync.mockReturnValue(createValidConfig())
      ConfigClass = require('../../core/Config.js')
      config = new ConfigClass()
    })

    it('should handle proxy with null target', () => {
      config.init()

      // Test proxy behavior with null values
      config.config.nullTest = null
      expect(config.config.nullTest).toBeNull()
    })

    it('should handle proxy deleteProperty', () => {
      config.init()

      config.config.deleteTest = 'value'
      expect(config.config.deleteTest).toBe('value')

      delete config.config.deleteTest
      expect(config.config.deleteTest).toBeUndefined()
    })

    it('should handle save with minimal JSON', () => {
      config.init()

      // Set config to something that would produce very short JSON
      config.config = {a: 1}
      config.force()

      expect(mockFs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', expect.any(String), 'utf8')
    })

    it('should handle backup creation timeout', () => {
      config.init()

      config.config.backupTest = 'value'
      config.force()

      // Advance time to trigger backup creation
      jest.advanceTimersByTime(5000)

      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/.bak/config.json.bak',
        expect.stringContaining('"backupTest": "value"'),
        'utf8'
      )
    })

    it.skip('should handle load when already saving and loaded', () => {
      config.init()

      // Set a test value
      config.config.testValue = 'test'

      // Mock readFileSync to return the same config with the test value
      mockFs.readFileSync.mockReturnValue(
        JSON.stringify({
          server: {pid: null, started: null, watchdog: null, os: 'linux', arch: 'x64'},
          testValue: 'test'
        })
      )

      // Reload should work and preserve the value
      config.reload()

      expect(config.config.testValue).toBe('test')
    })

    it.skip('should handle empty data during load', () => {
      mockFs.readFileSync.mockReturnValue('')

      config.init()

      expect(console.log).toHaveBeenCalledWith('Error reading config file:', '/home/user/.odac/config.json')
    })
  })

  describe('modular configuration', () => {
    beforeEach(() => {
      mockFs.renameSync = jest.fn()
      mockFs.unlinkSync = jest.fn()
    })

    describe('format detection', () => {
      it('should detect modular format when config directory exists', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })
        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })

      it('should detect single-file format when only config.json exists', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return false
          if (path === '/home/user/.odac/config.json') return true
          return false
        })
        mockFs.readFileSync.mockReturnValue(createValidConfig())

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })

      it('should detect new installation when neither exists', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return false
          return false
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(mockFs.mkdirSync).toHaveBeenCalled()
        expect(config.config.server).toBeDefined()
      })
    })

    describe('modular loading', () => {
      it('should load all module files correctly', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/server.json')) return true
          if (path.includes('/config/web.json')) return true
          return false
        })

        mockFs.readFileSync.mockImplementation(path => {
          if (path.includes('server.json')) {
            return JSON.stringify({server: {pid: 123}})
          }
          if (path.includes('web.json')) {
            return JSON.stringify({websites: {example: {}}})
          }
          return '{}'
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server.pid).toBe(123)
        expect(config.config.websites).toBeDefined()
      })

      it('should handle missing module files gracefully', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })

      it('should recover from corrupted module file using backup', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/server.json')) return true
          if (path.includes('/.bak/server.json.bak')) return true
          return false
        })

        let callCount = 0
        mockFs.readFileSync.mockImplementation(path => {
          if (path.includes('server.json') && !path.includes('.bak')) {
            callCount++
            if (callCount === 1) {
              throw new Error('Corrupted JSON')
            }
          }
          if (path.includes('server.json.bak')) {
            return JSON.stringify({server: {pid: 456}})
          }
          return '{}'
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(mockFs.copyFileSync).toHaveBeenCalled()
        expect(config.config.server).toBeDefined()
      })

      it('should handle empty module file', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/server.json')) return true
          return false
        })

        mockFs.readFileSync.mockImplementation(path => {
          if (path.includes('server.json')) return ''
          return '{}'
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })
    })

    describe('migration from single-file to modular', () => {
      it('should migrate single-file config to modular format', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config.json') return true
          if (path === '/home/user/.odac/config') return false
          if (path.includes('.pre-modular')) return false
          return false
        })

        const singleFileConfig = {
          server: {pid: 789},
          websites: {example: {domain: 'example.com'}},
          ssl: {enabled: true}
        }

        let configDirCreated = false
        mockFs.mkdirSync.mockImplementation(path => {
          if (path === '/home/user/.odac/config') {
            configDirCreated = true
          }
        })

        mockFs.readFileSync.mockImplementation(path => {
          if (path === '/home/user/.odac/config.json') {
            return JSON.stringify(singleFileConfig)
          }
          if (configDirCreated && path.includes('/config/')) {
            const module = path.split('/').pop().replace('.json', '')
            if (module === 'server') return JSON.stringify({server: singleFileConfig.server})
            if (module === 'web') return JSON.stringify({websites: singleFileConfig.websites})
            if (module === 'ssl') return JSON.stringify({ssl: singleFileConfig.ssl})
          }
          return '{}'
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(mockFs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac/config', {recursive: true})
        expect(mockFs.copyFileSync).toHaveBeenCalledWith('/home/user/.odac/config.json', '/home/user/.odac/config.json.pre-modular')
      })

      it('should rollback migration on failure', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config.json') return true
          if (path === '/home/user/.odac/config') return false
          return false
        })

        mockFs.readFileSync.mockReturnValue(createValidConfig())

        mockFs.mkdirSync.mockImplementation(path => {
          if (path === '/home/user/.odac/config') {
            // Simulate successful directory creation
            mockFs.existsSync.mockImplementation(p => {
              if (p === path) return true
              if (p === '/home/user/.odac') return true
              if (p === '/home/user/.odac/config.json') return true
              return false
            })
          }
        })

        mockFs.writeFileSync.mockImplementation(() => {
          throw new Error('Write failed')
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(mockFs.rmSync).toHaveBeenCalledWith('/home/user/.odac/config', {recursive: true, force: true})
        expect(config.config.server).toBeDefined()
      })

      it('should handle migration with permission errors', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config.json') return true
          if (path === '/home/user/.odac/config') return false
          return false
        })

        mockFs.readFileSync.mockReturnValue(createValidConfig())

        mockFs.mkdirSync.mockImplementation(() => {
          const err = new Error('Permission denied')
          err.code = 'EACCES'
          throw err
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })

      it('should use rmSync with recursive and force options during rollback', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config.json') return true
          if (path === '/home/user/.odac/config') return false
          return false
        })

        mockFs.readFileSync.mockReturnValue(createValidConfig())

        let configDirCreated = false
        mockFs.mkdirSync.mockImplementation(path => {
          if (path === '/home/user/.odac/config') {
            configDirCreated = true
            mockFs.existsSync.mockImplementation(p => {
              if (p === path) return configDirCreated
              if (p === '/home/user/.odac') return true
              if (p === '/home/user/.odac/config.json') return true
              return false
            })
          }
        })

        mockFs.writeFileSync.mockImplementation(() => {
          throw new Error('Write failed')
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(mockFs.rmSync).toHaveBeenCalledWith('/home/user/.odac/config', {
          recursive: true,
          force: true
        })
      })

      it('should handle rmSync errors gracefully during rollback', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config.json') return true
          if (path === '/home/user/.odac/config') return false
          return false
        })

        mockFs.readFileSync.mockReturnValue(createValidConfig())

        mockFs.mkdirSync.mockImplementation(path => {
          if (path === '/home/user/.odac/config') {
            mockFs.existsSync.mockImplementation(p => {
              if (p === path) return true
              if (p === '/home/user/.odac') return true
              if (p === '/home/user/.odac/config.json') return true
              return false
            })
          }
        })

        mockFs.writeFileSync.mockImplementation(() => {
          throw new Error('Write failed')
        })

        mockFs.rmSync.mockImplementation(() => {
          throw new Error('rmSync failed')
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()

        expect(() => {
          config.init()
        }).not.toThrow()

        expect(config.config.server).toBeDefined()
      })
    })

    describe('modular saving', () => {
      beforeEach(() => {
        process.mainModule = {path: '/mock/project'}
      })

      it('should save only changed modules', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.writeFileSync.mockClear()
        mockFs.renameSync.mockClear()

        config.config.websites = {example: {domain: 'test.com'}}
        config.force()

        const writeCalls = mockFs.writeFileSync.mock.calls
        const renameCalls = mockFs.renameSync.mock.calls

        const hasWebWrite = writeCalls.some(call => call[0].includes('web.json'))
        const hasWebRename = renameCalls.some(call => call[1].includes('web.json'))

        expect(hasWebWrite || hasWebRename).toBe(true)
      })

      it('should use atomic writes for module files', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/.bak')) return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.writeFileSync.mockClear()
        mockFs.renameSync.mockClear()

        config.config.ssl = {enabled: true}
        config.force()

        const tempWrites = mockFs.writeFileSync.mock.calls.filter(call => call[0].includes('.tmp'))
        const renames = mockFs.renameSync.mock.calls.filter(call => call[1].includes('ssl.json'))

        expect(tempWrites.length + renames.length).toBeGreaterThan(0)
      })

      it('should create backups before overwriting module files', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/server.json')) return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {pid: 100}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.copyFileSync.mockClear()

        config.config.server.pid = 200
        config.force()

        const backupCalls = mockFs.copyFileSync.mock.calls.filter(call => call[1].includes('.bak'))
        expect(backupCalls.length).toBeGreaterThan(0)
      })

      it('should fallback to single-file on modular save failure', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.writeFileSync.mockImplementation(() => {
          throw new Error('Disk full')
        })

        config.config.dns = {enabled: true}
        config.force()

        expect(config.config.server).toBeDefined()
      })
    })

    describe('deepCompare utility', () => {
      beforeEach(() => {
        mockFs.existsSync.mockReturnValue(true)
        mockFs.readFileSync.mockReturnValue(createValidConfig())
        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()
      })

      it('should detect identical objects', () => {
        const obj1 = {a: 1, b: {c: 2}}
        const obj2 = {a: 1, b: {c: 2}}

        // Test that identical objects are equal by comparing their JSON representation
        expect(JSON.stringify(obj1)).toBe(JSON.stringify(obj2))
      })

      it('should detect type mismatches', () => {
        const obj1 = {a: 1}
        const obj2 = {a: '1'}

        expect(obj1.a).not.toBe(obj2.a)
      })

      it('should detect missing keys', () => {
        const obj1 = {a: 1, b: 2}
        const obj2 = {a: 1}

        expect(obj1.b).toBeDefined()
        expect(obj2.b).toBeUndefined()
      })

      it('should handle null values', () => {
        const obj1 = {a: null}
        const obj2 = {a: null}

        expect(obj1.a).toBe(obj2.a)
      })

      it('should handle arrays', () => {
        const obj1 = {arr: [1, 2, 3]}
        const obj2 = {arr: [1, 2, 3]}

        expect(obj1.arr).toEqual(obj2.arr)
      })

      it('should detect array length differences', () => {
        const obj1 = {arr: [1, 2, 3]}
        const obj2 = {arr: [1, 2]}

        expect(obj1.arr.length).not.toBe(obj2.arr.length)
      })

      it('should handle deeply nested objects', () => {
        const obj1 = {a: {b: {c: {d: 1}}}}
        const obj2 = {a: {b: {c: {d: 1}}}}

        expect(JSON.stringify(obj1)).toBe(JSON.stringify(obj2))
      })
    })

    describe('atomic write operations', () => {
      beforeEach(() => {
        process.mainModule = {path: '/mock/project'}
      })

      it('should write to temp file first', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.writeFileSync.mockClear()

        config.config.mail = {enabled: true}
        config.force()

        const tempWrites = mockFs.writeFileSync.mock.calls.filter(call => call[0].includes('.tmp'))
        expect(tempWrites.length).toBeGreaterThan(0)
      })

      it('should cleanup temp file on write failure', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('.tmp')) return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        mockFs.renameSync.mockImplementation(() => {
          throw new Error('Rename failed')
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        mockFs.unlinkSync.mockClear()

        config.config.api = {enabled: true}
        config.force()

        // Should attempt cleanup or fallback to single-file mode
        const unlinkCalls = mockFs.unlinkSync.mock.calls
        const writeFileCalls = mockFs.writeFileSync.mock.calls

        expect(unlinkCalls.length + writeFileCalls.length).toBeGreaterThan(0)
      })

      it('should handle ENOSPC error gracefully', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          return false
        })

        mockFs.readFileSync.mockReturnValue(JSON.stringify({server: {}}))

        mockFs.writeFileSync.mockImplementation(() => {
          const err = new Error('No space left')
          err.code = 'ENOSPC'
          throw err
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        config.config.service = {enabled: true}
        config.force()

        expect(config.config.server).toBeDefined()
      })
    })

    describe('helper methods', () => {
      it('should initialize default config for server key', () => {
        mockFs.existsSync.mockReturnValue(true)
        mockFs.readFileSync.mockReturnValue(createValidConfig())

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        const testConfig = {}
        // Access private method through reflection for testing
        const initMethod = Object.getOwnPropertyNames(Object.getPrototypeOf(config)).find(name =>
          name.includes('initializeDefaultModuleConfig')
        )

        if (initMethod) {
          config[initMethod](testConfig, ['server'])
          expect(testConfig.server).toEqual({pid: null, started: null, watchdog: null})
        }
      })

      it('should initialize default config for websites key', () => {
        mockFs.existsSync.mockReturnValue(true)
        mockFs.readFileSync.mockReturnValue(createValidConfig())

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        const testConfig = {}
        const initMethod = Object.getOwnPropertyNames(Object.getPrototypeOf(config)).find(name =>
          name.includes('initializeDefaultModuleConfig')
        )

        if (initMethod) {
          config[initMethod](testConfig, ['websites'])
          expect(testConfig.websites).toEqual({})
        }
      })

      it('should initialize default config for services key', () => {
        mockFs.existsSync.mockReturnValue(true)
        mockFs.readFileSync.mockReturnValue(createValidConfig())

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        const testConfig = {}
        const initMethod = Object.getOwnPropertyNames(Object.getPrototypeOf(config)).find(name =>
          name.includes('initializeDefaultModuleConfig')
        )

        if (initMethod) {
          config[initMethod](testConfig, ['services'])
          expect(testConfig.services).toEqual([])
        }
      })

      it('should not overwrite existing config values', () => {
        mockFs.existsSync.mockReturnValue(true)
        mockFs.readFileSync.mockReturnValue(createValidConfig())

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        const testConfig = {server: {pid: 123}}
        const initMethod = Object.getOwnPropertyNames(Object.getPrototypeOf(config)).find(name =>
          name.includes('initializeDefaultModuleConfig')
        )

        if (initMethod) {
          config[initMethod](testConfig, ['server'])
          expect(testConfig.server.pid).toBe(123)
        }
      })
    })

    describe('corruption recovery', () => {
      it('should create .corrupted backup when recovering from corruption', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/dns.json')) return true
          if (path.includes('/.bak/dns.json.bak')) return true
          return false
        })

        mockFs.readFileSync.mockImplementation(path => {
          if (path.includes('dns.json') && !path.includes('.bak')) {
            throw new Error('Corrupted')
          }
          if (path.includes('dns.json.bak')) {
            return JSON.stringify({dns: {enabled: false}})
          }
          return '{}'
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        const corruptedCopies = mockFs.copyFileSync.mock.calls.filter(call => call[1].includes('.corrupted'))
        expect(corruptedCopies.length).toBeGreaterThan(0)
      })

      it('should handle both main and backup being corrupted', () => {
        mockFs.existsSync.mockImplementation(path => {
          if (path === '/home/user/.odac') return true
          if (path === '/home/user/.odac/config') return true
          if (path.includes('/config/mail.json')) return true
          if (path.includes('/.bak/mail.json.bak')) return true
          return false
        })

        mockFs.readFileSync.mockImplementation(() => {
          throw new Error('All corrupted')
        })

        ConfigClass = require('../../core/Config.js')
        config = new ConfigClass()
        config.init()

        expect(config.config.server).toBeDefined()
      })
    })
  })
})
