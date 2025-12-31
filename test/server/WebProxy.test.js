const {mockOdac} = require('./__mocks__/globalOdac')

// Mock modules before requiring Web
jest.mock('fs')
jest.mock('child_process')
jest.mock('axios', () => ({
  post: jest.fn(() => Promise.resolve())
}))

// Setup global Odac before everything
global.Odac = mockOdac

// Mock dependencies of Web.js
jest.mock(
  '../../server/src/Web/Firewall.js',
  () =>
    class {
      check() {
        return {allowed: true}
      }
    }
)

// Import mocked modules
const childProcess = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')
const axios = require('axios')

describe('Web Proxy Integration', () => {
  let Web
  let mockSpawn
  let mockStdout
  let mockStderr

  beforeEach(() => {
    jest.clearAllMocks()
    jest.useFakeTimers()

    // Setup fs mock
    fs.existsSync.mockReturnValue(true)

    // Setup child process mock
    mockStdout = {on: jest.fn()}
    mockStderr = {on: jest.fn()}
    mockSpawn = {
      stdout: mockStdout,
      stderr: mockStderr,
      on: jest.fn(),
      unref: jest.fn(),
      kill: jest.fn()
    }
    childProcess.spawn.mockReturnValue(mockSpawn)

    // Setup Odac mock
    mockOdac.core = jest.fn(module => {
      if (module === 'Config') return {config: {websites: {}, firewall: {enabled: true}, web: {}, ssl: null}}
      if (module === 'Log') return {init: () => ({log: jest.fn(), error: jest.fn()})}
      return {}
    })

    // Import Web module (singleton)
    Web = require('../../server/src/Web')
    // Reset internal state
    if (Web.reset) Web.reset()
  })

  afterEach(() => {
    jest.clearAllTimers()
    jest.useRealTimers()
  })

  test('should spawn proxy binary using unix socket on Linux/macOS', () => {
    // Mock os.platform for this test
    const originalPlatform = os.platform
    os.platform = jest.fn(() => 'linux')
    os.tmpdir = jest.fn(() => '/tmp')

    Web.spawnProxy()

    expect(childProcess.spawn).toHaveBeenCalledWith(
      expect.stringContaining('odac-proxy'),
      [],
      expect.objectContaining({
        env: expect.objectContaining({
          ODAC_SOCKET_PATH: expect.stringMatching(/odac.*\.sock/)
        })
      })
    )

    // Restore
    os.platform = originalPlatform
  })

  test('should spawn proxy binary using TCP on Windows', () => {
    // Mock os.platform for Windows
    const originalPlatform = os.platform
    os.platform = jest.fn(() => 'win32')

    Web.spawnProxy()

    // On Windows, ODAC_SOCKET_PATH should NOT be in env
    const spawnCall = childProcess.spawn.mock.calls[0]
    if (spawnCall) {
      const spawnEnv = spawnCall[2]?.env || {}
      expect(spawnEnv.ODAC_SOCKET_PATH).toBeUndefined()
      expect(spawnCall[0]).toContain('odac-proxy')
    }

    // Restore
    os.platform = originalPlatform
  })

  test('should sync config via Axios with Unix Socket', () => {
    const originalPlatform = os.platform
    os.platform = jest.fn(() => 'linux')
    os.tmpdir = jest.fn(() => '/tmp')

    Web.spawnProxy()

    // Advance timers to trigger syncConfig (it has setTimeout 500ms)
    jest.advanceTimersByTime(1000)

    expect(axios.post).toHaveBeenCalledWith(
      'http://localhost/config',
      expect.any(Object),
      expect.objectContaining({
        socketPath: expect.stringMatching(/odac.*\.sock/),
        validateStatus: expect.any(Function)
      })
    )

    // Restore
    os.platform = originalPlatform
  })
})
