const mockLog = jest.fn()
const mockError = jest.fn()

const {mockOdac} = require('./__mocks__/globalOdac')

mockOdac.setMock('core', 'Log', {
  init: jest.fn().mockReturnValue({
    log: mockLog,
    error: mockError
  })
})

global.Odac = mockOdac

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

    mockOdac.setMock('core', 'Config', {
      config: {
        hub: null,
        server: {started: Date.now()},
        websites: {},
        apps: [],
        mail: {accounts: {}}
      }
    })

    mockOdac.setMock('server', 'Api', {
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

    it('should reset counter after reaching 3600', () => {
      Hub.checkCounter = 3600
      Hub.check()
      expect(Hub.checkCounter).toBe(1)
    })

    it('should skip API call when counter is not 0', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Hub.checkCounter = 1
      Hub.websocket = {readyState: 1, send: jest.fn()}
      await Hub.check()
      expect(Hub.websocket.send).not.toHaveBeenCalled()
    })

    it('should skip API call when websocket is connected', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Hub.websocket = {readyState: 1, send: jest.fn()}
      Hub.checkCounter = 0
      await Hub.check()
      // Interval is 60, checkCounter becomes 1, so 1 % 60 != 0
      expect(Hub.websocket.send).not.toHaveBeenCalled()
    })
  })

  describe('check', () => {
    beforeEach(() => {
      Hub.checkCounter = 0
    })

    it('should return early if no hub config', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {hub: null}
      })

      await Hub.check()
      await Hub.check()
      expect(Hub.websocket).toBeNull()
    })

    it('should return early if no token', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {hub: {}}
      })

      // Mock call to ensure no connection attempt
      const WebSocket = require('ws')
      await Hub.check()
      expect(WebSocket).not.toHaveBeenCalled()
    })

    it('should send status to hub when interval matches', async () => {
      jest.useRealTimers()

      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()},
          websites: {'test-container': {}},
          apps: []
        }
      })

      mockOdac.setMock('server', 'Container', {
        available: true,
        list: jest.fn().mockResolvedValue([{id: '123', names: ['/test-container'], image: 'image'}]),
        getStats: jest.fn().mockResolvedValue({cpu: 10, memory: 100})
      })

      Hub.websocket = {
        readyState: 1,
        send: jest.fn(),
        on: jest.fn()
      }

      // statsInterval is 60
      Hub.checkCounter = 59
      await Hub.check()

      // Wait for async operations to complete
      await new Promise(resolve => setTimeout(resolve, 50))

      expect(Hub.websocket.send).toHaveBeenCalled()

      const sendCalls = Hub.websocket.send.mock.calls
      const hasStats = sendCalls.some(call => call[0].includes('container_stats'))
      expect(hasStats).toBe(true)

      jest.useFakeTimers()
    })

    it('should connect to websocket if not connected', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'}
        }
      })

      const WebSocket = require('ws')
      WebSocket.mockClear()

      Hub.websocket = null
      Hub.nextReconnectTime = 0

      await Hub.check()

      expect(WebSocket).toHaveBeenCalledWith(
        'wss://hub.odac.run/ws',
        expect.objectContaining({
          headers: {Authorization: 'Bearer test-token'}
        })
      )
    })

    it('should clear config on invalid token message', () => {
      const config = {
        hub: {token: 'invalid-token', secret: 'test-secret'},
        server: {started: Date.now()}
      }
      mockOdac.setMock('core', 'Config', {config})

      Hub.websocket = {
        close: jest.fn(),
        terminate: jest.fn()
      }
      Hub.stopHeartbeat = jest.fn()

      const disconnectMsg = JSON.stringify({
        type: 'disconnect',
        reason: 'token_invalid',
        timestamp: Math.floor(Date.now() / 1000)
      })

      // We need to bypass signature verification for this test or mock it
      jest.spyOn(Hub, 'verifyWebSocketMessage').mockReturnValue(true)

      Hub.handleWebSocketMessage(disconnectMsg)

      expect(config.hub).toBeUndefined()
      expect(Hub.websocket).toBeNull()
    })

    it('should handle websocket connection errors', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'}
        }
      })

      const WebSocket = require('ws')
      WebSocket.mockImplementation(() => {
        throw new Error('Connection failed')
      })

      Hub.websocket = null
      Hub.nextReconnectTime = 0

      await Hub.check()

      expect(mockLog).toHaveBeenCalledWith('Failed to connect WebSocket: %s', 'Connection failed')
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
      mockOdac.setMock('server', 'Api', {
        result: jest.fn(() => mockApiResult)
      })

      axios.post.mockResolvedValue(mockResponse)

      const result = await Hub.auth('valid-code')

      expect(axios.post).toHaveBeenCalled()
      expect(result).toEqual(mockApiResult)
      expect(mockOdac.core('Config').config.hub).toEqual({
        token: 'new-token',
        secret: 'new-secret'
      })
    })

    it('should handle authentication failure', async () => {
      const mockApiResult = {success: false, message: 'Authentication failed'}
      mockOdac.setMock('server', 'Api', {
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
      mockOdac.setMock('core', 'Config', {
        config: {
          websites: {
            'example.com': {},
            'test.com': {}
          },
          apps: ['web', 'mail'],
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
      expect(services.apps).toBe(2)
      expect(services.mail).toBe(2)
    })

    it('should handle missing services config', () => {
      mockOdac.setMock('core', 'Config', {config: {}})

      const services = Hub.getServicesInfo()

      expect(services.websites).toBe(0)
      expect(services.apps).toBe(0)
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
      mockOdac.setMock('core', 'Config', {
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
      mockOdac.setMock('core', 'Config', {
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
      mockOdac.setMock('core', 'Config', {
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
