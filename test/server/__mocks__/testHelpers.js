/**
 * Shared test utilities for common assertions and setup patterns
 * Provides helper functions to reduce boilerplate in server tests
 */

const {mockOdac, mockLangGet} = require('./globalOdac')

/**
 * Sets up the global Odac mock and __ function for tests
 * Should be called in beforeEach or beforeAll
 */
const setupGlobalMocks = () => {
  // Set up global Odac mock
  global.Odac = mockOdac

  // Set up global __ function mock
  global.__ = mockLangGet

  // Clear all previous mock calls
  mockOdac.clearMocks()
  mockLangGet.mockClear()
}

/**
 * Cleans up global mocks after tests
 * Should be called in afterEach or afterAll
 */
const cleanupGlobalMocks = () => {
  mockOdac.resetMocks()
  mockLangGet.mockClear()

  // Reset global references
  delete global.Odac
  delete global.__
}

/**
 * Creates a mock timer that can be controlled in tests
 */
const createMockTimer = () => {
  const timer = {
    id: Math.floor(Math.random() * 1000),
    callback: null,
    interval: 0,
    active: false,
    unref: jest.fn(() => timer)
  }

  return timer
}

/**
 * Helper to mock setTimeout and setInterval
 */
const mockTimers = () => {
  const timers = new Map()
  let timerId = 1

  const mockSetTimeout = jest.fn((callback, delay) => {
    const id = timerId++
    timers.set(id, {callback, delay, type: 'timeout'})
    return id
  })

  const mockSetInterval = jest.fn((callback, delay) => {
    const id = timerId++
    const timer = {callback, delay, type: 'interval', unref: jest.fn()}
    timers.set(id, timer)
    return timer
  })

  const mockClearTimeout = jest.fn(id => {
    timers.delete(id)
  })

  const mockClearInterval = jest.fn(id => {
    timers.delete(id)
  })

  // Helper to execute timer callbacks
  const executeTimer = id => {
    const timer = timers.get(id)
    if (timer && timer.callback) {
      timer.callback()
    }
  }

  // Helper to execute all timers
  const executeAllTimers = () => {
    timers.forEach((timer, id) => {
      if (timer.callback) {
        timer.callback()
      }
    })
  }

  return {
    setTimeout: mockSetTimeout,
    setInterval: mockSetInterval,
    clearTimeout: mockClearTimeout,
    clearInterval: mockClearInterval,
    executeTimer,
    executeAllTimers,
    getTimers: () => timers
  }
}

/**
 * Helper to create a mock EventEmitter
 */
const createMockEventEmitter = () => {
  const listeners = new Map()

  const emitter = {
    on: jest.fn((event, listener) => {
      if (!listeners.has(event)) {
        listeners.set(event, [])
      }
      listeners.get(event).push(listener)
      return emitter
    }),

    once: jest.fn((event, listener) => {
      const onceWrapper = (...args) => {
        listener(...args)
        emitter.removeListener(event, onceWrapper)
      }
      return emitter.on(event, onceWrapper)
    }),

    emit: jest.fn((event, ...args) => {
      const eventListeners = listeners.get(event)
      if (eventListeners) {
        eventListeners.forEach(listener => listener(...args))
        return true
      }
      return false
    }),

    removeListener: jest.fn((event, listener) => {
      const eventListeners = listeners.get(event)
      if (eventListeners) {
        const index = eventListeners.indexOf(listener)
        if (index > -1) {
          eventListeners.splice(index, 1)
        }
      }
      return emitter
    }),

    removeAllListeners: jest.fn(event => {
      if (event) {
        listeners.delete(event)
      } else {
        listeners.clear()
      }
      return emitter
    }),

    listenerCount: jest.fn(event => {
      const eventListeners = listeners.get(event)
      return eventListeners ? eventListeners.length : 0
    }),

    // Helper methods for testing
    getListeners: event => listeners.get(event) || [],
    getAllListeners: () => listeners
  }

  return emitter
}

/**
 * Helper to wait for async operations in tests
 */
