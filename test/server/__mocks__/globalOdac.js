/**
 * Comprehensive mock for the global Odac object used in server tests
 * Provides mocked implementations of core(), server(), cli(), and watchdog() methods
 */

class MockOdac {
  constructor() {
    this._registry = new Map()
    this._singletons = new Map()
    this._mocks = new Map()
  }

  // Mock the core method to return mocked core modules
  core(name, singleton = true) {
    const key = `core:${name}`

    if (this._mocks.has(key)) {
      return this._mocks.get(key)
    }

    // Default mocks for common core modules
    const coreMocks = {
      Config: {
        config: {
          server: {
            pid: null,
            started: null,
            watchdog: null,
            os: 'linux',
            arch: 'x64'
          }
        },
        force: jest.fn(),
        reload: jest.fn(),
        init: jest.fn()
      },
      Lang: {
        get: jest.fn(key => key),
        init: jest.fn()
      },
      Process: {
        spawn: jest.fn(),
        kill: jest.fn(),
        init: jest.fn()
      },
      Commands: {
        execute: jest.fn(),
        init: jest.fn()
      },
      Log: {
        init: jest.fn(function (moduleName) {
          return {
            log: jest.fn(),
            error: jest.fn()
          }
        })
      }
    }

    const mock = coreMocks[name] || {
      init: jest.fn()
    }

    this._mocks.set(key, mock)
    return mock
  }

  // Mock the server method to return mocked server modules
  server(name, singleton = true) {
    const key = `server:${name}`

    if (this._mocks.has(key)) {
      return this._mocks.get(key)
    }

    // Default mocks for server modules
    const serverMocks = {
      Api: {
        init: jest.fn(),
        start: jest.fn(),
        stop: jest.fn(),
        result: jest.fn((success, data) => ({success, data}))
      },
      Client: {
        auth: jest.fn(),
        call: jest.fn(),
        init: jest.fn()
      },
      DNS: {
        init: jest.fn(),
        start: jest.fn(),
        stop: jest.fn(),
        add: jest.fn(),
        delete: jest.fn(),
        list: jest.fn()
      },
      Mail: {
        init: jest.fn(),
        start: jest.fn(),
        stop: jest.fn(),
        create: jest.fn(),
        delete: jest.fn(),
        list: jest.fn(),
        password: jest.fn(),
        send: jest.fn()
      },
      SSL: {
        init: jest.fn(),
        renew: jest.fn(),
        check: jest.fn()
      },
      Server: {
        init: jest.fn(),
        start: jest.fn(),
        stop: jest.fn()
      },
      Service: {
        init: jest.fn(),
        add: jest.fn(),
        start: jest.fn(),
        stop: jest.fn(),
        list: jest.fn(),
        status: jest.fn()
      },
      Subdomain: {
        init: jest.fn(),
        create: jest.fn(),
        delete: jest.fn(),
        list: jest.fn()
      },
      Web: {
        init: jest.fn(),
        start: jest.fn(),
        stop: jest.fn(),
        create: jest.fn(),
        delete: jest.fn(),
        list: jest.fn()
      }
    }

    const mock = serverMocks[name] || {
      init: jest.fn()
    }

    this._mocks.set(key, mock)
    return mock
  }

  // Mock the cli method
  cli(name, singleton = true) {
    const key = `cli:${name}`

    if (this._mocks.has(key)) {
      return this._mocks.get(key)
    }

    const mock = {
      init: jest.fn(),
      execute: jest.fn()
    }

    this._mocks.set(key, mock)
    return mock
  }

  // Mock the watchdog method
  watchdog(name, singleton = true) {
    const key = `watchdog:${name}`

    if (this._mocks.has(key)) {
      return this._mocks.get(key)
    }

    const mock = {
      init: jest.fn(),
      start: jest.fn(),
      stop: jest.fn()
    }

    this._mocks.set(key, mock)
    return mock
  }

  // Helper method to set custom mocks
  setMock(type, name, mock) {
    const key = `${type}:${name}`
    this._mocks.set(key, mock)
  }

  // Helper method to get a mock
  getMock(type, name) {
    const key = `${type}:${name}`
    return this._mocks.get(key)
  }

  // Helper method to clear all mocks
  clearMocks() {
    this._mocks.forEach(mock => {
      if (mock && typeof mock === 'object') {
        Object.values(mock).forEach(fn => {
          if (jest.isMockFunction(fn)) {
            fn.mockClear()
          }
        })
      }
    })
  }

  // Helper method to reset all mocks
  resetMocks() {
    this._mocks.clear()
    this._registry.clear()
    this._singletons.clear()
  }
}

// Create the global mock instance
const mockOdac = new MockOdac()

// Mock the global __ function
const mockLangGet = jest.fn(key => key)

module.exports = {
  mockOdac,
  mockLangGet,
  MockOdac
}
