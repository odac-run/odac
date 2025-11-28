
// Mock dependencies
const mockLog = {
  log: jest.fn(),
  error: jest.fn(),
  init: jest.fn().mockReturnThis()
}

// Mock Candy global
global.Candy = {
  core: jest.fn((module) => {
    if (module === 'Log') return mockLog
    if (module === 'Config') return {
      config: {
        firewall: {
            enabled: true,
            rateLimit: {
                enabled: true,
                windowMs: 1000,
                max: 2
            },
            blacklist: [],
            whitelist: []
        }
      }
    }
    return {}
  })
}

const Firewall = require('../../../server/src/Web/Firewall.js')

describe('Firewall', () => {
  let firewall

  beforeEach(() => {
    jest.clearAllMocks()
    firewall = new Firewall()
  })

  afterEach(() => {
     if (firewall.cleanupInterval) clearInterval(firewall.cleanupInterval)
  })

  test('should allow requests from normal IPs', () => {
    const req = { socket: { remoteAddress: '127.0.0.1' }, headers: {} }
    expect(firewall.check(req)).toBe(true)
  })

  test('should block requests from blacklisted IPs', () => {
    firewall.addBlock('1.2.3.4')
    const req = { socket: { remoteAddress: '1.2.3.4' }, headers: {} }
    expect(firewall.check(req)).toBe(false)
  })

  test('should allow requests from whitelisted IPs even if rate limited', () => {
      // Mock rate limit config to be very strict
      firewall = new Firewall()
      // Manually set low limit
      firewall.addWhitelist('1.2.3.4')

      const req = { socket: { remoteAddress: '1.2.3.4' }, headers: {} }

      // Send many requests
      expect(firewall.check(req)).toBe(true)
      expect(firewall.check(req)).toBe(true)
      expect(firewall.check(req)).toBe(true)
      expect(firewall.check(req)).toBe(true)
  })

  test('should enforce rate limits', () => {
      const req = { socket: { remoteAddress: '10.0.0.1' }, headers: {} }

      // Config is max 2 per 1000ms
      expect(firewall.check(req)).toBe(true) // 1
      expect(firewall.check(req)).toBe(true) // 2
      expect(firewall.check(req)).toBe(false) // 3 - blocked
  })

  test('should reset rate limits after window', async () => {
      const req = { socket: { remoteAddress: '10.0.0.2' }, headers: {} }

      expect(firewall.check(req)).toBe(true) // 1
      expect(firewall.check(req)).toBe(true) // 2
      expect(firewall.check(req)).toBe(false) // 3

      // Wait for window to pass (1000ms)
      await new Promise(resolve => setTimeout(resolve, 1100))

      expect(firewall.check(req)).toBe(true) // Should be allowed again
  })

  test('should handle IPv6 mapped IPv4 addresses', () => {
      const req = { socket: { remoteAddress: '::ffff:127.0.0.1' }, headers: {} }
      expect(firewall.check(req)).toBe(true)

      firewall.addBlock('127.0.0.1')
      expect(firewall.check(req)).toBe(false)
  })

  test('should use x-forwarded-for if socket address is missing', () => {
      const req = { socket: {}, headers: { 'x-forwarded-for': '1.2.3.4' } }
      firewall.addBlock('1.2.3.4')
      expect(firewall.check(req)).toBe(false)
  })
})