const waitFor = (ms = 0) => {
  return new Promise(resolve => setTimeout(resolve, ms))
}

/**
 * Helper to wait for a condition to be true
 */
const waitForCondition = async (condition, timeout = 1000, interval = 10) => {
  const start = Date.now()

  while (Date.now() - start < timeout) {
    if (await condition()) {
      return true
    }
    await waitFor(interval)
  }

  throw new Error(`Condition not met within ${timeout}ms`)
}

/**
 * Helper to capture console output during tests
 */
const captureConsole = () => {
  const originalLog = console.log
  const originalError = console.error
  const originalWarn = console.warn
  const originalInfo = console.info

  const logs = []
  const errors = []
  const warnings = []
  const infos = []

  console.log = jest.fn((...args) => {
    logs.push(args.join(' '))
  })

  console.error = jest.fn((...args) => {
    errors.push(args.join(' '))
  })

  console.warn = jest.fn((...args) => {
    warnings.push(args.join(' '))
  })

  console.info = jest.fn((...args) => {
    infos.push(args.join(' '))
  })

  const restore = () => {
    console.log = originalLog
    console.error = originalError
    console.warn = originalWarn
    console.info = originalInfo
  }

  return {
    logs,
    errors,
    warnings,
    infos,
    restore
  }
}

/**
 * Helper to create a mock stream
 */
const createMockStream = (readable = true, writable = true) => {
  const stream = createMockEventEmitter()

  Object.assign(stream, {
    readable,
    writable,
    destroyed: false,

    read: jest.fn(),
    write: jest.fn((chunk, encoding, callback) => {
      if (typeof encoding === 'function') {
        callback = encoding
      }
      if (callback) callback()
      return true
    }),
    end: jest.fn((chunk, encoding, callback) => {
      if (chunk) stream.write(chunk, encoding)
      if (typeof encoding === 'function') {
        callback = encoding
      }
      if (typeof chunk === 'function') {
        callback = chunk
      }
      if (callback) callback()
      stream.emit('end')
    }),
    destroy: jest.fn(error => {
      stream.destroyed = true
      if (error) {
        stream.emit('error', error)
      }
      stream.emit('close')
    }),
    pipe: jest.fn(destination => {
      return destination
    })
  })

  return stream
}

/**
 * Helper assertions for common test patterns
 */
const assertions = {
  /**
   * Assert that a mock function was called with specific arguments
   */
  toHaveBeenCalledWithArgs: (mockFn, ...expectedArgs) => {
    expect(mockFn).toHaveBeenCalledWith(...expectedArgs)
  },

  /**
   * Assert that an object has all required properties
   */
  toHaveRequiredProperties: (obj, properties) => {
    properties.forEach(prop => {
      expect(obj).toHaveProperty(prop)
    })
  },

  /**
   * Assert that a function throws a specific error
   */
  toThrowErrorWithMessage: (fn, expectedMessage) => {
    expect(fn).toThrow(expectedMessage)
  },

  /**
   * Assert that an async function resolves to a specific value
   */
  toResolveWith: async (promise, expectedValue) => {
    const result = await promise
    expect(result).toEqual(expectedValue)
  },

  /**
   * Assert that an async function rejects with a specific error
   */
  toRejectWith: async (promise, expectedError) => {
    await expect(promise).rejects.toThrow(expectedError)
  }
}

/**
 * Helper to create a test suite with common setup/teardown
 */
const createTestSuite = (suiteName, setupFn, teardownFn) => {
  return testFn => {
    describe(suiteName, () => {
      beforeEach(() => {
        setupGlobalMocks()
        if (setupFn) setupFn()
      })

      afterEach(() => {
        if (teardownFn) teardownFn()
        cleanupGlobalMocks()
      })

      testFn()
    })
  }
}

module.exports = {
  setupGlobalMocks,
  cleanupGlobalMocks,
  createMockTimer,
  mockTimers,
  createMockEventEmitter,
  waitFor,
  waitForCondition,
  captureConsole,
  createMockStream,
  assertions,
  createTestSuite
}
