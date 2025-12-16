const mockLog = jest.fn()
const mockError = jest.fn()

const {mockCandy} = require('./__mocks__/globalCandy')

mockCandy.setMock('core', 'Log', {
  init: jest.fn().mockReturnValue({
    log: mockLog,
    error: mockError
  })
})

global.Candy = mockCandy

jest.mock('axios')
const axios = require('axios')

jest.mock('ws')

jest.mock('os')
const os = require('os')

jest.mock('fs')
const fs = require('fs')

jest.useFakeTimers()

describe('Hub', () => {
  let Hub

  beforeEach(() => {
    jest.clearAllMocks()
    jest.clearAllTimers()

    mockCandy.setMock('core', 'Config', {
      config: {
        hub: null,
        server: {started: Date.now()},
        websites: {},
        services: [],
        mail: {accounts: {}}
      }
    })

    mockCandy.setMock('server', 'Api', {
      result: jest.fn((success, message) => ({success, message}))
    })

    os.hostname.mockReturnValue('test-host')
    os.platform.mockReturnValue('linux')
    os.arch.mockReturnValue('x64')
    os.totalmem.mockReturnValue(8589934592)
    os.freemem.mockReturnValue(4294967296)
    os.cpus.mockReturnValue([
      {times: {user: 1000, nice: 0, sys: 500, idle: 8500, irq: 0}},
      {times: {user: 1000, nice: 0, sys: 500, idle: 8500, irq: 0}}
    ])

    jest.isolateModules(() => {
      Hub = require('../../server/src/Hub')
    })
  })

  afterEach(() => {
    if (Hub.websocket) {
      Hub.websocket = null
    }
    Hub.checkCounter = 0
  })

  describe('initialization', () => {
    it('should initialize with default values', () => {
      expect(Hub.websocket).toBeNull()
      expect(Hub.websocketReconnectAttempts).toBe(0)
      expect(Hub.maxReconnectAttempts).toBe(5)
      expect(Hub.checkCounter).toBe(0)
    })
  })

  describe('check counter', () => {
    it('should increment counter on each check', () => {
      expect(Hub.checkCounter).toBe(0)
      Hub.check()
      expect(Hub.checkCounter).toBe(1)
      Hub.check()
      expect(Hub.checkCounter).toBe(2)
    })

    it('should reset counter after reaching 60', () => {
      Hub.checkCounter = 59
      Hub.check()
      expect(Hub.checkCounter).toBe(0)
    })

    it('should skip API call when counter is not 0', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Hub.checkCounter = 1
      await Hub.check()
      expect(axios.post).not.toHaveBeenCalled()
    })

    it('should skip API call when websocket is connected', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Hub.websocket = {readyState: 1}
      Hub.checkCounter = 0
      await Hub.check()
      expect(axios.post).not.toHaveBeenCalled()
    })
  })

  describe('check', () => {
    beforeEach(() => {
      Hub.checkCounter = 0
    })

    it('should return early if no hub config', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {hub: null}
      })

      await Hub.check()
      expect(axios.post).not.toHaveBeenCalled()
    })

    it('should return early if no token', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {hub: {}}
      })

      await Hub.check()
      expect(axios.post).not.toHaveBeenCalled()
    })

    it('should send status to hub when counter is 0', async () => {
      jest.useRealTimers()

      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {authenticated: true}
        }
      })

      Hub.checkCounter = 59
      Hub.check()

      await new Promise(resolve => setImmediate(resolve))
      expect(axios.post).toHaveBeenCalled()

      jest.useFakeTimers()
    })

    it('should handle authentication failure', async () => {
      jest.useRealTimers()

      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'invalid-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {authenticated: false, reason: 'token_invalid'}
        }
      })

      Hub.checkCounter = 59
      Hub.check()

      await new Promise(resolve => setImmediate(resolve))
      expect(mockLog).toHaveBeenCalledWith('Server not authenticated: %s', 'token_invalid')

      jest.useFakeTimers()
    })

    it('should clear config on invalid token', async () => {
      jest.useRealTimers()

      const config = {
        hub: {token: 'invalid-token', secret: 'test-secret'},
        server: {started: Date.now()}
      }
      mockCandy.setMock('core', 'Config', {config})

      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {authenticated: false, reason: 'token_invalid'}
        }
      })

      Hub.checkCounter = 59
      Hub.check()

      await new Promise(resolve => setImmediate(resolve))
      expect(config.hub).toBeUndefined()

      jest.useFakeTimers()
    })

    it('should handle check errors gracefully', async () => {
      jest.useRealTimers()

      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      axios.post.mockRejectedValue(new Error('Network error'))

      Hub.checkCounter = 59
      Hub.check()

      await new Promise(resolve => setImmediate(resolve))
      expect(mockLog).toHaveBeenCalledWith('Failed to report status: %s', 'Network error')

      jest.useFakeTimers()
    })
  })

  describe('authentication', () => {
    it('should authenticate with valid code', async () => {
      const mockResponse = {
        data: {
          result: {success: true},
          data: {
            token: 'new-token',
            secret: 'new-secret'
          }
        }
      }

      const mockApiResult = {success: true, message: 'Authentication successful'}
      mockCandy.setMock('server', 'Api', {
        result: jest.fn(() => mockApiResult)
      })

      axios.post.mockResolvedValue(mockResponse)

      const result = await Hub.auth('valid-code')

      expect(axios.post).toHaveBeenCalled()
      expect(result).toEqual(mockApiResult)
      expect(mockCandy.core('Config').config.hub).toEqual({
        token: 'new-token',
        secret: 'new-secret'
      })
    })

    it('should handle authentication failure', async () => {
      const mockApiResult = {success: false, message: 'Authentication failed'}
      mockCandy.setMock('server', 'Api', {
        result: jest.fn(() => mockApiResult)
      })

      axios.post.mockRejectedValue(new Error('Invalid code'))

      const result = await Hub.auth('invalid-code')

      expect(result).toEqual(mockApiResult)
      expect(mockLog).toHaveBeenCalledWith('Authentication failed: %s', 'Invalid code')
    })

    it('should include distro info on Linux', async () => {
      os.platform.mockReturnValue('linux')
      fs.readFileSync.mockReturnValue('NAME="Ubuntu"\nVERSION_ID="20.04"\nID=ubuntu')

      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {token: 'token', secret: 'secret'}
        }
      })

      await Hub.auth('code')

      const callArgs = axios.post.mock.calls[0][1]
      expect(callArgs.distro).toBeDefined()
      expect(callArgs.distro.name).toBe('Ubuntu')
    })
  })

  describe('system status', () => {
    it('should get system status', () => {
      const status = Hub.getSystemStatus()

      expect(status).toHaveProperty('cpu')
      expect(status).toHaveProperty('memory')
      expect(status).toHaveProperty('disk')
      expect(status).toHaveProperty('network')
      expect(status).toHaveProperty('services')
      expect(status).toHaveProperty('uptime')
      expect(status.hostname).toBe('test-host')
      expect(status.platform).toBe('linux')
      expect(status.arch).toBe('x64')
    })

    it('should get services info', () => {
      mockCandy.setMock('core', 'Config', {
        config: {
          websites: {
            'example.com': {},
            'test.com': {}
          },
          services: ['web', 'mail'],
          mail: {
            accounts: {
              'user@example.com': {},
              'admin@test.com': {}
            }
          }
        }
      })

      const services = Hub.getServicesInfo()

      expect(services.websites).toBe(2)
      expect(services.services).toBe(2)
      expect(services.mail).toBe(2)
    })

    it('should handle missing services config', () => {
      mockCandy.setMock('core', 'Config', {config: {}})

      const services = Hub.getServicesInfo()

      expect(services.websites).toBe(0)
      expect(services.services).toBe(0)
      expect(services.mail).toBe(0)
    })
  })

  describe('memory usage', () => {
    it('should get memory usage on Linux', () => {
      os.platform.mockReturnValue('linux')
      os.totalmem.mockReturnValue(8589934592)
      os.freemem.mockReturnValue(4294967296)

      const memory = Hub.getMemoryUsage()

      expect(memory.total).toBe(8589934592)
      expect(memory.used).toBe(4294967296)
    })
  })

  describe('CPU usage', () => {
    it('should return 0 on first call', () => {
      const usage = Hub.getCpuUsage()
      expect(usage).toBe(0)
    })

    it('should calculate CPU usage on subsequent calls', () => {
      Hub.getCpuUsage()

      os.cpus.mockReturnValue([
        {times: {user: 2000, nice: 0, sys: 1000, idle: 7000, irq: 0}},
        {times: {user: 2000, nice: 0, sys: 1000, idle: 7000, irq: 0}}
      ])

      const usage = Hub.getCpuUsage()
      expect(usage).toBeGreaterThanOrEqual(0)
      expect(usage).toBeLessThanOrEqual(100)
    })
  })

  describe('request signing', () => {
    it('should sign request with secret', () => {
      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'token', secret: 'test-secret'}
        }
      })

      const data = {test: 'data'}
      const signature = Hub.signRequest(data)

      expect(signature).toBeTruthy()
      expect(typeof signature).toBe('string')
    })

    it('should return null without secret', () => {
      mockCandy.setMock('core', 'Config', {
        config: {hub: {token: 'token'}}
      })

      const signature = Hub.signRequest({test: 'data'})
      expect(signature).toBeNull()
    })
  })

  describe('API calls', () => {
    it('should make successful API call', async () => {
      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {response: 'data'}
        }
      })

      const result = await Hub.call('test-action', {param: 'value'})

      expect(result).toEqual({response: 'data'})
      expect(axios.post).toHaveBeenCalledWith('https://hub.odac.run/test-action', {param: 'value'}, expect.any(Object))
    })

    it('should include authorization header when token exists', async () => {
      mockCandy.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'}
        }
      })

      axios.post.mockResolvedValue({
        data: {
          result: {success: true},
          data: {}
        }
      })

      await Hub.call('test', {})

      const callArgs = axios.post.mock.calls[0][2]
      expect(callArgs.headers.Authorization).toBe('Bearer test-token')
    })

    it('should handle API errors', async () => {
      axios.post.mockResolvedValue({
        data: {
          result: {success: false, message: 'API error'}
        }
      })

      await expect(Hub.call('test', {})).rejects.toBe('API error')
    })

    it('should handle network errors', async () => {
      axios.post.mockRejectedValue({
        response: {status: 500, data: 'Server error'}
      })

      await expect(Hub.call('test', {})).rejects.toBe('Server error')
    })
  })

  describe('Linux distro detection', () => {
    it('should return null on non-Linux platforms', () => {
      os.platform.mockReturnValue('darwin')
      const distro = Hub.getLinuxDistro()
      expect(distro).toBeNull()
    })

    it('should parse os-release file', () => {
      os.platform.mockReturnValue('linux')
      fs.readFileSync.mockReturnValue('NAME="Ubuntu"\nVERSION_ID="20.04"\nID=ubuntu')

      const distro = Hub.getLinuxDistro()

      expect(distro.name).toBe('Ubuntu')
      expect(distro.version).toBe('20.04')
      expect(distro.id).toBe('ubuntu')
    })

    it('should handle missing os-release file', () => {
      os.platform.mockReturnValue('linux')
      fs.readFileSync.mockImplementation(() => {
        throw new Error('File not found')
      })

      const distro = Hub.getLinuxDistro()
      expect(distro).toBeNull()
    })
  })
})
