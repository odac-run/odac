/**
 * Unit tests for Mail.js server initialization and database setup
 * Tests SMTP/IMAP server creation and database initialization
 */

const {setupGlobalMocks, cleanupGlobalMocks, createMockEventEmitter} = require('./__mocks__/testHelpers')
const {createMockWebsiteConfig} = require('./__mocks__/testFactories')

// Mock external dependencies
jest.mock('smtp-server')
jest.mock('sqlite3')
jest.mock('fs')
jest.mock('os')
jest.mock('../../server/src/mail/server', () => require('./__mocks__/server/src/mail/server'))
jest.mock('tls')

const {SMTPServer} = require('smtp-server')
const sqlite3 = require('sqlite3')
const fs = require('fs')
const os = require('os')
const mailServer = require('../../server/src/mail/server')
const tls = require('tls')

describe('Mail Module - Server Initialization and Database Setup', () => {
  let Mail
  let mockDb
  let mockConfig

  beforeEach(() => {
    setupGlobalMocks()

    // Setup mock database
    mockDb = {
      serialize: jest.fn(callback => callback && callback()),
      run: jest.fn((sql, params, callback) => {
        if (typeof params === 'function') {
          callback = params
        }
        if (callback) callback(null)
      })
    }

    // Setup sqlite3 mock
    sqlite3.verbose.mockReturnValue({
      Database: jest.fn().mockImplementation((path, callback) => {
        if (callback) callback(null)
        return mockDb
      })
    })

    // Setup mock config with MX records
    mockConfig = {
      config: {
        websites: {
          'example.com': createMockWebsiteConfig('example.com', {
            DNS: {
              MX: [{name: 'example.com', value: 'mail.example.com', priority: 10}]
            }
          })
        }
      }
    }

    // Setup global Odac mocks
    global.Odac.setMock('core', 'Config', mockConfig)
    global.Odac.setMock('server', 'Log', {
      init: jest.fn().mockReturnValue({
        log: jest.fn(),
        error: jest.fn()
      })
    })

    // Setup os mock
    os.homedir.mockReturnValue('/home/user')

    // Setup fs mock
    fs.existsSync.mockReturnValue(true)
    fs.mkdirSync.mockImplementation(() => {})
    fs.readFileSync.mockReturnValue('mock-cert-content')

    // Setup SMTP server mock
    const mockSMTPServer = createMockEventEmitter()
    mockSMTPServer.listen = jest.fn()
    SMTPServer.mockImplementation(() => mockSMTPServer)

    // Setup mail server mock
    const mockMailServer = createMockEventEmitter()
    mockMailServer.listen = jest.fn()
    mailServer.mockImplementation(() => mockMailServer)

    // Setup TLS mock
    tls.createSecureContext.mockReturnValue({})

    // Clear module cache and require fresh instance
    jest.resetModules()
    Mail = require('../../server/src/Mail')
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.clearAllMocks()
  })

  describe('SMTP Server Initialization', () => {
    test('should initialize SMTP server on port 25', () => {
      // Act
      Mail.init()

      // Assert
      expect(SMTPServer).toHaveBeenCalledWith(
        expect.objectContaining({
          logger: true,
          secure: false,
          banner: 'Odac',
          size: 1024 * 1024 * 10,
          authOptional: true
        })
      )

      const smtpInstance = SMTPServer.mock.results[0].value
      expect(smtpInstance.listen).toHaveBeenCalledWith(25)
    })

    test('should initialize secure SMTP server on port 465', () => {
      // Act
      Mail.init()

      // Assert
      expect(SMTPServer).toHaveBeenCalledTimes(2)

      const secureSmtpInstance = SMTPServer.mock.results[1].value
      expect(secureSmtpInstance.listen).toHaveBeenCalledWith(465)
    })

    test('should configure secure SMTP server with SSL options', () => {
      // Act
      Mail.init()

      // Assert
      const secureSmtpOptions = SMTPServer.mock.calls[1][0]
      expect(secureSmtpOptions.secure).toBe(true)
      expect(secureSmtpOptions).toHaveProperty('SNICallback')
    })
  })

  describe('IMAP Server Initialization', () => {
    test('should initialize IMAP server on port 143', () => {
      // Act
      Mail.init()

      // Assert
      expect(mailServer).toHaveBeenCalledWith(
        expect.objectContaining({
          logger: true,
          secure: false,
          banner: 'Odac'
        })
      )

      const imapInstance = mailServer.mock.results[0].value
      expect(imapInstance.listen).toHaveBeenCalledWith(143)
    })

    test('should initialize secure IMAP server on port 993', () => {
      // Act
      Mail.init()

      // Assert
      expect(mailServer).toHaveBeenCalledTimes(2)

      const secureImapInstance = mailServer.mock.results[1].value
      expect(secureImapInstance.listen).toHaveBeenCalledWith(993)
    })
  })

  describe('Database Setup', () => {
    test('should create SQLite database with correct path', () => {
      // Act
      Mail.init()

      // Assert
      expect(sqlite3.verbose().Database).toHaveBeenCalledWith('/home/user/.odac/db/mail', expect.any(Function))
    })

    test('should create mail database tables on initialization', () => {
      // Act
      Mail.init()

      // Assert
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE TABLE IF NOT EXISTS mail_received'))
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE TABLE IF NOT EXISTS mail_account'))
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE TABLE IF NOT EXISTS mail_box'))
    })

    test('should create database indexes on initialization', () => {
      // Act
      Mail.init()

      // Assert
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE INDEX IF NOT EXISTS idx_email ON mail_account'))
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE INDEX IF NOT EXISTS idx_domain ON mail_account'))
      expect(mockDb.run).toHaveBeenCalledWith(expect.stringContaining('CREATE INDEX IF NOT EXISTS idx_uid ON mail_received'))
    })

    test('should create database directory if it does not exist', () => {
      // Arrange
      fs.existsSync.mockReturnValue(false)

      // Act
      Mail.init()

      // Assert
      expect(fs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac/db', {recursive: true})
    })

    test('should handle database connection errors', () => {
      // Arrange
      const dbError = new Error('Database connection failed')
      const mockError = jest.fn()

      sqlite3.verbose.mockReturnValue({
        Database: jest.fn().mockImplementation((path, callback) => {
          callback(dbError)
          return mockDb
        })
      })

      global.Odac.setMock('server', 'Log', {
        init: jest.fn().mockReturnValue({
          log: jest.fn(),
          error: mockError
        })
      })

      // Clear module cache and require fresh instance
      jest.resetModules()
      const FreshMail = require('../../server/src/Mail')

      // Act
      FreshMail.init()

      // Assert
      expect(mockError).toHaveBeenCalledWith(dbError.message)
    })
  })

  describe('SSL/TLS Configuration', () => {
    test('should setup SNI callback for SSL certificate selection', () => {
      // Arrange
      mockConfig.config.ssl = {
        key: '/etc/ssl/private/default.key',
        cert: '/etc/ssl/certs/default.crt'
      }

      // Act
      Mail.init()

      // Assert
      const smtpOptions = SMTPServer.mock.calls[1][0] // Second call is for secure SMTP
      expect(smtpOptions).toHaveProperty('SNICallback')
      expect(tls.createSecureContext).toHaveBeenCalled()
    })

    test('should use website-specific SSL certificates when available', () => {
      // Arrange
      const mockWebsite = createMockWebsiteConfig('test.com')
      mockConfig.config.websites['test.com'] = mockWebsite
      mockConfig.config.ssl = {
        key: '/etc/ssl/private/default.key',
        cert: '/etc/ssl/certs/default.crt'
      }

      fs.existsSync.mockImplementation(path => {
        return path.includes('test.com') // Only test.com certs exist
      })

      // Act
      Mail.init()

      // Get the SNI callback
      const smtpOptions = SMTPServer.mock.calls[1][0]
      const sniCallback = smtpOptions.SNICallback
      const mockCallback = jest.fn()

      // Test SNI callback with existing cert
      sniCallback('test.com', mockCallback)

      // Assert
      expect(fs.existsSync).toHaveBeenCalledWith(mockWebsite.cert.ssl.key)
      expect(fs.existsSync).toHaveBeenCalledWith(mockWebsite.cert.ssl.cert)
      expect(fs.readFileSync).toHaveBeenCalledWith(mockWebsite.cert.ssl.key)
      expect(fs.readFileSync).toHaveBeenCalledWith(mockWebsite.cert.ssl.cert)
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-cert-content',
        cert: 'mock-cert-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })

    test('should fallback to default SSL certificates when website certs not found', () => {
      // Arrange
      mockConfig.config.ssl = {
        key: '/etc/ssl/private/default.key',
        cert: '/etc/ssl/certs/default.crt'
      }

      fs.existsSync.mockImplementation(path => {
        return !path.includes('test.com') // Website certs don't exist
      })

      // Act
      Mail.init()

      // Get the SNI callback
      const smtpOptions = SMTPServer.mock.calls[1][0]
      const sniCallback = smtpOptions.SNICallback
      const mockCallback = jest.fn()

      // Test SNI callback with missing website cert
      sniCallback('test.com', mockCallback)

      // Assert
      expect(fs.readFileSync).toHaveBeenCalledWith('/etc/ssl/private/default.key')
      expect(fs.readFileSync).toHaveBeenCalledWith('/etc/ssl/certs/default.crt')
      expect(tls.createSecureContext).toHaveBeenCalledWith({
        key: 'mock-cert-content',
        cert: 'mock-cert-content'
      })
      expect(mockCallback).toHaveBeenCalledWith(null, expect.any(Object))
    })
  })

  describe('Initialization Conditions', () => {
    test('should not initialize if no domains have MX records', () => {
      // Arrange
      mockConfig.config.websites = {
        'example.com': createMockWebsiteConfig('example.com', {
          DNS: {
            A: [{name: 'example.com', value: '127.0.0.1'}]
            // No MX records
          }
        })
      }

      // Act
      Mail.init()

      // Assert
      expect(SMTPServer).not.toHaveBeenCalled()
      expect(mailServer).not.toHaveBeenCalled()
    })

    test('should not initialize twice if already started', () => {
      // Act
      Mail.init()
      Mail.init() // Second call

      // Assert
      expect(SMTPServer).toHaveBeenCalledTimes(2) // Only from first init
      expect(mailServer).toHaveBeenCalledTimes(2) // Only from first init
    })
  })

  describe('Error Handling', () => {
    test('should handle SMTP server errors', () => {
      // Arrange
      const mockLog = jest.fn()
      global.Odac.setMock('server', 'Log', {
        init: jest.fn().mockReturnValue({
          log: mockLog,
          error: jest.fn()
        })
      })

      // Act
      Mail.init()

      // Simulate SMTP server error
      const smtpInstance = SMTPServer.mock.results[0].value
      const errorHandler = smtpInstance.on.mock.calls.find(call => call[0] === 'error')[1]
      const testError = new Error('SMTP Server Error')
      errorHandler(testError)

      // Assert
      expect(mockLog).toHaveBeenCalledWith('SMTP Server Error: ', testError)
    })

    test('should handle secure SMTP server errors', () => {
      // Arrange
      const mockError = jest.fn()
      global.Odac.setMock('server', 'Log', {
        init: jest.fn().mockReturnValue({
          log: jest.fn(),
          error: mockError
        })
      })

      // Act
      Mail.init()

      // Simulate secure SMTP server error
      const secureSmtpInstance = SMTPServer.mock.results[1].value
      const errorHandler = secureSmtpInstance.on.mock.calls.find(call => call[0] === 'error')[1]
      const testError = new Error('Secure SMTP Server Error')
      errorHandler(testError)

      // Assert
      expect(mockError).toHaveBeenCalledWith('SMTP Server Error: ', testError)
    })
  })
})
