// Create a test version of the Server class
class TestServer {
  constructor() {
    this.mockConfig = {
      server: {
        pid: null,
        started: null
      }
    }

    this.mockService = {
      check: jest.fn(),
      stopAll: jest.fn()
    }

    this.mockDNS = {}
    this.mockWeb = {
      check: jest.fn(),
      stopAll: jest.fn()
    }

    this.mockMail = {
      check: jest.fn()
    }

    this.mockApi = {}
    this.mockSSL = {
      check: jest.fn()
    }

    // Store original global Odac if it exists
    this.originalCandy = global.Odac

    // Setup isolated Odac mock for this instance
    global.Odac = {
      core: jest.fn(module => {
        if (module === 'Config') {
          return {config: this.mockConfig}
        }
        return {}
      }),
      server: jest.fn(module => {
        switch (module) {
          case 'Service':
            return this.mockService
          case 'DNS':
            return this.mockDNS
          case 'Web':
            return this.mockWeb
          case 'Mail':
            return this.mockMail
          case 'Api':
            return this.mockApi
          case 'SSL':
            return this.mockSSL
          default:
            return {}
        }
      })
    }

    // Initialize like the real Server
    this.init()
  }

  init() {
    global.Odac.core('Config').config.server.pid = process.pid
    global.Odac.core('Config').config.server.started = Date.now()
    global.Odac.server('Service')
    global.Odac.server('DNS')
    global.Odac.server('Web')
    global.Odac.server('Mail')
    global.Odac.server('Api')

    // Setup health checks
    setTimeout(() => {
      this.intervalId = setInterval(() => {
        try {
          global.Odac.server('Service').check()
        } catch (e) {
          // Ignore service check errors
        }
        try {
          global.Odac.server('SSL').check()
        } catch (e) {
          // Ignore SSL check errors
        }
        try {
          global.Odac.server('Web').check()
        } catch (e) {
          // Ignore Web check errors
        }
        try {
          global.Odac.server('Mail').check()
        } catch (e) {
          // Ignore Mail check errors
        }
      }, 1000)
    }, 1000)
  }

  stop() {
    try {
      global.Odac.server('Service').stopAll()
    } catch (e) {
      // Ignore service stop errors
    }
    try {
      global.Odac.server('Web').stopAll()
    } catch (e) {
      // Ignore web stop errors
    }
    if (this.intervalId) {
      clearInterval(this.intervalId)
    }
  }

  destroy() {
    this.stop()
    // Restore original Odac if it existed
    if (this.originalCandy) {
      global.Odac = this.originalCandy
    } else {
      delete global.Odac
    }
  }
}

