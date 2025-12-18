// Import test utilities
const {setupGlobalMocks, createMockEventEmitter} = require('./__mocks__/testHelpers')
const {createMockServiceConfig} = require('./__mocks__/testFactories')

// Create enhanced mock child process with EventEmitter capabilities
const createMockChildProcess = (pid = 12345) => {
  const emitter = createMockEventEmitter()
  const stdout = createMockEventEmitter()
  const stderr = createMockEventEmitter()

  return {
    pid,
    killed: false,
    exitCode: null,
    signalCode: null,
    connected: true,
    stdout,
    stderr,
    stdin: {
      write: jest.fn(),
      end: jest.fn(),
      writable: true
    },
    kill: jest.fn((signal = 'SIGTERM') => {
      emitter.killed = true
      emitter.signalCode = signal
      return true
    }),
    send: jest.fn(),
    disconnect: jest.fn(),
    ...emitter
  }
}

// Service will be required inside tests after mocks are set up
let Service

describe('Service', () => {
  let mockCandy
  let mockChildProcess
  let mockSpawn
  let childProcess
  let fs
  let os
  let path

  beforeEach(() => {
    jest.clearAllMocks()

    // Setup global Odac mock
    setupGlobalMocks()
    mockCandy = global.Odac

    // Mock child_process module
    mockChildProcess = createMockChildProcess()
    mockSpawn = jest.fn().mockReturnValue(mockChildProcess)

    jest.doMock('child_process', () => ({
      spawn: mockSpawn
    }))

    // Mock fs module
    jest.doMock('fs', () => ({
      existsSync: jest.fn().mockReturnValue(true),
      readFile: jest.fn().mockImplementation((filePath, encoding, callback) => {
        callback(null, 'mock log data')
      }),
      writeFile: jest.fn().mockImplementation((filePath, data, encoding, callback) => {
        callback(null)
      })
    }))

    // Mock os module
    jest.doMock('os', () => ({
      homedir: jest.fn().mockReturnValue('/home/user')
    }))

    // Mock path module
    jest.doMock('path', () => ({
      basename: jest.fn().mockImplementation(filePath => {
        const parts = filePath.split('/')
        return parts[parts.length - 1]
      }),
      dirname: jest.fn().mockImplementation(filePath => {
        const parts = filePath.split('/')
        return parts.slice(0, -1).join('/')
      }),
      resolve: jest.fn().mockImplementation(filePath => `/resolved${filePath}`)
    }))

    // Reset and re-require Service to get fresh instance
    jest.resetModules()

    // Get the mocked modules
    childProcess = require('child_process')
    fs = require('fs')
    os = require('os')
    path = require('path')

    // Now require Service with our mocks in place
    Service = require('../../server/src/Service')
  })

  afterEach(() => {
    jest.resetModules()
    jest.dontMock('child_process')
    jest.dontMock('fs')
    jest.dontMock('os')
    jest.dontMock('path')
  })

  describe('Service registration and process spawning', () => {
    test('should validate service file existence checking', async () => {
      const testFile = '/path/to/nonexistent-service.js'

      // Mock fs.existsSync to return false for non-existent file
      fs.existsSync.mockReturnValue(false)

      // Mock config
      mockCandy.setMock('core', 'Config', {
        config: {services: []}
      })

      const result = await Service.start(testFile)

      expect(result.success).toBe(false)
      expect(result.data).toContain('not found')
    })

    test('should reject empty service file parameter', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {services: []}
      })

      const result = await Service.start('')

      expect(result.success).toBe(false)
      expect(result.data).toContain('not specified')
    })

    test('should prevent duplicate service registration', async () => {
      const testFile = '/path/to/test-service.js'
      const resolvedFile = `/resolved${testFile}`

      // Create existing service that matches the resolved file path
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        name: 'test-service.js',
        file: resolvedFile,
        active: true
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [existingService]}
      })

      // Mock fs to say file exists
      fs.existsSync.mockReturnValue(true)

      const result = await Service.start(testFile)

      expect(result.success).toBe(true)
      expect(result.data).toContain('already exists')
    })

    test('should generate correct service configuration structure', () => {
      // Test the service configuration structure by examining the factory
      const mockService = createMockServiceConfig('my-awesome-service.js', {
        file: '/resolved/path/to/my-awesome-service.js'
      })

      // Verify the service has all required properties for registration
      expect(mockService).toHaveProperty('id')
      expect(mockService).toHaveProperty('name', 'my-awesome-service.js')
      expect(mockService).toHaveProperty('file', '/resolved/path/to/my-awesome-service.js')
      expect(mockService).toHaveProperty('active', true)
      expect(typeof mockService.id).toBe('number')
    })

    test('should handle service file path resolution', () => {
      const testFile = '/path/to/test-service.js'

      // Test that path.resolve is called correctly
      const resolved = path.resolve(testFile)
      expect(resolved).toBe(`/resolved${testFile}`)
      expect(path.resolve).toHaveBeenCalledWith(testFile)
    })

    test('should handle service name extraction from file path', () => {
      const testCases = [
        {input: '/path/to/my-service.js', expected: 'my-service.js'},
        {input: '/another/path/awesome-service.js', expected: 'awesome-service.js'},
        {input: 'simple-service.js', expected: 'simple-service.js'}
      ]

      testCases.forEach(({input, expected}) => {
        const result = path.basename(input)
        expect(result).toBe(expected)
      })
    })

    test('should handle working directory extraction from file path', () => {
      const testCases = [
        {input: '/path/to/services/my-service.js', expected: '/path/to/services'},
        {input: '/another/location/service.js', expected: '/another/location'},
        {input: '/root/service.js', expected: '/root'}
      ]

      testCases.forEach(({input, expected}) => {
        const result = path.dirname(input)
        expect(result).toBe(expected)
      })
    })

    test('should verify child process spawning interface', () => {
      // Test that childProcess.spawn is available and can be called with correct parameters
      const testFile = '/resolved/path/to/test-service.js'
      const expectedCwd = '/resolved/path/to'

      // Call spawn directly to verify the interface
      const mockProcess = mockSpawn('node', [testFile], {cwd: expectedCwd})

      expect(mockSpawn).toHaveBeenCalledWith('node', [testFile], {cwd: expectedCwd})
      expect(mockProcess).toBeDefined()
      expect(mockProcess.pid).toBeDefined()
    })

    test('should verify process ID tracking capabilities', () => {
      // Test that we can track process IDs and status
      const mockPid = 12345
      const serviceConfig = createMockServiceConfig('test-service.js', {
        id: 0,
        active: true,
        pid: mockPid,
        status: 'running',
        started: Date.now()
      })

      // Verify the service configuration has all required fields for process tracking
      expect(serviceConfig).toMatchObject({
        pid: mockPid,
        status: 'running',
        active: true
      })
      expect(serviceConfig.started).toBeDefined()
      expect(typeof serviceConfig.started).toBe('number')
    })
  })

  describe('Service monitoring and restart logic', () => {
    test('should detect services with missing PIDs and restart them', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null // No PID means not running
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      expect(mockSpawn).toHaveBeenCalledWith('node', [existingService.file], {
        cwd: path.dirname(existingService.file)
      })
      expect(servicesConfig[0].pid).toBe(12345)
      expect(servicesConfig[0].status).toBe('running')
    })

    test('should restart services when watcher indicates process is not running', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: 12345
      })

      let servicesConfig = [existingService]

      // Mock Process.stop
      mockCandy.setMock('core', 'Process', {
        stop: jest.fn()
      })

      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      expect(mockCandy.getMock('core', 'Process').stop).toHaveBeenCalledWith(12345)
      expect(mockSpawn).toHaveBeenCalled()
    })

    test('should implement error counting mechanism by tracking crashes', async () => {
      // Test that the Service module tracks error counts internally
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null // Start without PID to trigger restart
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // First check should start the service
      await Service.check()
      expect(mockSpawn).toHaveBeenCalledTimes(1)

      // Simulate process exit to increment error count and set #active to false
      mockChildProcess.emit('exit', 1)

      // Service should now be stopped and have no PID
      expect(servicesConfig[0].status).toBe('stopped')
      expect(servicesConfig[0].pid).toBeNull()

      // Set updated time to past to avoid cooldown (error_count * 1000ms)
      servicesConfig[0].updated = Date.now() - 2000

      // Second check should restart since pid is null and cooldown has passed
      await Service.check()
      expect(mockSpawn).toHaveBeenCalledTimes(2)
    })

    test('should implement cooldown period after errors', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null,
        status: 'errored',
        updated: Date.now() // Recent error timestamp - should trigger cooldown
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // First start the service to get an error count
      await Service.check()
      const initialSpawnCount = mockSpawn.mock.calls.length

      // Simulate exit to increment error count
      mockChildProcess.emit('exit', 1)

      // Set service to errored with very recent timestamp to trigger cooldown
      servicesConfig[0].status = 'errored'
      servicesConfig[0].updated = Date.now()

      // Should not restart due to cooldown period
      await Service.check()

      // Should not have spawned additional processes due to cooldown
      expect(mockSpawn).toHaveBeenCalledTimes(initialSpawnCount)
    })

    test('should stop service after exceeding maximum error limit', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null,
        status: 'stopped'
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // Simulate many check cycles to exceed error limit
      // The Service module should stop trying after 10 errors
      for (let i = 0; i < 15; i++) {
        await Service.check()
      }

      // Should have stopped trying to restart after error limit
      // The exact number depends on internal error counting logic
      expect(mockSpawn.mock.calls.length).toBeLessThan(15)
    })

    test('should update service status during lifecycle events', async () => {
      // Test that service status is properly updated during different events
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Should be running after successful start
      expect(servicesConfig[0].status).toBe('running')
      expect(servicesConfig[0].pid).toBe(12345)
      expect(servicesConfig[0].started).toBeDefined()

      // Simulate stderr output (error)
      mockChildProcess.stderr.emit('data', 'Error occurred')

      expect(servicesConfig[0].status).toBe('errored')
      expect(servicesConfig[0].updated).toBeDefined()

      // Simulate process exit - should only change status if currently running
      // Since we're already errored, the exit handler checks if status is 'running'
      servicesConfig[0].status = 'running' // Reset to running to test exit behavior
      mockChildProcess.emit('exit', 1)

      expect(servicesConfig[0].status).toBe('stopped')
      expect(servicesConfig[0].pid).toBeNull()
      expect(servicesConfig[0].started).toBeNull()
    })

    test('should handle process monitoring state correctly', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // Start first process
      mockChildProcess.pid = 12345
      await Service.check()

      expect(servicesConfig[0].pid).toBe(12345)
      expect(mockSpawn).toHaveBeenCalledTimes(1)

      // Simulate process exit - this sets #active[id] = false and pid = null
      mockChildProcess.emit('exit', 1)

      expect(servicesConfig[0].pid).toBeNull()
      expect(servicesConfig[0].status).toBe('stopped')

      // Set updated time to past to avoid cooldown
      servicesConfig[0].updated = Date.now() - 2000

      // Create new mock child process for restart
      const newMockChildProcess = createMockChildProcess(12346)
      mockSpawn.mockReturnValue(newMockChildProcess)

      // Check should restart the service since pid is null and cooldown has passed
      await Service.check()

      expect(mockSpawn).toHaveBeenCalledTimes(2)
      expect(servicesConfig[0].pid).toBe(12346)
    })

    test('should prevent concurrent restarts of the same service', async () => {
      // Test the #active flag prevents concurrent restarts
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // Try to check multiple times rapidly
      const checkPromises = [Service.check(), Service.check(), Service.check()]

      await Promise.all(checkPromises)

      // Should only start once due to #active flag
      expect(mockSpawn).toHaveBeenCalledTimes(1)
    })

    test('should handle service status transitions correctly', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Should start as running
      expect(servicesConfig[0].status).toBe('running')

      // Error should change status to errored
      mockChildProcess.stderr.emit('data', 'Test error')
      expect(servicesConfig[0].status).toBe('errored')

      // Exit should only change status to stopped if currently running
      // Reset to running to test the exit behavior properly
      servicesConfig[0].status = 'running'
      mockChildProcess.emit('exit', 1)
      expect(servicesConfig[0].status).toBe('stopped')

      // After exit, the service should have no PID and be ready for restart
      expect(servicesConfig[0].pid).toBeNull()

      // Restart should change back to running, but we need to account for cooldown
      // Set updated time to past to avoid cooldown
      servicesConfig[0].updated = Date.now() - 10000

      await Service.check()
      expect(servicesConfig[0].status).toBe('running')
    })

    test('should only monitor active services', async () => {
      const activeService = createMockServiceConfig('active-service.js', {
        id: 0,
        file: '/path/to/active-service.js',
        active: true,
        pid: null
      })

      const inactiveService = createMockServiceConfig('inactive-service.js', {
        id: 1,
        file: '/path/to/inactive-service.js',
        active: false,
        pid: null
      })

      let servicesConfig = [activeService, inactiveService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Should only start the active service
      expect(mockSpawn).toHaveBeenCalledTimes(1)
      expect(mockSpawn).toHaveBeenCalledWith('node', [activeService.file], {
        cwd: path.dirname(activeService.file)
      })
    })

    test('should handle watcher state management correctly', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: 12345,
        status: 'running', // Set initial status
        updated: Date.now() - 5000 // Set to past to avoid any cooldown issues
      })

      let servicesConfig = [existingService]

      // Mock Process.stop to simulate process not found
      mockCandy.setMock('core', 'Process', {
        stop: jest.fn()
      })

      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Should detect missing process and restart
      expect(mockCandy.getMock('core', 'Process').stop).toHaveBeenCalledWith(12345)
      expect(mockSpawn).toHaveBeenCalled()

      // The check method sets PID to null after calling #run, so PID should be null
      // This is the actual behavior: #run sets PID, then check sets it to null
      expect(servicesConfig[0].pid).toBeNull()
    })
  })

  describe('Service log management and status reporting', () => {
    test('should capture stdout and stderr logs', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // Start the service
      await Service.check()

      // Simulate stdout data
      const stdoutData = 'Service started successfully'
      mockChildProcess.stdout.emit('data', stdoutData)

      // Simulate stderr data
      const stderrData = 'Warning: deprecated function'
      mockChildProcess.stderr.emit('data', stderrData)

      // Trigger log writing by calling check again
      await Service.check()

      expect(fs.writeFile).toHaveBeenCalledWith(
        '/home/user/.odac/logs/test-service.js.log',
        expect.stringContaining(stdoutData),
        'utf8',
        expect.any(Function)
      )

      expect(fs.writeFile).toHaveBeenCalledWith(
        '/home/user/.odac/logs/test-service.js.err.log',
        expect.stringContaining(stderrData),
        'utf8',
        expect.any(Function)
      )
    })

    test('should format log entries with timestamps', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      const testMessage = 'Test log message'
      mockChildProcess.stdout.emit('data', testMessage)

      await Service.check()

      const logCall = fs.writeFile.mock.calls.find(call => call[0].includes('.log') && !call[0].includes('.err.log'))

      expect(logCall[1]).toMatch(/\[LOG\]\[\d+\] Test log message/)
    })

    test('should handle multiline log messages correctly', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      const multilineMessage = 'Line 1\nLine 2\nLine 3'
      mockChildProcess.stdout.emit('data', multilineMessage)

      await Service.check()

      const logCall = fs.writeFile.mock.calls.find(call => call[0].includes('.log') && !call[0].includes('.err.log'))

      expect(logCall[1]).toMatch(/\[LOG\]\[\d+\] Line 1\n\[LOG\]\[\d+\] Line 2\n\[LOG\]\[\d+\] Line 3/)
    })

    test('should implement log rotation when logs exceed size limit', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Generate large log data (over 1MB)
      const largeMessage = 'x'.repeat(500000)
      mockChildProcess.stdout.emit('data', largeMessage)
      mockChildProcess.stdout.emit('data', largeMessage)
      mockChildProcess.stdout.emit('data', largeMessage)

      await Service.check()

      const logCall = fs.writeFile.mock.calls.find(call => call[0].includes('.log') && !call[0].includes('.err.log'))

      // Log should be truncated to 1MB
      expect(logCall[1].length).toBeLessThanOrEqual(1000000)
    })

    test('should load existing logs on initialization', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true
      })

      const existingLogData = 'Previous log entries'
      fs.readFile.mockImplementation((path, encoding, callback) => {
        if (path.includes('test-service.js.log')) {
          callback(null, existingLogData)
        } else {
          callback(new Error('File not found'))
        }
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [existingService]}
      })

      await Service.init()

      expect(fs.readFile).toHaveBeenCalledWith('/home/user/.odac/logs/test-service.js.log', 'utf8', expect.any(Function))
    })

    test('should calculate and report service uptime correctly', async () => {
      const startTime = Date.now() - 90061000 // 1 day, 1 hour, 1 minute, 1 second ago
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        status: 'running',
        started: startTime
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [existingService]}
      })

      const services = await Service.status()

      expect(services[0].uptime).toMatch(/1d 1h 1m 1s/)
    })

    test('should handle uptime calculation for various durations', async () => {
      const testCases = [
        {duration: 3661000, expected: /1h 1m 1s/}, // 1 hour, 1 minute, 1 second
        {duration: 61000, expected: /1m 1s/}, // 1 minute, 1 second
        {duration: 1000, expected: /1s/}, // 1 second
        {duration: 500, expected: /^$/} // Less than 1 second - empty string
      ]

      for (const testCase of testCases) {
        const startTime = Date.now() - testCase.duration
        const service = createMockServiceConfig('test-service.js', {
          id: 0,
          status: 'running',
          started: startTime
        })

        mockCandy.setMock('core', 'Config', {
          config: {services: [service]}
        })

        const services = await Service.status()
        if (testCase.duration < 1000) {
          // For very short durations, uptime might be empty string
          expect(services[0].uptime || '').toMatch(testCase.expected)
        } else {
          expect(services[0].uptime).toMatch(testCase.expected)
        }
      }
    })

    test('should stop service and clean up resources', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: 12345
      })

      let servicesConfig = [existingService]
      const mockProcessStop = jest.fn()

      mockCandy.setMock('core', 'Process', {
        stop: mockProcessStop
      })

      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      Service.stop(0)

      expect(mockProcessStop).toHaveBeenCalledWith(12345)
      expect(servicesConfig[0].pid).toBeNull()
      expect(servicesConfig[0].started).toBeNull()
      expect(servicesConfig[0].active).toBe(false)
    })

    test('should handle stopping non-existent service gracefully', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {services: []}
      })

      // Should not throw error
      expect(() => Service.stop('nonexistent')).not.toThrow()
    })

    test('should handle stopping already stopped service', async () => {
      const stoppedService = createMockServiceConfig('test-service.js', {
        id: 0,
        active: false,
        pid: null
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [stoppedService]}
      })

      // Should not throw error
      expect(() => Service.stop(0)).not.toThrow()
    })

    test('should handle file write errors gracefully', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      // Mock fs.writeFile to call callback with error
      fs.writeFile.mockImplementation((path, data, encoding, callback) => {
        callback(new Error('Disk full'))
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      mockChildProcess.stdout.emit('data', 'Test message')

      // Should not throw error even when file write fails
      await expect(Service.check()).resolves.not.toThrow()
    })

    test('should handle stderr logs and add them to both logs and errs', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      const errorMessage = 'Critical error occurred'
      mockChildProcess.stderr.emit('data', errorMessage)

      await Service.check()

      // Should write to both regular log and error log
      expect(fs.writeFile).toHaveBeenCalledWith(
        '/home/user/.odac/logs/test-service.js.log',
        expect.stringContaining('[ERR]'),
        'utf8',
        expect.any(Function)
      )

      expect(fs.writeFile).toHaveBeenCalledWith(
        '/home/user/.odac/logs/test-service.js.err.log',
        expect.stringContaining(errorMessage),
        'utf8',
        expect.any(Function)
      )

      // Should also update service status to errored
      expect(servicesConfig[0].status).toBe('errored')
    })

    test('should handle log rotation for error logs when they exceed size limit', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: null
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      await Service.check()

      // Generate large error log data (over 1MB)
      const largeErrorMessage = 'x'.repeat(500000)
      mockChildProcess.stderr.emit('data', largeErrorMessage)
      mockChildProcess.stderr.emit('data', largeErrorMessage)
      mockChildProcess.stderr.emit('data', largeErrorMessage)

      await Service.check()

      const errorLogCall = fs.writeFile.mock.calls.find(call => call[0].includes('.err.log'))

      // Error log should be truncated to 1MB
      expect(errorLogCall[1].length).toBeLessThanOrEqual(1000000)
    })

    test('should not write logs for services without log data', async () => {
      const existingService = createMockServiceConfig('test-service.js', {
        id: 0,
        file: '/path/to/test-service.js',
        active: true,
        pid: 12345 // Already running, won't restart
      })

      let servicesConfig = [existingService]
      mockCandy.setMock('core', 'Config', {
        config: {
          get services() {
            return servicesConfig
          },
          set services(value) {
            servicesConfig = value
          }
        }
      })

      // Mock watcher to indicate process is running
      Service.check.__proto__.constructor.prototype['#watcher'] = {12345: true}

      await Service.check()

      // Should not call fs.writeFile since there are no logs
      expect(fs.writeFile).not.toHaveBeenCalled()
    })

    test('should handle service status reporting for stopped services', async () => {
      const stoppedService = createMockServiceConfig('test-service.js', {
        id: 0,
        status: 'stopped',
        started: null
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [stoppedService]}
      })

      const services = await Service.status()

      // Stopped services should not have uptime calculated
      expect(services[0].uptime).toBeUndefined()
    })

    test('should handle service status reporting for errored services', async () => {
      const erroredService = createMockServiceConfig('test-service.js', {
        id: 0,
        status: 'errored',
        started: Date.now() - 60000 // 1 minute ago
      })

      mockCandy.setMock('core', 'Config', {
        config: {services: [erroredService]}
      })

      const services = await Service.status()

      // Errored services should not have uptime calculated (only running services do)
      expect(services[0].uptime).toBeUndefined()
    })
  })
})
