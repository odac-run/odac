/**
 * Index file for all server test mocks
 * Provides easy access to all mock modules and utilities
 */

// Import all mock modules
const fs = require('./fs')
const net = require('./net')
const http = require('./http')
const https = require('./https')
const child_process = require('./child_process')
const crypto = require('./crypto')
const os = require('./os')
const path = require('./path')

// Import test utilities
const {mockCandy, mockLangGet, MockOdac} = require('./globalCandy')
const testFactories = require('./testFactories')
const testHelpers = require('./testHelpers')

/**
 * Setup all Node.js module mocks for Jest
 * Call this in your test setup to mock all Node.js built-in modules
 */
const setupNodeMocks = () => {
  jest.mock('fs', () => require('./fs'))
  jest.mock('net', () => require('./net'))
  jest.mock('http', () => require('./http'))
  jest.mock('https', () => require('./https'))
  jest.mock('child_process', () => require('./child_process'))
  jest.mock('crypto', () => require('./crypto'))
  jest.mock('os', () => require('./os'))
  jest.mock('path', () => require('./path'))
}

/**
 * Setup global mocks (Odac and __ function)
 * Call this in beforeEach to set up global mocks
 */
const setupGlobalMocks = () => {
  global.Odac = mockCandy
  global.__ = mockLangGet

  // Clear all previous mock calls
  mockCandy.clearMocks()
  mockLangGet.mockClear()
}

/**
 * Cleanup all mocks
 * Call this in afterEach to clean up mocks
 */
const cleanupAllMocks = () => {
  // Clear global mocks
  mockCandy.resetMocks()
  mockLangGet.mockClear()

  // Clear Node.js module mocks
  fs.__resetFileSystem()
  net.__clearAll()
  http.__clearAll()
  https.__clearAll()
  child_process.__clearAll()
  crypto.__resetMocks()
  os.__resetMocks()
  path.__resetMocks()

  // Reset global references
  delete global.Odac
  delete global.__
}

/**
 * Create a complete test environment with all mocks
 * Returns an object with all mock modules and utilities
 */
const createTestEnvironment = () => {
  setupNodeMocks()
  setupGlobalMocks()

  return {
    // Mock modules
    mocks: {
      fs,
      net,
      http,
      https,
      child_process,
      crypto,
      os,
      path
    },

    // Global mocks
    global: {
      Odac: mockCandy,
      __: mockLangGet
    },

    // Test utilities
    factories: testFactories,
    helpers: testHelpers,

    // Cleanup function
    cleanup: cleanupAllMocks
  }
}

/**
 * Timer mock utilities for controlling time in tests
 */
const createTimerMocks = () => {
  const timers = new Map()
  let timerId = 1

  const mockSetTimeout = jest.fn((callback, delay) => {
    const id = timerId++
    timers.set(id, {callback, delay, type: 'timeout'})
    return id
  })

  const mockSetInterval = jest.fn((callback, delay) => {
    const id = timerId++
    const timer = {
      callback,
      delay,
      type: 'interval',
      unref: jest.fn(() => timer)
    }
    timers.set(id, timer)
    return timer
  })

  const mockClearTimeout = jest.fn(id => {
    timers.delete(id)
  })

  const mockClearInterval = jest.fn(id => {
    timers.delete(id)
  })

  // Apply mocks to global
  global.setTimeout = mockSetTimeout
  global.setInterval = mockSetInterval
  global.clearTimeout = mockClearTimeout
  global.clearInterval = mockClearInterval

  return {
    setTimeout: mockSetTimeout,
    setInterval: mockSetInterval,
    clearTimeout: mockClearTimeout,
    clearInterval: mockClearInterval,

    // Helper functions
    executeTimer: id => {
      const timer = timers.get(id)
      if (timer && timer.callback) {
        timer.callback()
      }
    },

    executeAllTimers: () => {
      timers.forEach(timer => {
        if (timer.callback) {
          timer.callback()
        }
      })
    },

    getTimers: () => new Map(timers),

    clearAllTimers: () => {
      timers.clear()
      timerId = 1
    }
  }
}

/**
 * Process mock utilities for controlling process behavior
 */
const createProcessMocks = () => {
  const originalProcess = {...process}

  const mockProcess = {
    ...process,
    exit: jest.fn(),
    kill: jest.fn(),
    nextTick: jest.fn(callback => {
      setTimeout(callback, 0)
    }),
    env: {...process.env},
    argv: [...process.argv],
    cwd: jest.fn(() => '/mock/cwd'),
    chdir: jest.fn(directory => {
      mockProcess._cwd = directory
    }),

    // Test helpers
    __setEnv: env => {
      mockProcess.env = {...env}
    },

    __setArgv: argv => {
      mockProcess.argv = [...argv]
    },

    __setCwd: cwd => {
      mockProcess.cwd.mockReturnValue(cwd)
    },

    __reset: () => {
      Object.assign(mockProcess, originalProcess)
      mockProcess.env = {...originalProcess.env}
      mockProcess.argv = [...originalProcess.argv]
    }
  }

  return mockProcess
}

module.exports = {
  // Mock modules
  fs,
  net,
  http,
  https,
  child_process,
  crypto,
  os,
  path,

  // Global mocks
  mockCandy,
  mockLangGet,
  MockOdac,

  // Test utilities
  testFactories,
  testHelpers,

  // Setup functions
  setupNodeMocks,
  setupGlobalMocks,
  cleanupAllMocks,
  createTestEnvironment,
  createTimerMocks,
  createProcessMocks
}
