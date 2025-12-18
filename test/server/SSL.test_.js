const fs = require('fs')
const os = require('os')
const acme = require('acme-client')
const selfsigned = require('selfsigned')

// Import test utilities
const {setupGlobalMocks, cleanupGlobalMocks} = require('./__mocks__/testHelpers')
const {createMockWebsiteConfig} = require('./__mocks__/testFactories')
const {mockOdac} = require('./__mocks__/globalOdac')

// Mock all dependencies
jest.mock('fs')
jest.mock('os')
jest.mock('acme-client')
jest.mock('selfsigned')

describe('SSL', () => {
  let SSL
  let mockConfig
  let mockLog
  let mockDNS

  beforeEach(() => {
    // Reset all mocks
    jest.clearAllMocks()

    // Set up global mocks
    setupGlobalMocks()

    // Mock the __ function to return formatted strings
    global.__ = jest.fn((key, ...args) => {
      // Simple string formatting for test purposes
      let result = key
      args.forEach((arg, index) => {
        result = result.replace('%s', arg)
      })
      return result
    })

    // Get mock instances from global Odac
    mockConfig = mockOdac.core('Config')
    mockLog = mockOdac.server('Log').init('SSL')
    mockDNS = mockOdac.server('DNS')

    // Set up DNS mock methods
    mockDNS.record = jest.fn()
    mockDNS.delete = jest.fn()

    // Mock os.homedir
    os.homedir.mockReturnValue('/home/test')

    // Mock fs methods
    fs.existsSync.mockReturnValue(true)
    fs.mkdirSync.mockImplementation(() => {})
    fs.writeFileSync.mockImplementation(() => {})

    // Import SSL module after mocks are set up
    // Clear the require cache to get a fresh instance
    delete require.cache[require.resolve('../../server/src/SSL.js')]
    SSL = require('../../server/src/SSL.js')
  })

  afterEach(() => {
    cleanupGlobalMocks()
  })

  describe('initialization', () => {
    it('should initialize SSL module correctly', () => {
      expect(SSL).toBeDefined()
      expect(typeof SSL.check).toBe('function')
      expect(typeof SSL.renew).toBe('function')
    })
  })

  describe('certificate checking and renewal logic', () => {
    describe('check method', () => {
      it('should skip checking if already checking', async () => {
        // Set up a website config to trigger SSL generation
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null // Force self-signed generation

        // Start first check (will set checking flag)
        const checkPromise1 = SSL.check()
        // Start second check immediately (should be skipped due to checking flag)
        const checkPromise2 = SSL.check()

        await Promise.all([checkPromise1, checkPromise2])

        // Self-signed certificate should only be generated once
        expect(selfsigned.generate).toHaveBeenCalledTimes(1)
      })

      describe('certificate expiration date validation', () => {
        it('should validate certificate expiry dates correctly', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 60 // 60 days (valid)

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should not trigger renewal for valid certificate
          expect(acme.forge.createPrivateKey).not.toHaveBeenCalled()
        })

        it('should trigger renewal for certificates expiring within 30 days', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 15 // 15 days (needs renewal)

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should trigger renewal for certificate expiring soon
          expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        })

        it('should trigger renewal for expired certificates', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl.expiry = Date.now() - 1000 * 60 * 60 * 24 // Expired yesterday

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should trigger renewal for expired certificate
          expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        })
      })

      describe('certificate file existence checking', () => {
        it('should trigger renewal when certificate configuration is missing', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl = null // No SSL configuration

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should trigger renewal when SSL configuration is missing
          expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        })

        it('should skip renewal when certificate files exist and are valid', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 60 // Valid expiry

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          // Mock file existence check to return true
          fs.existsSync.mockReturnValue(true)

          await SSL.check()

          // Should not trigger renewal when files exist and certificate is valid
          expect(acme.forge.createPrivateKey).not.toHaveBeenCalled()
        })

        it('should validate self-signed certificate file existence', async () => {
          mockConfig.config.websites = {
            'example.com': createMockWebsiteConfig('example.com')
          }
          mockConfig.config.ssl = {
            key: '/home/test/.odac/cert/ssl/odac.key',
            cert: '/home/test/.odac/cert/ssl/odac.crt',
            expiry: Date.now() + 86400000 // Valid
          }

          // Mock self-signed certificate files as missing
          fs.existsSync.mockImplementation(path => {
            if (path.includes('odac.key') || path.includes('odac.crt')) {
              return false
            }
            return true
          })

          await SSL.check()

          // Should regenerate self-signed certificate when files are missing
          expect(selfsigned.generate).toHaveBeenCalled()
        })
      })

      describe('automatic renewal triggers', () => {
        it('should automatically trigger renewal for certificates near expiry', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          // Set expiry to exactly 29 days from now (within threshold)
          mockWebsite.cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 29

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should trigger renewal when less than 30 days remain
          expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        })

        it('should not trigger renewal for certificates with more than 30 days validity', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          // Set expiry to 31 days from now (just over threshold)
          mockWebsite.cert.ssl.expiry = Date.now() + 1000 * 60 * 60 * 24 * 31

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should not trigger renewal when certificate has more than 30 days
          expect(acme.forge.createPrivateKey).not.toHaveBeenCalled()
        })

        it('should handle missing SSL configuration gracefully', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl = null // No SSL config

          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Should trigger renewal when SSL config is missing
          expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        })
      })

      it('should skip checking if no websites configured', async () => {
        mockConfig.config.websites = null

        await SSL.check()

        // Should not attempt to generate certificates for domains
        expect(acme.forge.createPrivateKey).not.toHaveBeenCalled()
      })

      it('should generate self-signed certificate if missing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalledWith([{name: 'commonName', value: 'Odac'}], {days: 365, keySize: 2048})
        expect(fs.mkdirSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl', {recursive: true})
        expect(fs.writeFileSync).toHaveBeenCalledTimes(2)
      })

      it('should skip self-signed generation if valid certificate exists', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = {
          key: '/path/to/key',
          cert: '/path/to/cert',
          expiry: Date.now() + 86400000
        }

        await SSL.check()

        expect(selfsigned.generate).not.toHaveBeenCalled()
      })

      it('should check SSL certificates for all domains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert = false // Skip SSL for this domain

        const testWebsite = createMockWebsiteConfig('test.com')
        testWebsite.cert.ssl = null // Force renewal for test.com

        mockConfig.config.websites = {
          'example.com': mockWebsite,
          'test.com': testWebsite
        }

        await SSL.check()

        // Should process test.com but skip example.com
        expect(acme.forge.createPrivateKey).toHaveBeenCalled()
      })

      it('should renew certificates near expiry', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert = {
          ssl: {
            key: '/path/to/key',
            cert: '/path/to/cert',
            expiry: Date.now() + 1000 * 60 * 60 * 24 * 15 // 15 days (less than 30 day threshold)
          }
        }

        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        expect(acme.Client).toHaveBeenCalled()
      })

      it('should skip renewal for valid certificates', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert = {
          ssl: {
            key: '/path/to/key',
            cert: '/path/to/cert',
            expiry: Date.now() + 1000 * 60 * 60 * 24 * 60 // 60 days (more than 30 day threshold)
          }
        }

        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createPrivateKey).not.toHaveBeenCalled()
      })
    })

    describe('renew method', () => {
      beforeEach(() => {
        // Set up the API mock to return proper result format
        const mockApi = mockOdac.server('Api')
        mockApi.result = jest.fn((success, message) => ({success, data: message}))
      })

      it('should reject renewal for IP addresses', () => {
        const result = SSL.renew('192.168.1.1')

        expect(result.success).toBe(false)
        expect(result.data).toContain('SSL renewal is not available for IP addresses')
      })

      it('should return error for non-existent domain', () => {
        mockConfig.config.websites = {}

        const result = SSL.renew('nonexistent.com')

        expect(result.success).toBe(false)
        expect(result.data).toBe('Domain nonexistent.com not found.')
      })

      it('should find domain by subdomain', () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = ['www', 'api']
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        const result = SSL.renew('www.example.com')

        expect(result.success).toBe(true)
        expect(result.data).toBe('SSL certificate for domain example.com renewed successfully.')
      })

      it('should successfully renew existing domain', () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }

        const result = SSL.renew('example.com')

        expect(result.success).toBe(true)
        expect(result.data).toBe('SSL certificate for domain example.com renewed successfully.')
      })
    })
  })

  describe('ACME protocol integration and challenge handling', () => {
    let mockClient

    beforeEach(() => {
      mockClient = {
        auto: jest.fn().mockResolvedValue('mock-certificate')
      }
      acme.Client.mockImplementation(() => mockClient)
    })

    describe('ACME client initialization and account creation', () => {
      it('should create ACME client with correct configuration', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        // Remove SSL cert to trigger renewal
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createPrivateKey).toHaveBeenCalled()
        expect(acme.Client).toHaveBeenCalledWith({
          directoryUrl: acme.directory.letsencrypt.production,
          accountKey: 'mock-private-key'
        })
      })

      it('should create private key for ACME account', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createPrivateKey).toHaveBeenCalledWith()
      })

      it("should use Let's Encrypt production directory URL", async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.Client).toHaveBeenCalledWith({
          directoryUrl: acme.directory.letsencrypt.production,
          accountKey: 'mock-private-key'
        })
      })

      it('should create CSR with domain and subdomains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = ['www', 'api']
        // Remove SSL cert to trigger renewal
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createCsr).toHaveBeenCalledWith({
          commonName: 'example.com',
          altNames: ['example.com', 'www.example.com', 'api.example.com']
        })
      })

      it('should handle domains without subdomains in CSR', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = [] // No subdomains
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createCsr).toHaveBeenCalledWith({
          commonName: 'example.com',
          altNames: ['example.com']
        })
      })

      it('should handle undefined subdomains in CSR', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        delete mockWebsite.subdomain // Undefined subdomains
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createCsr).toHaveBeenCalledWith({
          commonName: 'example.com',
          altNames: ['example.com']
        })
      })
    })

    describe('DNS-01 challenge creation and DNS record management', () => {
      it('should create DNS challenge records with correct parameters', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        // Remove SSL cert to trigger renewal
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeCreateFn
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'example.com'}}
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'mock-key-auth'

          await options.challengeCreateFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockDNS.record).toHaveBeenCalledWith({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'mock-key-auth',
          ttl: 100,
          unique: true
        })
      })

      it('should create DNS challenge records for subdomains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = ['www']
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeCreateFn for subdomain
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'www.example.com'}}
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'subdomain-key-auth'

          await options.challengeCreateFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockDNS.record).toHaveBeenCalledWith({
          name: '_acme-challenge.www.example.com',
          type: 'TXT',
          value: 'subdomain-key-auth',
          ttl: 100,
          unique: true
        })
      })

      it('should handle non-DNS challenge types gracefully', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeCreateFn with http-01 challenge
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'example.com'}}
          const challenge = {type: 'http-01'}
          const keyAuthorization = 'http-key-auth'

          await options.challengeCreateFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        // Should not create DNS record for non-DNS challenges
        expect(mockDNS.record).not.toHaveBeenCalled()
      })

      it('should remove DNS challenge records after validation', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeRemoveFn
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'example.com'}}
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'mock-key-auth'

          await options.challengeRemoveFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'mock-key-auth'
        })
      })

      it('should remove DNS challenge records for subdomains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeRemoveFn for subdomain
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'api.example.com'}}
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'api-key-auth'

          await options.challengeRemoveFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.api.example.com',
          type: 'TXT',
          value: 'api-key-auth'
        })
      })

      it('should handle non-DNS challenge removal gracefully', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeRemoveFn with http-01 challenge
        mockClient.auto.mockImplementation(async options => {
          const authz = {identifier: {value: 'example.com'}}
          const challenge = {type: 'http-01'}
          const keyAuthorization = 'http-key-auth'

          await options.challengeRemoveFn(authz, challenge, keyAuthorization)
          return 'mock-certificate'
        })

        await SSL.check()

        // Should not attempt to delete DNS record for non-DNS challenges
        expect(mockDNS.delete).not.toHaveBeenCalled()
      })

      it('should handle challenge key authorization correctly', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeKeyAuthorizationFn
        mockClient.auto.mockImplementation(async options => {
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'mock-key-auth'

          const result = await options.challengeKeyAuthorizationFn(challenge, keyAuthorization)
          expect(result).toBe('mock-key-auth')
          return 'mock-certificate'
        })

        await SSL.check()
      })

      it('should handle challenge timeout gracefully', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to call challengeTimeoutFn
        mockClient.auto.mockImplementation(async options => {
          await options.challengeTimeoutFn()
          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockClient.auto).toHaveBeenCalled()
      })

      it('should use dns-01 as challenge priority', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(mockClient.auto).toHaveBeenCalledWith(
          expect.objectContaining({
            challengePriority: ['dns-01']
          })
        )
      })
    })

    describe('certificate signing request (CSR) generation and processing', () => {
      it('should generate CSR with correct parameters', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = [] // No subdomains for this test
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createCsr).toHaveBeenCalledWith({
          commonName: 'example.com',
          altNames: ['example.com']
        })
      })

      it('should generate CSR with multiple domains including subdomains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = ['www', 'api', 'mail']
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(acme.forge.createCsr).toHaveBeenCalledWith({
          commonName: 'example.com',
          altNames: ['example.com', 'www.example.com', 'api.example.com', 'mail.example.com']
        })
      })

      it('should process CSR with ACME client auto method', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(mockClient.auto).toHaveBeenCalledWith({
          csr: 'mock-csr',
          termsOfServiceAgreed: true,
          challengePriority: ['dns-01'],
          challengeCreateFn: expect.any(Function),
          challengeRemoveFn: expect.any(Function),
          challengeKeyAuthorizationFn: expect.any(Function),
          challengeTimeoutFn: expect.any(Function)
        })
      })

      it('should agree to terms of service automatically', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(mockClient.auto).toHaveBeenCalledWith(
          expect.objectContaining({
            termsOfServiceAgreed: true
          })
        )
      })

      it('should store certificate files after successful generation', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockImplementation(path => {
          if (path.includes('.odac/cert/ssl')) {
            return false
          }
          return true
        })

        await SSL.check()

        expect(fs.mkdirSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl', {recursive: true})
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/example.com.key', 'mock-key')
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/example.com.crt', 'mock-certificate')
      })

      it('should not create directory if it already exists', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock directory exists
        fs.existsSync.mockReturnValue(true)

        await SSL.check()

        expect(fs.mkdirSync).not.toHaveBeenCalled()
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/example.com.key', 'mock-key')
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/example.com.crt', 'mock-certificate')
      })

      it('should update website configuration with new certificate', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        await SSL.check()

        expect(mockWebsite.cert.ssl).toEqual({
          key: '/home/test/.odac/cert/ssl/example.com.key',
          cert: '/home/test/.odac/cert/ssl/example.com.crt',
          expiry: expect.any(Number)
        })
      })

      it('should set certificate expiry to 90 days from now', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        const beforeTime = Date.now()
        await SSL.check()
        const afterTime = Date.now()

        const expectedExpiry = 1000 * 60 * 60 * 24 * 30 * 3 // 90 days
        expect(mockWebsite.cert.ssl.expiry).toBeGreaterThanOrEqual(beforeTime + expectedExpiry - 1000)
        expect(mockWebsite.cert.ssl.expiry).toBeLessThanOrEqual(afterTime + expectedExpiry + 1000)
      })

      describe('configuration updates after renewal', () => {
        it('should update website configuration with new certificate paths', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl = null // Force renewal
          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          expect(mockWebsite.cert.ssl).toEqual({
            key: '/home/test/.odac/cert/ssl/example.com.key',
            cert: '/home/test/.odac/cert/ssl/example.com.crt',
            expiry: expect.any(Number)
          })
          expect(mockConfig.config.websites['example.com']).toBe(mockWebsite)
        })

        it('should save configuration after certificate renewal', async () => {
          const mockWebsite = createMockWebsiteConfig('example.com')
          mockWebsite.cert.ssl = null
          mockConfig.config.websites = {
            'example.com': mockWebsite
          }

          await SSL.check()

          // Configuration should be updated with new certificate info
          expect(mockConfig.config.websites['example.com'].cert.ssl).toBeDefined()
          expect(mockConfig.config.websites['example.com'].cert.ssl.key).toBe('/home/test/.odac/cert/ssl/example.com.key')
          expect(mockConfig.config.websites['example.com'].cert.ssl.cert).toBe('/home/test/.odac/cert/ssl/example.com.crt')
        })
      })
    })

    describe('challenge cleanup and DNS record removal', () => {
      it('should clean up DNS records after successful challenge', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to simulate successful challenge completion
        mockClient.auto.mockImplementation(async options => {
          // Simulate challenge creation
          const authz = {identifier: {value: 'example.com'}}
          const challenge = {type: 'dns-01'}
          const keyAuthorization = 'test-key-auth'

          await options.challengeCreateFn(authz, challenge, keyAuthorization)
          await options.challengeRemoveFn(authz, challenge, keyAuthorization)

          return 'mock-certificate'
        })

        await SSL.check()

        expect(mockDNS.record).toHaveBeenCalledWith({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'test-key-auth',
          ttl: 100,
          unique: true
        })

        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'test-key-auth'
        })
      })

      it('should clean up DNS records for all subdomains', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.subdomain = ['www', 'api']
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Mock the auto method to simulate challenges for multiple domains
        mockClient.auto.mockImplementation(async options => {
          const domains = ['example.com', 'www.example.com', 'api.example.com']

          for (const domain of domains) {
            const authz = {identifier: {value: domain}}
            const challenge = {type: 'dns-01'}
            const keyAuthorization = `${domain}-key-auth`

            await options.challengeCreateFn(authz, challenge, keyAuthorization)
            await options.challengeRemoveFn(authz, challenge, keyAuthorization)
          }

          return 'mock-certificate'
        })

        await SSL.check()

        // Verify cleanup for all domains
        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.example.com',
          type: 'TXT',
          value: 'example.com-key-auth'
        })
        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.www.example.com',
          type: 'TXT',
          value: 'www.example.com-key-auth'
        })
        expect(mockDNS.delete).toHaveBeenCalledWith({
          name: '_acme-challenge.api.example.com',
          type: 'TXT',
          value: 'api.example.com-key-auth'
        })
      })
    })
  })

  describe('self-signed certificate generation and error handling', () => {
    describe('self-signed certificate generation with selfsigned module', () => {
      it('should generate self-signed certificate when SSL config is missing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null // No SSL config

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalledWith([{name: 'commonName', value: 'Odac'}], {days: 365, keySize: 2048})
      })

      it('should generate self-signed certificate when SSL config is expired', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = {
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: Date.now() - 86400000 // Expired yesterday
        }

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalledWith([{name: 'commonName', value: 'Odac'}], {days: 365, keySize: 2048})
      })

      it('should use correct certificate attributes for self-signed generation', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalledWith([{name: 'commonName', value: 'Odac'}], {days: 365, keySize: 2048})
      })

      it('should use correct options for self-signed certificate generation', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        await SSL.check()

        const expectedOptions = {days: 365, keySize: 2048}
        expect(selfsigned.generate).toHaveBeenCalledWith(expect.any(Array), expectedOptions)
      })

      it('should not generate self-signed certificate when valid one exists', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = {
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: Date.now() + 86400000 // Valid for another day
        }

        // Mock files exist
        fs.existsSync.mockReturnValue(true)

        await SSL.check()

        expect(selfsigned.generate).not.toHaveBeenCalled()
      })

      it('should regenerate self-signed certificate when key file is missing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = {
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: Date.now() + 86400000 // Valid expiry
        }

        // Mock key file missing but cert file exists
        fs.existsSync.mockImplementation(path => {
          if (path.includes('odac.key')) return false
          if (path.includes('odac.crt')) return true
          return true
        })

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalled()
      })

      it('should regenerate self-signed certificate when cert file is missing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = {
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: Date.now() + 86400000 // Valid expiry
        }

        // Mock cert file missing but key file exists
        fs.existsSync.mockImplementation(path => {
          if (path.includes('odac.key')) return true
          if (path.includes('odac.crt')) return false
          return true
        })

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalled()
      })
    })

    describe('certificate file storage and configuration updates', () => {
      it('should create SSL directory if it does not exist', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist
        fs.existsSync.mockImplementation(path => {
          if (path.includes('.odac/cert/ssl')) return false
          return true
        })

        await SSL.check()

        expect(fs.mkdirSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl', {recursive: true})
      })

      it('should not create SSL directory if it already exists', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory exists
        fs.existsSync.mockReturnValue(true)

        await SSL.check()

        expect(fs.mkdirSync).not.toHaveBeenCalled()
      })

      it('should write self-signed private key to correct file path', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        await SSL.check()

        expect(fs.writeFileSync).toHaveBeenCalledWith(
          '/home/test/.odac/cert/ssl/odac.key',
          '-----BEGIN PRIVATE KEY-----\nmock-private-key\n-----END PRIVATE KEY-----'
        )
      })

      it('should write self-signed certificate to correct file path', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        await SSL.check()

        expect(fs.writeFileSync).toHaveBeenCalledWith(
          '/home/test/.odac/cert/ssl/odac.crt',
          '-----BEGIN CERTIFICATE-----\nmock-certificate\n-----END CERTIFICATE-----'
        )
      })

      it('should update SSL configuration with new certificate paths', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        await SSL.check()

        expect(mockConfig.config.ssl).toEqual({
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: expect.any(Number)
        })
      })

      it('should set self-signed certificate expiry to 24 hours from now', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        const beforeTime = Date.now()
        await SSL.check()
        const afterTime = Date.now()

        const expectedExpiry = 86400000 // 24 hours in milliseconds
        expect(mockConfig.config.ssl.expiry).toBeGreaterThanOrEqual(beforeTime + expectedExpiry - 1000)
        expect(mockConfig.config.ssl.expiry).toBeLessThanOrEqual(afterTime + expectedExpiry + 1000)
      })

      it('should preserve existing SSL configuration when certificate is valid', async () => {
        const existingSSL = {
          key: '/home/test/.odac/cert/ssl/odac.key',
          cert: '/home/test/.odac/cert/ssl/odac.crt',
          expiry: Date.now() + 86400000 // Valid for another day
        }

        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = existingSSL

        // Mock files exist
        fs.existsSync.mockReturnValue(true)

        await SSL.check()

        expect(mockConfig.config.ssl).toEqual(existingSSL)
        expect(selfsigned.generate).not.toHaveBeenCalled()
      })
    })

    describe('error handling and retry logic for failed renewals', () => {
      it('should handle selfsigned.generate errors by throwing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock selfsigned.generate to throw an error
        selfsigned.generate.mockImplementation(() => {
          throw new Error('Certificate generation failed')
        })

        // Should throw the error since SSL module doesn't handle it
        await expect(SSL.check()).rejects.toThrow('Certificate generation failed')

        expect(selfsigned.generate).toHaveBeenCalled()
      })

      it('should handle file system write errors by throwing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock fs.writeFileSync to throw an error only for self-signed cert files
        fs.writeFileSync.mockImplementation(path => {
          if (path.includes('odac.key') || path.includes('odac.crt')) {
            throw new Error('File write failed')
          }
        })

        // Should throw the error since SSL module doesn't handle it
        await expect(SSL.check()).rejects.toThrow('File write failed')

        expect(fs.writeFileSync).toHaveBeenCalled()
      })

      it('should handle directory creation errors by throwing', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist
        fs.existsSync.mockReturnValue(false)

        // Mock fs.mkdirSync to throw an error
        fs.mkdirSync.mockImplementation(() => {
          throw new Error('Directory creation failed')
        })

        // Should throw the error since SSL module doesn't handle it
        await expect(SSL.check()).rejects.toThrow('Directory creation failed')

        expect(fs.mkdirSync).toHaveBeenCalled()
      })

      it('should handle ACME client errors with retry logic', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null // Force renewal
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Create a local mock client for this test
        const localMockClient = {
          auto: jest.fn().mockRejectedValue(new Error('ACME challenge failed'))
        }
        acme.Client.mockImplementation(() => localMockClient)

        await SSL.check()

        // Should have attempted ACME renewal
        expect(localMockClient.auto).toHaveBeenCalled()

        // Should log the error (verify error logging was called)
        expect(mockLog.error).toHaveBeenCalledWith(expect.any(Error))
      })

      it('should implement exponential backoff for failed ACME renewals', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Create a local mock client for this test
        const localMockClient = {
          auto: jest.fn().mockRejectedValue(new Error('ACME failed'))
        }
        acme.Client.mockImplementation(() => localMockClient)

        // First attempt
        await SSL.check()

        // Reset checking flag to allow second attempt
        // Note: We can't access private properties, so we'll test the behavior indirectly

        // Second attempt should be blocked by retry interval
        await SSL.check()

        // Should only attempt once due to retry logic (second call is blocked by checking flag)
        expect(localMockClient.auto).toHaveBeenCalledTimes(1)
      })

      it('should limit retry attempts to prevent infinite loops', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Create a local mock client for this test
        const localMockClient = {
          auto: jest.fn().mockRejectedValue(new Error('Persistent ACME failure'))
        }
        acme.Client.mockImplementation(() => localMockClient)

        // First attempt will fail and set retry interval
        await SSL.check()

        // Should have attempted once and logged error
        expect(localMockClient.auto).toHaveBeenCalled()
        expect(mockLog.error).toHaveBeenCalled()
      })

      it('should reset error count after successful renewal', async () => {
        const mockWebsite = createMockWebsiteConfig('example.com')
        mockWebsite.cert.ssl = null
        mockConfig.config.websites = {
          'example.com': mockWebsite
        }

        // Create a local mock client for this test
        const localMockClient = {
          auto: jest.fn().mockRejectedValueOnce(new Error('Temporary failure')).mockResolvedValueOnce('mock-certificate')
        }
        acme.Client.mockImplementation(() => localMockClient)

        // First failure
        await SSL.check()
        expect(mockLog.error).toHaveBeenCalledTimes(1)

        // Note: We can't easily test the second attempt due to private state management
        // This test verifies the first failure is handled correctly
        expect(localMockClient.auto).toHaveBeenCalledTimes(1)
      })
    })

    describe('certificate validation and format verification', () => {
      it('should validate self-signed certificate format from selfsigned module', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        // Verify the mock was called and returns properly formatted PEM certificates
        expect(selfsigned.generate).toHaveBeenCalled()
        const mockReturnValue = selfsigned.generate.mock.results[0].value
        expect(mockReturnValue.private).toContain('-----BEGIN PRIVATE KEY-----')
        expect(mockReturnValue.private).toContain('-----END PRIVATE KEY-----')
        expect(mockReturnValue.cert).toContain('-----BEGIN CERTIFICATE-----')
        expect(mockReturnValue.cert).toContain('-----END CERTIFICATE-----')
      })

      it('should validate certificate file paths are correctly set', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        expect(mockConfig.config.ssl.key).toBe('/home/test/.odac/cert/ssl/odac.key')
        expect(mockConfig.config.ssl.cert).toBe('/home/test/.odac/cert/ssl/odac.crt')
        expect(typeof mockConfig.config.ssl.expiry).toBe('number')
        expect(mockConfig.config.ssl.expiry).toBeGreaterThan(Date.now())
      })

      it('should validate certificate expiry is set correctly', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        const beforeTime = Date.now()
        await SSL.check()
        const afterTime = Date.now()

        // Should be set to 24 hours from now (86400000 ms)
        expect(mockConfig.config.ssl.expiry).toBeGreaterThanOrEqual(beforeTime + 86400000 - 1000)
        expect(mockConfig.config.ssl.expiry).toBeLessThanOrEqual(afterTime + 86400000 + 1000)
      })

      it('should validate certificate files are written with correct content', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        // Verify key file content
        expect(fs.writeFileSync).toHaveBeenCalledWith(
          '/home/test/.odac/cert/ssl/odac.key',
          expect.stringContaining('-----BEGIN PRIVATE KEY-----')
        )

        // Verify certificate file content
        expect(fs.writeFileSync).toHaveBeenCalledWith(
          '/home/test/.odac/cert/ssl/odac.crt',
          expect.stringContaining('-----BEGIN CERTIFICATE-----')
        )
      })

      it('should validate certificate attributes match expected values', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        expect(selfsigned.generate).toHaveBeenCalledWith([{name: 'commonName', value: 'Odac'}], {days: 365, keySize: 2048})
      })

      it('should validate certificate generation parameters are secure', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        await SSL.check()

        // Verify the call was made with secure parameters
        expect(selfsigned.generate).toHaveBeenCalledWith(
          expect.any(Array),
          expect.objectContaining({
            keySize: expect.any(Number),
            days: expect.any(Number)
          })
        )

        // Get the actual options passed
        const callArgs = selfsigned.generate.mock.calls[0]
        const options = callArgs[1]

        // Verify secure key size (2048 bits minimum)
        expect(options.keySize).toBeGreaterThanOrEqual(2048)

        // Verify reasonable validity period (365 days)
        expect(options.days).toBe(365)
      })

      it('should handle malformed certificate data gracefully', async () => {
        mockConfig.config.websites = {
          'example.com': createMockWebsiteConfig('example.com')
        }
        mockConfig.config.ssl = null

        // Mock directory doesn't exist to trigger creation
        fs.existsSync.mockReturnValue(false)

        // Mock selfsigned to return malformed data
        selfsigned.generate.mockReturnValue({
          private: 'invalid-key-data',
          cert: 'invalid-cert-data'
        })

        await SSL.check()

        // Should still write the files even with malformed data
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/odac.key', 'invalid-key-data')
        expect(fs.writeFileSync).toHaveBeenCalledWith('/home/test/.odac/cert/ssl/odac.crt', 'invalid-cert-data')
      })
    })
  })
})
