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

// Mock fs to prevent actual file writes
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

describe('deviceAdd', () => {
  beforeEach(async () => {
    mockConfig.apps = [{id: 1, name: 'test-app', devices: []}]
    await App.init()
    jest.clearAllMocks()
  })

  test('should add a new device to an existing app', () => {
    const result = App.deviceAdd(1, '/dev/ttyACM0')

    expect(result.success).toBe(true)
    const app = mockConfig.apps.find(a => a.id === 1)
    expect(app.devices).toHaveLength(1)
    expect(app.devices[0]).toEqual({host: '/dev/ttyACM0', container: '/dev/ttyACM0'})
  })

  test('should add a device with a custom container path', () => {
    const result = App.deviceAdd(1, '/dev/ttyACM0', '/dev/arduino')

    expect(result.success).toBe(true)
    const app = mockConfig.apps.find(a => a.id === 1)
    expect(app.devices[0]).toEqual({host: '/dev/ttyACM0', container: '/dev/arduino'})
  })

  test('should update existing device mapping if host path already exists', () => {
    App.deviceAdd(1, '/dev/ttyACM0', '/dev/old')
    const result = App.deviceAdd(1, '/dev/ttyACM0', '/dev/new')

    expect(result.success).toBe(true)
    const app = mockConfig.apps.find(a => a.id === 1)
    expect(app.devices).toHaveLength(1)
    expect(app.devices[0].container).toBe('/dev/new')
  })

  test('should return error if app is not found', () => {
    const result = App.deviceAdd(999, '/dev/ttyACM0')
    expect(result.success).toBe(false)
    expect(result.message).toMatch(/not found/)
  })

  test('should return error if host path is missing', () => {
    const result = App.deviceAdd(1, null)
    expect(result.success).toBe(false)
    expect(result.message).toMatch(/Missing host/)
  })
})