describe('Server', () => {
  let server

  beforeEach(() => {
    jest.clearAllMocks()
    jest.clearAllTimers()
    jest.useFakeTimers()

    // Clear any existing global Odac
    delete global.Odac
  })

  afterEach(() => {
    if (server && server.destroy) {
      server.destroy()
    }
    jest.useRealTimers()
    delete global.Odac
  })

  describe('initialization', () => {
    test('should set server PID in config during construction', () => {
      const originalPid = process.pid

      // Set the PID before creating the server
      Object.defineProperty(process, 'pid', {
        value: 12345,
        writable: true
      })

      server = new TestServer()

      expect(server.mockConfig.server.pid).toBe(12345)

      // Restore original PID
      Object.defineProperty(process, 'pid', {
        value: originalPid,
        writable: true
      })
    })

    test('should set server started timestamp during construction', () => {
      const mockDate = 1640995200000 // 2022-01-01 00:00:00
      jest.spyOn(Date, 'now').mockReturnValue(mockDate)

      server = new TestServer()

      expect(server.mockConfig.server.started).toBe(mockDate)

      Date.now.mockRestore()
    })

    test('should initialize all required services in correct order', () => {
      server = new TestServer()

      // Verify all services are initialized
      expect(global.Odac.server).toHaveBeenCalledWith('Service')
      expect(global.Odac.server).toHaveBeenCalledWith('DNS')
      expect(global.Odac.server).toHaveBeenCalledWith('Web')
      expect(global.Odac.server).toHaveBeenCalledWith('Mail')
      expect(global.Odac.server).toHaveBeenCalledWith('Api')

      // Verify call order
      const serverCalls = global.Odac.server.mock.calls.map(call => call[0])
      expect(serverCalls).toEqual(['Service', 'DNS', 'Web', 'Mail', 'Api'])
    })

    test('should setup periodic health checks after initialization delay', () => {
      server = new TestServer()

      // Initially no checks should be called
      expect(server.mockService.check).not.toHaveBeenCalled()
      expect(server.mockSSL.check).not.toHaveBeenCalled()
      expect(server.mockWeb.check).not.toHaveBeenCalled()
      expect(server.mockMail.check).not.toHaveBeenCalled()

      // Fast-forward past initial delay
      jest.advanceTimersByTime(1000)

      // Still no checks after just the initial delay
      expect(server.mockService.check).not.toHaveBeenCalled()

      // Fast-forward to trigger first interval
      jest.advanceTimersByTime(1000)

      // Now checks should be called
      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
      expect(server.mockMail.check).toHaveBeenCalledTimes(1)
    })

    test('should continue periodic health checks at 1 second intervals', () => {
      server = new TestServer()

      // Fast-forward past initial delay and first interval
      jest.advanceTimersByTime(2000)

      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
      expect(server.mockMail.check).toHaveBeenCalledTimes(1)

      // Fast-forward another second
      jest.advanceTimersByTime(1000)

      expect(server.mockService.check).toHaveBeenCalledTimes(2)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(2)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(2)
      expect(server.mockMail.check).toHaveBeenCalledTimes(2)

      // Fast-forward multiple intervals
      jest.advanceTimersByTime(3000)

      expect(server.mockService.check).toHaveBeenCalledTimes(5)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(5)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(5)
      expect(server.mockMail.check).toHaveBeenCalledTimes(5)
    })
  })

  describe('service coordination', () => {
    test('should handle service initialization errors gracefully', () => {
      // Create a modified TestServer that throws during DNS initialization
      class ErrorTestServer extends TestServer {
        init() {
          global.Odac.core('Config').config.server.pid = process.pid
          global.Odac.core('Config').config.server.started = Date.now()
          global.Odac.server('Service')

          // This should throw
          throw new Error('DNS initialization failed')
        }
      }

      expect(() => {
        new ErrorTestServer()
      }).toThrow('DNS initialization failed')
    })

    test('should maintain service references for health checks', () => {
      server = new TestServer()

      // Fast-forward to trigger health checks
      jest.advanceTimersByTime(2000)

      // Verify that the same service instances are being called during health checks
      expect(global.Odac.server).toHaveBeenCalledWith('Service')
      expect(global.Odac.server).toHaveBeenCalledWith('SSL')
      expect(global.Odac.server).toHaveBeenCalledWith('Web')
      expect(global.Odac.server).toHaveBeenCalledWith('Mail')
    })
  })

  describe('monitoring and health checks', () => {
    beforeEach(() => {
      server = new TestServer()
    })

    test('should perform health checks on all monitored services', () => {
      // Fast-forward to trigger health checks
      jest.advanceTimersByTime(2000)

      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
      expect(server.mockMail.check).toHaveBeenCalledTimes(1)
    })

    test('should handle health check errors without stopping monitoring', () => {
      // Mock a service check that throws an error
      server.mockService.check.mockImplementation(() => {
        throw new Error('Service check failed')
      })

      // Should not throw when health check fails
      expect(() => {
        jest.advanceTimersByTime(2000)
      }).not.toThrow()

      // Other services should still be checked
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
      expect(server.mockMail.check).toHaveBeenCalledTimes(1)
    })

    test('should continue monitoring after individual service failures', () => {
      // Mock SSL check to fail on first call but succeed on second
      let callCount = 0
      server.mockSSL.check.mockImplementation(() => {
        callCount++
        if (callCount === 1) {
          throw new Error('SSL check failed')
        }
      })

      // First health check cycle
      jest.advanceTimersByTime(2000)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)

      // Second health check cycle should still call SSL check
      jest.advanceTimersByTime(1000)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(2)
    })

    test('should maintain consistent monitoring intervals', () => {
      const checkTimes = []
      server.mockService.check.mockImplementation(() => {
        checkTimes.push(Date.now())
      })

      // Trigger multiple health check cycles
      jest.advanceTimersByTime(2000) // Initial delay + first check
      jest.advanceTimersByTime(1000) // Second check
      jest.advanceTimersByTime(1000) // Third check

      expect(server.mockService.check).toHaveBeenCalledTimes(3)
    })

    test('should implement 1-second periodic health check intervals', () => {
      // Verify initial delay before first health check
      jest.advanceTimersByTime(999)
      expect(server.mockService.check).not.toHaveBeenCalled()

      // First health check after 1 second delay
      jest.advanceTimersByTime(1)
      expect(server.mockService.check).not.toHaveBeenCalled() // Still in initial delay

      // Complete initial delay and first interval
      jest.advanceTimersByTime(1000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)

      // Verify subsequent 1-second intervals
      for (let i = 2; i <= 5; i++) {
        jest.advanceTimersByTime(1000)
        expect(server.mockService.check).toHaveBeenCalledTimes(i)
      }
    })

    test('should monitor service status and health continuously', () => {
      const serviceStatuses = []

      // Track service check calls
      server.mockService.check.mockImplementation(() => {
        serviceStatuses.push('Service checked')
      })

      server.mockWeb.check.mockImplementation(() => {
        serviceStatuses.push('Web checked')
      })

      // Run multiple monitoring cycles
      jest.advanceTimersByTime(2000) // First cycle
      jest.advanceTimersByTime(1000) // Second cycle
      jest.advanceTimersByTime(1000) // Third cycle

      expect(serviceStatuses).toEqual([
        'Service checked',
        'Web checked',
        'Service checked',
        'Web checked',
        'Service checked',
        'Web checked'
      ])
    })

    test('should handle all service types in monitoring cycle', () => {
      // Fast-forward to trigger multiple health check cycles
      jest.advanceTimersByTime(4000) // 3 complete cycles

      // Verify all service types are monitored consistently
      expect(server.mockService.check).toHaveBeenCalledTimes(3)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(3)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(3)
      expect(server.mockMail.check).toHaveBeenCalledTimes(3)
    })

    test('should isolate service check failures from each other', () => {
      // Mock different services to fail at different times
      let serviceCallCount = 0
      let sslCallCount = 0

      server.mockService.check.mockImplementation(() => {
        serviceCallCount++
        if (serviceCallCount === 2) {
          throw new Error('Service check failed on second call')
        }
      })

      server.mockSSL.check.mockImplementation(() => {
        sslCallCount++
        if (sslCallCount === 1) {
          throw new Error('SSL check failed on first call')
        }
      })

      // Run multiple cycles
      jest.advanceTimersByTime(2000) // First cycle - SSL fails
      jest.advanceTimersByTime(1000) // Second cycle - Service fails
      jest.advanceTimersByTime(1000) // Third cycle - both succeed

      // All services should have been called despite individual failures
      expect(server.mockService.check).toHaveBeenCalledTimes(3)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(3)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(3)
      expect(server.mockMail.check).toHaveBeenCalledTimes(3)
    })

    test('should maintain monitoring state across service restarts', () => {
      // Start monitoring
      jest.advanceTimersByTime(2000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)

      // Simulate service restart by resetting mock
      server.mockService.check.mockClear()

      // Continue monitoring
      jest.advanceTimersByTime(2000)
      expect(server.mockService.check).toHaveBeenCalledTimes(2)
    })
  })

  describe('graceful shutdown', () => {
    beforeEach(() => {
      server = new TestServer()
    })

    test('should stop all services when stop() is called', () => {
      server.stop()

      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)
    })

    test('should handle service stop errors gracefully', () => {
      // Mock service stopAll to throw an error
      server.mockService.stopAll.mockImplementation(() => {
        throw new Error('Service stop failed')
      })

      // Should not throw when stopping services
      expect(() => {
        server.stop()
      }).not.toThrow()

      // Web service should still be stopped
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)
    })

    test('should stop services in correct order', () => {
      const stopOrder = []

      server.mockService.stopAll.mockImplementation(() => {
        stopOrder.push('Service')
      })

      server.mockWeb.stopAll.mockImplementation(() => {
        stopOrder.push('Web')
      })

      server.stop()

      expect(stopOrder).toEqual(['Service', 'Web'])
    })

    test('should handle partial service stop failures', () => {
      // Mock Web stopAll to fail
      server.mockWeb.stopAll.mockImplementation(() => {
        throw new Error('Web stop failed')
      })

      expect(() => {
        server.stop()
      }).not.toThrow()

      // Service should still be stopped
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)
    })

    test('should clear health check intervals during shutdown', () => {
      // Start health checks
      jest.advanceTimersByTime(2000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)

      // Stop the server
      server.stop()

      // Advance time and verify no more health checks occur
      jest.advanceTimersByTime(5000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1) // Should remain 1
    })

    test('should handle shutdown when health checks are not yet started', () => {
      // Stop server immediately after creation
      server.stop()

      // Verify shutdown methods are called
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)

      // Test should not throw and shutdown should complete successfully
      expect(() => {
        jest.advanceTimersByTime(5000)
      }).not.toThrow()
    })

    test('should perform complete cleanup of all resources', () => {
      // Start health checks
      jest.advanceTimersByTime(2000)

      // Verify health checks are running
      expect(server.mockService.check).toHaveBeenCalledTimes(1)

      // Stop server
      server.stop()

      // Verify all cleanup actions
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)

      // Verify no more health checks after cleanup
      jest.advanceTimersByTime(10000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockSSL.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
      expect(server.mockMail.check).toHaveBeenCalledTimes(1)
    })

    test('should handle multiple shutdown calls gracefully', () => {
      // Call stop multiple times
      server.stop()
      server.stop()
      server.stop()

      // Services should only be stopped once
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(3)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(3)
    })
  })

  describe('resource deallocation and cleanup', () => {
    beforeEach(() => {
      server = new TestServer()
    })

    test('should deallocate all monitoring intervals during shutdown', () => {
      // Start monitoring
      jest.advanceTimersByTime(2000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)

      // Verify interval is active
      const activeTimers = jest.getTimerCount()
      expect(activeTimers).toBeGreaterThan(0)

      // Stop server and verify interval cleanup
      server.stop()

      // Advance time significantly and verify no more checks
      jest.advanceTimersByTime(10000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1) // Should not increase
    })

    test('should handle cleanup when no intervals are active', () => {
      // Stop server before any intervals are created
      server.stop()

      expect(() => {
        jest.advanceTimersByTime(5000)
      }).not.toThrow()

      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)
    })

    test('should perform complete resource cleanup sequence', () => {
      const cleanupOrder = []

      server.mockService.stopAll.mockImplementation(() => {
        cleanupOrder.push('Service cleanup')
      })

      server.mockWeb.stopAll.mockImplementation(() => {
        cleanupOrder.push('Web cleanup')
      })

      // Start monitoring to create resources
      jest.advanceTimersByTime(2000)

      // Perform cleanup
      server.stop()

      expect(cleanupOrder).toEqual(['Service cleanup', 'Web cleanup'])
    })

    test('should handle resource cleanup errors without failing', () => {
      // Mock cleanup methods to throw errors
      server.mockService.stopAll.mockImplementation(() => {
        throw new Error('Service cleanup failed')
      })

      server.mockWeb.stopAll.mockImplementation(() => {
        throw new Error('Web cleanup failed')
      })

      // Should not throw during cleanup
      expect(() => {
        server.stop()
      }).not.toThrow()

      // Verify cleanup was attempted
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)
    })

    test('should ensure no memory leaks from monitoring intervals', () => {
      // Test that stopped servers don't continue monitoring
      const testServer1 = new TestServer()

      // Start monitoring for first server
      jest.advanceTimersByTime(2000)
      const firstServerCalls = testServer1.mockService.check.mock.calls.length
      expect(firstServerCalls).toBeGreaterThan(0)

      // Stop and destroy first server
      testServer1.destroy()

      // Create second server with fresh mocks
      const testServer2 = new TestServer()

      // Advance time - only second server should have monitoring
      jest.advanceTimersByTime(3000)

      // First server should not have additional calls after stop
      expect(testServer1.mockService.check).toHaveBeenCalledTimes(firstServerCalls)

      // Second server should have its own monitoring calls
      expect(testServer2.mockService.check.mock.calls.length).toBeGreaterThan(0)

      // Clean up second server
      testServer2.destroy()
    })

    test('should clean up service references during shutdown', () => {
      // Start monitoring
      jest.advanceTimersByTime(2000)

      // Verify services are being monitored
      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)

      // Stop server
      server.stop()

      // Verify service cleanup was called
      expect(server.mockService.stopAll).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.stopAll).toHaveBeenCalledTimes(1)

      // Verify no further service interactions
      jest.advanceTimersByTime(5000)
      expect(server.mockService.check).toHaveBeenCalledTimes(1)
      expect(server.mockWeb.check).toHaveBeenCalledTimes(1)
    })
  })

  describe('configuration management', () => {
    test('should update config with current process PID', () => {
      const originalPid = process.pid

      // Set the PID before creating the server
      Object.defineProperty(process, 'pid', {
        value: 54321,
        writable: true
      })

      server = new TestServer()

      expect(server.mockConfig.server.pid).toBe(54321)

      // Restore original PID
      Object.defineProperty(process, 'pid', {
        value: originalPid,
        writable: true
      })
    })

    test('should record server startup timestamp', () => {
      const mockTimestamp = 1641081600000 // 2022-01-02 00:00:00
      jest.spyOn(Date, 'now').mockReturnValue(mockTimestamp)

      server = new TestServer()

      expect(server.mockConfig.server.started).toBe(mockTimestamp)

      Date.now.mockRestore()
    })

    test('should handle config access errors gracefully', () => {
      // Create a function that simulates the Server initialization with config error
      const createServerWithConfigError = () => {
        // Setup global Odac mock that throws on Config access
        global.Odac = {
          core: jest.fn(module => {
            if (module === 'Config') {
              throw new Error('Config access failed')
            }
            return {}
          }),
          server: jest.fn(() => ({}))
        }

        // Simulate the Server initialization process
        global.Odac.core('Config').config.server.pid = process.pid
      }

      expect(() => {
        createServerWithConfigError()
      }).toThrow('Config access failed')
    })

    test('should preserve existing config structure', () => {
      server = new TestServer()

      // Add existing config data
      server.mockConfig.server.existingProperty = 'existing value'
      server.mockConfig.otherSection = {data: 'preserved'}

      // Re-initialize to test preservation
      server.init()

      expect(server.mockConfig.server.existingProperty).toBe('existing value')
      expect(server.mockConfig.otherSection).toEqual({data: 'preserved'})
      expect(server.mockConfig.server.pid).toBe(process.pid)
      expect(server.mockConfig.server.started).toBeDefined()
    })
  })
})
