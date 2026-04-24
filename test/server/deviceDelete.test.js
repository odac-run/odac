const path = require('path')

// Stable mock config reference
let mockConfig = {
  app: {path: '/var/odac/apps'},
  apps: []
}

// Mock Odac and dependencies
global.__ = msg => msg
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
    if (module === 'Api') {
      return {
        result: jest.fn((result, message, data) => ({result, success: result, message, data}))
      }
    }
    return {}
  })
}

// Mock fs
jest.mock('fs', () => ({
  existsSync: jest.fn(() => true),
  mkdirSync: jest.fn(),
  promises: {
    mkdir: jest.fn().mockResolvedValue(true),
    access: jest.fn().mockResolvedValue(true),
    readdir: jest.fn().mockResolvedValue([])
  }
}))

const App = require('../../server/src/App')

describe('deviceDelete', () => {
  beforeEach(async () => {
    mockConfig.apps = [
      {
        id: 1,
        name: 'test-app',
        devices: [
          {host: '/dev/ttyACM0', container: '/dev/ttyACM0'},
          {host: '/dev/ttyUSB0', container: '/dev/ttyUSB0'}
        ]
      }
    ]
    await App.init()
    jest.clearAllMocks()
  })

  test('should remove a device mapping from an existing app', () => {
    const result = App.deviceDelete(1, '/dev/ttyACM0')

    expect(result.success).toBe(true)
    const app = mockConfig.apps.find(a => a.id === 1)
    expect(app.devices).toHaveLength(1)
    expect(app.devices[0].host).toBe('/dev/ttyUSB0')
  })

  test('should handle app with no devices', () => {
    const app = mockConfig.apps.find(a => a.id === 1)
    delete app.devices

    const result = App.deviceDelete(1, '/dev/ttyACM0')
    expect(result.success).toBe(true)
  })

  test('should return success even if device does not exist', () => {
    const result = App.deviceDelete(1, '/dev/nonexistent')
    expect(result.success).toBe(true)
    const app = mockConfig.apps.find(a => a.id === 1)
    expect(app.devices).toHaveLength(2)
  })

  test('should return error if app is not found', () => {
    const result = App.deviceDelete(999, '/dev/ttyACM0')
    expect(result.success).toBe(false)
    expect(result.message).toMatch(/not found/)
  })
})
