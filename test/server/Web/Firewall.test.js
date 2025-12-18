// Mock dependencies
const mockLog = {
  log: jest.fn(),
  error: jest.fn(),
  init: jest.fn().mockReturnThis()
}

// Global config store
let mockConfigData = {
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

// Mock Odac global
global.Odac = {
  core: jest.fn(module => {
    if (module === 'Log') return mockLog
    if (module === 'Config')
      return {
        config: mockConfigData
      }
    return {}
  })
}

const Firewall = require('../../../server/src/Web/Firewall.js')

describe('Firewall', () => {
  let firewall

  beforeEach(() => {
    jest.clearAllMocks()
    // Reset config
    mockConfigData.firewall = {
      enabled: true,
      rateLimit: {
        enabled: true,
        windowMs: 1000,
        max: 2
      },
      blacklist: [],
      whitelist: []
    }
    firewall = new Firewall()
  })

  test('should allow requests from normal IPs', () => {
    const req = {socket: {remoteAddress: '127.0.0.1'}, headers: {}}
    expect(firewall.check(req).allowed).toBe(true)
  })

  test('should block requests from blacklisted IPs', () => {
    firewall.addBlock('1.2.3.4')
    const req = {socket: {remoteAddress: '1.2.3.4'}, headers: {}}
    const result = firewall.check(req)
    expect(result.allowed).toBe(false)
    expect(result.reason).toBe('blacklist')
  })

  test('should allow requests from whitelisted IPs even if rate limited', () => {
    // Mock rate limit config to be very strict
    mockConfigData.firewall.rateLimit.max = 0
    firewall = new Firewall() // reload config

    firewall.addWhitelist('1.2.3.4')

    const req = {socket: {remoteAddress: '1.2.3.4'}, headers: {}}

    expect(firewall.check(req).allowed).toBe(true)
  })

  test('should enforce rate limits', () => {
    const req = {socket: {remoteAddress: '10.0.0.1'}, headers: {}}

    // Config is max 2 per 1000ms
    expect(firewall.check(req).allowed).toBe(true) // 1
    expect(firewall.check(req).allowed).toBe(true) // 2
    const result = firewall.check(req) // 3 - blocked
    expect(result.allowed).toBe(false)
    expect(result.reason).toBe('rate_limit')
  })

  test('should reset rate limits after window', async () => {
    const req = {socket: {remoteAddress: '10.0.0.2'}, headers: {}}

    expect(firewall.check(req).allowed).toBe(true) // 1
    expect(firewall.check(req).allowed).toBe(true) // 2
    expect(firewall.check(req).allowed).toBe(false) // 3

    // Wait for window to pass (1000ms)
    await new Promise(resolve => setTimeout(resolve, 1100))

    expect(firewall.check(req).allowed).toBe(true) // Should be allowed again
  })

  test('should handle IPv6 mapped IPv4 addresses', () => {
    const req = {socket: {remoteAddress: '::ffff:127.0.0.1'}, headers: {}}
    expect(firewall.check(req).allowed).toBe(true)

    firewall.addBlock('127.0.0.1')
    expect(firewall.check(req).allowed).toBe(false)
  })

  test('should use x-forwarded-for if socket address is missing', () => {
    const req = {socket: {}, headers: {'x-forwarded-for': '1.2.3.4'}}
    firewall.addBlock('1.2.3.4')
    expect(firewall.check(req).allowed).toBe(false)
  })

  test('should handle x-forwarded-for with multiple IPs', () => {
    // First IP is client
    const req = {socket: {}, headers: {'x-forwarded-for': '1.2.3.4, 5.6.7.8'}}
    firewall.addBlock('1.2.3.4')
    const result = firewall.check(req)
    expect(result.allowed).toBe(false)
    expect(result.reason).toBe('blacklist')
  })

  test('should handle x-forwarded-for with spaces', () => {
    const req = {socket: {}, headers: {'x-forwarded-for': ' 1.2.3.4 , 5.6.7.8 '}}
    firewall.addBlock('1.2.3.4')
    expect(firewall.check(req).allowed).toBe(false)
  })

  test('should allow everything when disabled', () => {
    mockConfigData.firewall.enabled = false
    firewall = new Firewall() // reload config

    firewall.addBlock('1.2.3.4') // even if blocked
    const req = {socket: {remoteAddress: '1.2.3.4'}, headers: {}}

    expect(firewall.check(req).allowed).toBe(true)
  })

  test('should remove block', () => {
    firewall.addBlock('1.2.3.4')
    const req = {socket: {remoteAddress: '1.2.3.4'}, headers: {}}
    expect(firewall.check(req).allowed).toBe(false)

    firewall.removeBlock('1.2.3.4')
    expect(firewall.check(req).allowed).toBe(true)
  })

  test('should remove whitelist', () => {
    // Set strict rate limit
    mockConfigData.firewall.rateLimit.max = 0
    firewall = new Firewall()

    firewall.addWhitelist('1.2.3.4')
    const req = {socket: {remoteAddress: '1.2.3.4'}, headers: {}}
    expect(firewall.check(req).allowed).toBe(true)

    firewall.removeWhitelist('1.2.3.4')
    expect(firewall.check(req).allowed).toBe(false) // rate limited
  })

  test('should persist changes to config', () => {
    firewall.addBlock('1.1.1.1')
    expect(mockConfigData.firewall.blacklist).toContain('1.1.1.1')

    firewall.addWhitelist('2.2.2.2')
    expect(mockConfigData.firewall.whitelist).toContain('2.2.2.2')

    firewall.removeBlock('1.1.1.1')
    expect(mockConfigData.firewall.blacklist).not.toContain('1.1.1.1')
  })
})
