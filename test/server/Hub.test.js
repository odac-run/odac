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
const WebSocketLib = require('ws')

jest.mock('os')
const os = require('os')
jest.mock('fs')
const fs = require('fs')

const {cleanupGlobalMocks} = require('./__mocks__/testHelpers')

jest.setTimeout(15000)

describe('Hub', () => {
  let Hub
  let System
  let MessageSigner

  beforeEach(() => {
    jest.clearAllMocks()
    jest.clearAllTimers()

    mockOdac.setMock('core', 'Config', {
      config: {
        hub: null,
        server: {started: Date.now()},
        domains: {},
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
      Hub.start()
      for (const task of Hub.tasks) task.lastRun = Date.now()
      System = require('../../server/src/Hub/System')
      const WS = require('../../server/src/Hub/WebSocket')
      MessageSigner = WS.MessageSigner
    })
  })

  afterEach(() => {
    if (Hub.ws && Hub.ws.socket) {
      Hub.ws.disconnect()
    }
    Hub.checkCounter = 0
  })

  describe('initialization', () => {
    it('should initialize with default values', () => {
      expect(Hub.ws).toBeDefined()
      expect(Hub.ws.connected).toBe(false)
      expect(Hub.tasks).toBeInstanceOf(Array)
    })
  })

  describe('tasks interval', () => {
    it('should skip task if interval not reached', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Object.defineProperty(Hub.ws, 'connected', {get: () => true})
      const sendSpy = jest.spyOn(Hub.ws, 'send').mockReturnValue(true)

      for (const t of Hub.tasks) t.lastRun = Date.now()
      const task = Hub.tasks.find(t => t.name === 'app.stats')
      task.lastRun = Date.now()

      sendSpy.mockClear()
      await Hub.check()
      expect(sendSpy).not.toHaveBeenCalled()
    })

    it('should execute task if interval reached', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()}
        }
      })

      Object.defineProperty(Hub.ws, 'connected', {get: () => true})
      const sendSpy = jest.spyOn(Hub.ws, 'send').mockReturnValue(true)

      mockOdac.setMock('server', 'App', {
        list: jest.fn().mockResolvedValue({result: true, data: []})
      })

      for (const t of Hub.tasks) t.lastRun = Date.now()
      const task = Hub.tasks.find(t => t.name === 'app.stats')
      task.lastRun = Date.now() - task.interval - 1

      sendSpy.mockClear()
      await Hub.check()
      // task fn is async, check might return before send
      await new Promise(resolve => setTimeout(resolve, 0))
      expect(sendSpy).toHaveBeenCalled()
    })
  })

  describe('check', () => {
    it('should return early if no hub config', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {hub: null}
      })

      const connectSpy = jest.spyOn(Hub.ws, 'connect')
      await Hub.check()
      expect(connectSpy).not.toHaveBeenCalled()
    })

    it('should return early if no token', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {hub: {}}
      })

      const connectSpy = jest.spyOn(Hub.ws, 'connect')
      await Hub.check()
      expect(connectSpy).not.toHaveBeenCalled()
    })

    it('should send status to hub when task triggers', async () => {
      jest.useRealTimers()

      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'},
          server: {started: Date.now()},
          domains: {'test-container': {}},
          apps: []
        }
      })

      mockOdac.setMock('server', 'Container', {
        available: true,
        list: jest.fn().mockResolvedValue([{id: '123', names: ['/test-container'], image: 'image'}]),
        getStats: jest.fn().mockResolvedValue({cpu: 10, memory: 100})
      })

      mockOdac.setMock('server', 'App', {
        list: jest.fn().mockResolvedValue({
          result: true,
          data: [{name: 'test-container', status: 'running'}]
        })
      })

      // Mock connected state and send
      Object.defineProperty(Hub.ws, 'connected', {get: () => true})
      const sendSpy = jest.spyOn(Hub.ws, 'send').mockReturnValue(true)

      // Find app.stats task and force execution
      const task = Hub.tasks.find(t => t.name === 'app.stats')
      task.lastRun = 0

      await Hub.check()

      // Wait for async operations to complete
      await new Promise(resolve => setTimeout(resolve, 50))

      expect(sendSpy).toHaveBeenCalled()

      const sendCalls = sendSpy.mock.calls
      const hasStats = sendCalls.some(call => {
        const data = typeof call[0] === 'string' ? JSON.parse(call[0]) : call[0]
        return data.type === 'app.stats'
      })
      expect(hasStats).toBe(true)

      jest.useFakeTimers()
    })

    it('should connect to websocket if not connected', async () => {
      mockOdac.setMock('core', 'Config', {
        config: {
          hub: {token: 'test-token', secret: 'test-secret'}
        }
      })

      WebSocketLib.mockClear()

      // Ensure it thinks it needs to reconnect
      jest.spyOn(Hub.ws, 'shouldReconnect').mockReturnValue(true)

      await Hub.check()

      expect(WebSocketLib).toHaveBeenCalledWith(
        'wss://hub.odac.run/ws',
        expect.objectContaining({
          headers: {Authorization: 'Bearer test-token'}
        })
      )
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
      // Expect an Error object as the second argument
      expect(mockLog).toHaveBeenCalledWith('Authentication failed: %s', expect.objectContaining({message: 'Invalid code'}))
    })
  })

  describe('system status', () => {
    it('should get system status via System module', () => {
      const status = System.getStatus()

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
          domains: {
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

      const services = System.getServicesInfo()

      expect(services.domains).toBe(2)
      expect(services.apps).toBe(2)
      expect(services.mail).toBe(2)
    })

    it('should handle missing services config', () => {
      mockOdac.setMock('core', 'Config', {config: {}})

      const services = System.getServicesInfo()

      expect(services.domains).toBe(0)
      expect(services.apps).toBe(0)
      expect(services.mail).toBe(0)
    })
  })

  describe('memory usage', () => {
    it('should get memory usage on Linux via System', () => {
      os.platform.mockReturnValue('linux')
      os.totalmem.mockReturnValue(8589934592)
      os.freemem.mockReturnValue(4294967296)

      const memory = System.getMemoryUsage()

      expect(memory.total).toBe(8589934592)
      expect(memory.used).toBe(4294967296)
    })
  })

  describe('request signing', () => {
    it('should sign request with secret using MessageSigner', () => {
      const data = {test: 'data'}
      const timestamp = Date.now()
      const secret = 'test-secret'

      const signature = MessageSigner.sign({type: 'test', data, timestamp}, secret)

      expect(signature).toBeTruthy()
      expect(typeof signature).toBe('string')
    })

    it('should return null without secret', () => {
      const signature = MessageSigner.sign({type: 'test', data: {}}, null)
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

      await expect(Hub.call('test', {})).rejects.toThrow('API error')
    })

    it('should handle network errors', async () => {
      axios.post.mockRejectedValue(new Error('Server error'))

      await expect(Hub.call('test', {})).rejects.toThrow('Server error')
    })
  })

  describe('Linux distro detection', () => {
    it('should return null on non-Linux platforms via System', () => {
      os.platform.mockReturnValue('darwin')
      const distro = System.getLinuxDistro()
      expect(distro).toBeNull()
    })

    it('should parse os-release file via System', () => {
      os.platform.mockReturnValue('linux')
      fs.readFileSync.mockReturnValue('NAME="Ubuntu"\nVERSION_ID="20.04"\nID=ubuntu')

      const distro = System.getLinuxDistro()

      expect(distro.name).toBe('Ubuntu')
      expect(distro.version).toBe('20.04')
      expect(distro.id).toBe('ubuntu')
    })

    it('should handle missing os-release file via System', () => {
      os.platform.mockReturnValue('linux')
      fs.readFileSync.mockImplementation(() => {
        throw new Error('File not found')
      })

      const distro = System.getLinuxDistro()
      expect(distro).toBeNull()
    })
  })
})
