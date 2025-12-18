/**
 * Comprehensive unit tests for the Mail.js module
 * Tests DKIM key generation, mail processing, and SMTP authentication
 */

const {setupGlobalMocks, cleanupGlobalMocks, createMockEventEmitter, waitFor} = require('./__mocks__/testHelpers')
const {createMockMailAccount, createMockEmailMessage, createMockWebsiteConfig} = require('./__mocks__/testFactories')

// Mock external dependencies
jest.mock('bcrypt')
jest.mock('smtp-server')
jest.mock('mailparser')
jest.mock('sqlite3')
jest.mock('node-forge')
jest.mock('fs')
jest.mock('os')
jest.mock('../../server/src/mail/server', () => require('./__mocks__/server/src/mail/server'))
jest.mock('../../server/src/mail/smtp', () => require('./__mocks__/server/src/mail/smtp'))
jest.mock('tls')

const bcrypt = require('bcrypt')
const {SMTPServer} = require('smtp-server')
const {simpleParser} = require('mailparser')
const sqlite3 = require('sqlite3')
const forge = require('node-forge')
const fs = require('fs')
const os = require('os')
const mailServer = require('../../server/src/mail/server')
const smtp = require('../../server/src/mail/smtp')
const tls = require('tls')

describe('Mail Module', () => {
  let Mail
  let mockDb
  let mockConfig
  let mockDNS
  let mockApi

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
      }),
      get: jest.fn((sql, params, callback) => {
        if (typeof params === 'function') {
          callback = params
          params = []
        }
        if (callback) callback(null, null)
      }),
      all: jest.fn((sql, params, callback) => {
        if (typeof params === 'function') {
          callback = params
          params = []
        }
        if (callback) callback(null, [])
      }),
      each: jest.fn((sql, params, rowCallback, completeCallback) => {
        if (typeof params === 'function') {
          completeCallback = rowCallback
          rowCallback = params
          params = []
        }
        if (completeCallback) completeCallback(null, 0)
      }),
      prepare: jest.fn(() => ({
        run: jest.fn(),
        finalize: jest.fn()
      }))
    }

    // Setup sqlite3 mock
    sqlite3.verbose.mockReturnValue({
      Database: jest.fn().mockImplementation((path, callback) => {
        if (callback) callback(null)
        return mockDb
      })
    })

    // Setup mock config
    mockConfig = {
      config: {
        websites: {
          'example.com': createMockWebsiteConfig('example.com', {
            DNS: {
              MX: [{name: 'example.com', value: 'mail.example.com', priority: 10}]
            },
            cert: false
          })
        }
      }
    }

    // Setup mock DNS service
    mockDNS = {
      record: jest.fn()
    }

    // Setup mock API service
    mockApi = {
      result: jest.fn((success, data) => ({success, data}))
    }

    // Setup global Odac mocks
    global.Odac.setMock('core', 'Config', mockConfig)
    global.Odac.setMock('server', 'DNS', mockDNS)
    global.Odac.setMock('server', 'Api', mockApi)
    global.Odac.setMock('server', 'Log', {
      init: jest.fn().mockReturnValue({
        log: jest.fn(),
        error: jest.fn()
      })
    })

    // Setup Odac.core mock
    jest.spyOn(global.Odac, 'core').mockImplementation(name => {
      if (name === 'Config') return mockConfig
      return {init: jest.fn()}
    })

    // Setup os mock
    os.homedir.mockReturnValue('/home/user')

    // Setup fs mock
    fs.existsSync.mockReturnValue(true)
    fs.mkdirSync.mockImplementation(() => {})
    fs.writeFileSync.mockImplementation(() => {})

    // Setup node-forge mock
    forge.pki = {
      rsa: {
        generateKeyPair: jest.fn().mockReturnValue({
          privateKey: 'mock-private-key',
          publicKey: 'mock-public-key'
        })
      },
      privateKeyToPem: jest.fn().mockReturnValue('-----BEGIN PRIVATE KEY-----\nmock-private-key\n-----END PRIVATE KEY-----'),
      publicKeyToPem: jest.fn().mockReturnValue('-----BEGIN PUBLIC KEY-----\nmock-public-key\n-----END PUBLIC KEY-----')
    }

    // Setup bcrypt mock
    bcrypt.hash.mockImplementation((password, rounds, callback) => {
      callback(null, '$2b$10$hashedpassword')
    })
    bcrypt.compare.mockImplementation((password, hash, callback) => {
      if (callback) {
        callback(null, password === 'correctpassword')
      } else {
        return Promise.resolve(password === 'correctpassword')
      }
    })

    // Setup SMTP server mock
    const mockSMTPServer = createMockEventEmitter()
    mockSMTPServer.listen = jest.fn()
    SMTPServer.mockImplementation(() => mockSMTPServer)

    // Setup mail server mock
    const mockMailServer = createMockEventEmitter()
    mockMailServer.listen = jest.fn()
    mailServer.mockImplementation(() => mockMailServer)

    // Setup mail parser mock
    simpleParser.mockImplementation((stream, options, callback) => {
      if (typeof options === 'function') {
        callback = options
      }
      const mockParsedMail = createMockEmailMessage()
      callback(null, mockParsedMail)
    })

    // Setup TLS mock
    tls.createSecureContext.mockReturnValue({})

    // Clear module cache and require fresh instance
    jest.resetModules()
    Mail = require('../../server/src/Mail')

    // Reset the Mail module's internal state by creating a new instance
    // Since Mail is a singleton, we need to reset its state
    if (Mail._resetForTesting) {
      Mail._resetForTesting()
    }
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.clearAllMocks()
  })

  describe('DKIM Key Generation', () => {
    beforeEach(() => {
      // Ensure domain has MX record but no DKIM cert
      mockConfig.config.websites['example.com'].cert = {}

      // Initialize Mail module first
      Mail.init()
    })

    test('should generate DKIM key pair for domain with MX records', () => {
      // Ensure domain has MX record but no DKIM cert
      mockConfig.config.websites['example.com'].cert = {}

      // Act
      Mail.check()

      // Assert
      expect(forge.pki.rsa.generateKeyPair).toHaveBeenCalledWith(1024)
      expect(forge.pki.privateKeyToPem).toHaveBeenCalledWith('mock-private-key')
      expect(forge.pki.publicKeyToPem).toHaveBeenCalledWith('mock-public-key')
    })

    test('should create DKIM directory if it does not exist', () => {
      // Arrange
      fs.existsSync.mockReturnValue(false)

      // Act
      Mail.check()

      // Assert
      expect(fs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac/cert/dkim', {recursive: true})
    })

    test('should write DKIM private and public keys to files', () => {
      // Act
      Mail.check()

      // Assert
      expect(fs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/cert/dkim/example.com.key',
        '-----BEGIN PRIVATE KEY-----\nmock-private-key\n-----END PRIVATE KEY-----'
      )
      expect(fs.writeFileSync).toHaveBeenCalledWith(
        '/home/user/.odac/cert/dkim/example.com.pub',
        '-----BEGIN PUBLIC KEY-----\nmock-public-key\n-----END PUBLIC KEY-----'
      )
    })

    test('should update website configuration with DKIM certificate paths', () => {
      // Act
      Mail.check()

      // Assert
      expect(mockConfig.config.websites['example.com'].cert).toEqual({
        dkim: {
          private: '/home/user/.odac/cert/dkim/example.com.key',
          public: '/home/user/.odac/cert/dkim/example.com.pub'
        }
      })
    })

    test('should create DNS TXT record for DKIM public key', () => {
      // Act
      Mail.check()

      // Assert
      expect(mockDNS.record).toHaveBeenCalledWith({
        type: 'TXT',
        name: 'default._domainkey.example.com',
        value: expect.stringContaining('v=DKIM1; k=rsa; p=')
      })
    })

    test('should not generate DKIM keys if already exists', () => {
      // Arrange
      mockConfig.config.websites['example.com'].cert = {
        dkim: {
          private: '/existing/path/key',
          public: '/existing/path/pub'
        }
      }

      // Act
      Mail.check()

      // Assert
      expect(forge.pki.rsa.generateKeyPair).not.toHaveBeenCalled()
    })

    test('should not generate DKIM keys for domains without MX records', () => {
      // Arrange
      delete mockConfig.config.websites['example.com'].DNS.MX

      // Act
      Mail.check()

      // Assert
      expect(forge.pki.rsa.generateKeyPair).not.toHaveBeenCalled()
    })

    test('should handle multiple domains with MX records', () => {
      // Arrange
      mockConfig.config.websites['test.com'] = createMockWebsiteConfig('test.com', {
        DNS: {
          MX: [{name: 'test.com', value: 'mail.test.com', priority: 10}]
        },
        cert: false
      })

      // Act
      Mail.check()

      // Assert
      expect(forge.pki.rsa.generateKeyPair).toHaveBeenCalledTimes(2)
      expect(fs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/cert/dkim/example.com.key', expect.any(String))
      expect(fs.writeFileSync).toHaveBeenCalledWith('/home/user/.odac/cert/dkim/test.com.key', expect.any(String))
    })
  })

  describe('Mail Parsing and Storage', () => {
    let mockParsedMail

    beforeEach(() => {
      mockParsedMail = createMockEmailMessage('sender@example.com', 'recipient@example.com')
      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Initialize Mail module
      Mail.init()
    })

    test('should parse incoming mail messages', async () => {
      // Arrange
      const mockStream = 'mock-email-stream'

      // Act
      await new Promise(resolve => {
        simpleParser(mockStream, {}, (err, parsed) => {
          resolve()
        })
      })

      // Assert
      expect(simpleParser).toHaveBeenCalledWith(mockStream, {}, expect.any(Function))
    })

    test('should store parsed mail in database with correct structure', async () => {
      // Arrange
      const mockEmail = 'test@example.com'
      const mockMailbox = 'INBOX'
      const mockFlags = '[]'

      // Mock the private store method by calling it through mail processing
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      // Act
      await mockSMTPOptions.onData('mock-stream', {user: mockEmail}, jest.fn())

      // Assert
      expect(mockDb.run).toHaveBeenCalledWith(
        expect.stringContaining('INSERT INTO mail_received'),
        expect.arrayContaining([
          expect.any(Number), // uid
          mockEmail,
          'INBOX',
          expect.any(String), // attachments JSON
          expect.any(String), // headers JSON
          expect.any(String), // headerLines JSON
          expect.any(String), // html
          expect.any(String), // text
          expect.any(String), // textAsHtml
          expect.any(String), // subject
          expect.any(String), // to JSON
          expect.any(String), // from JSON
          expect.any(String), // messageId
          expect.any(String) // flags
        ]),
        expect.any(Function)
      )
    })

    test('should handle mail parsing errors gracefully', async () => {
      // Arrange
      const parseError = new Error('Parse error')
      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(parseError)
      })

      const mockSMTPOptions = SMTPServer.mock.calls[0][0]
      const mockCallback = jest.fn()

      // Act
      await mockSMTPOptions.onData('mock-stream', {user: 'test@example.com'}, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(parseError)
    })

    test('should set seen flag for sent mail', async () => {
      // Arrange
      mockParsedMail.from.value[0].address = 'sender@example.com'
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      // Act
      await mockSMTPOptions.onData('mock-stream', {user: 'sender@example.com'}, jest.fn())

      // Assert
      expect(mockDb.run).toHaveBeenCalledWith(
        expect.stringContaining('INSERT INTO mail_received'),
        expect.arrayContaining([
          expect.any(Number),
          'sender@example.com',
          'Sent', // Should be Sent mailbox for sender
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          '["seen"]' // Should have seen flag
        ]),
        expect.any(Function)
      )
    })

    test('should increment UID counter for each stored message', async () => {
      // Arrange
      const mockEmail = 'test@example.com'
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {count: 5})
      })

      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      // Act
      await mockSMTPOptions.onData('mock-stream', {user: mockEmail}, jest.fn())

      // Assert
      expect(mockDb.run).toHaveBeenCalledWith(
        expect.stringContaining('INSERT INTO mail_received'),
        expect.arrayContaining([
          6, // Should be count + 1
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String),
          expect.any(String)
        ]),
        expect.any(Function)
      )
    })
  })

  describe('SMTP Authentication', () => {
    let mockSMTPOptions

    beforeEach(() => {
      Mail.init()
      mockSMTPOptions = SMTPServer.mock.calls[0][0]
    })

    test('should authenticate valid mail account credentials', async () => {
      // Arrange
      const mockAuth = {
        username: 'test@example.com',
        password: 'correctpassword'
      }
      const mockSession = {
        remoteAddress: '127.0.0.1'
      }
      const mockCallback = jest.fn()

      // Mock exists method to return user
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {email: 'test@example.com', password: '$2b$10$hashedpassword'})
      })

      // Act
      await mockSMTPOptions.onAuth(mockAuth, mockSession, mockCallback)

      // Assert
      expect(bcrypt.compare).toHaveBeenCalledWith('correctpassword', '$2b$10$hashedpassword')
      expect(mockCallback).toHaveBeenCalledWith(null, {user: 'test@example.com'})
    })

    test('should reject invalid credentials', async () => {
      // Arrange
      const mockAuth = {
        username: 'test@example.com',
        password: 'wrongpassword'
      }
      const mockSession = {
        remoteAddress: '127.0.0.1'
      }
      const mockCallback = jest.fn()

      // Mock exists method to return user
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {email: 'test@example.com', password: '$2b$10$hashedpassword'})
      })

      // Act
      await mockSMTPOptions.onAuth(mockAuth, mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toBe('Invalid username or password')
    })

    test('should reject invalid email format', async () => {
      // Arrange
      const mockAuth = {
        username: 'invalid-email',
        password: 'password'
      }
      const mockSession = {
        remoteAddress: '127.0.0.1'
      }
      const mockCallback = jest.fn()

      // Act
      await mockSMTPOptions.onAuth(mockAuth, mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toBe('Invalid username or password')
    })

    test('should implement rate limiting for failed attempts', async () => {
      // Arrange
      const mockAuth = {
        username: 'test@example.com',
        password: 'wrongpassword'
      }
      const mockSession = {
        remoteAddress: '192.168.1.100'
      }
      const mockCallback = jest.fn()

      // Mock exists method to return user
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {email: 'test@example.com', password: '$2b$10$hashedpassword'})
      })

      // First failed attempt
      await mockSMTPOptions.onAuth(mockAuth, mockSession, jest.fn())

      // Second failed attempt (should trigger rate limiting)
      await mockSMTPOptions.onAuth(mockAuth, mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toContain('Too many attempts')
    })

    test('should reset rate limiting after timeout', async () => {
      // Arrange
      const mockAuth = {
        username: 'test@example.com',
        password: 'wrongpassword'
      }
      const mockSession = {
        remoteAddress: '192.168.1.100'
      }

      // Mock exists method to return user
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {email: 'test@example.com', password: '$2b$10$hashedpassword'})
      })

      // Simulate old failed attempt (more than 1 hour ago)
      const oldTimestamp = Date.now() - 1000 * 60 * 60 * 2 // 2 hours ago
      jest.spyOn(Date, 'now').mockReturnValue(oldTimestamp)

      await mockSMTPOptions.onAuth(mockAuth, mockSession, jest.fn())

      // Reset Date.now to current time
      Date.now.mockRestore()

      const mockCallback = jest.fn()

      // Act - should not be rate limited
      await mockSMTPOptions.onAuth(mockAuth, mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toBe('Invalid username or password')
    })
  })

  describe('Mail Sending Functionality', () => {
    beforeEach(() => {
      Mail.init()
    })

    test('should trigger SMTP send for authenticated user mail', async () => {
      // Arrange
      const mockParsedMail = createMockEmailMessage('sender@example.com', 'recipient@external.com')
      const mockSession = {user: 'sender@example.com'}
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Act
      await mockSMTPOptions.onData('mock-stream', mockSession, jest.fn())

      // Assert
      expect(smtp.send).toHaveBeenCalledWith(mockParsedMail)
    })

    test('should not trigger SMTP send for received mail', async () => {
      // Arrange
      const mockParsedMail = createMockEmailMessage('external@other.com', 'recipient@example.com')
      const mockSession = {user: null}
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Mock exists method to return recipient
      mockDb.get.mockImplementation((sql, params, callback) => {
        if (params[0] === 'recipient@example.com') {
          callback(null, {email: 'recipient@example.com'})
        } else {
          callback(null, null)
        }
      })

      // Act
      await mockSMTPOptions.onData('mock-stream', mockSession, jest.fn())

      // Assert
      expect(smtp.send).not.toHaveBeenCalled()
    })

    test('should validate sender matches authenticated user', async () => {
      // Arrange
      const mockParsedMail = createMockEmailMessage('different@example.com', 'recipient@external.com')
      const mockSession = {user: 'sender@example.com'}
      const mockCallback = jest.fn()
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Mock exists method to return sender
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, {email: 'different@example.com'})
      })

      // Act
      await mockSMTPOptions.onData('mock-stream', mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toBe('Unexpected sender')
    })

    test('should validate recipient exists for external senders', async () => {
      // Arrange
      const mockParsedMail = createMockEmailMessage('external@other.com', 'nonexistent@example.com')
      const mockSession = {user: null}
      const mockCallback = jest.fn()
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Mock exists method to return null for both sender and recipient
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, null)
      })

      // Act
      await mockSMTPOptions.onData('mock-stream', mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith(expect.any(Error))
      expect(mockCallback.mock.calls[0][0].message).toBe('Unexpected recipient')
    })

    test('should allow mail to hostmaster and postmaster accounts', async () => {
      // Arrange
      const mockParsedMail = createMockEmailMessage('external@other.com', 'hostmaster@example.com')
      const mockSession = {user: null}
      const mockCallback = jest.fn()
      const mockSMTPOptions = SMTPServer.mock.calls[0][0]

      simpleParser.mockImplementation((stream, options, callback) => {
        if (typeof options === 'function') {
          callback = options
        }
        callback(null, mockParsedMail)
      })

      // Mock exists method to return null (no account exists)
      mockDb.get.mockImplementation((sql, params, callback) => {
        callback(null, null)
      })

      // Act
      await mockSMTPOptions.onData('mock-stream', mockSession, mockCallback)

      // Assert
      expect(mockCallback).toHaveBeenCalledWith() // No error
    })
  })

  describe('Server Initialization and Database Setup', () => {
    beforeEach(() => {
      // Reset mocks before each test
      jest.clearAllMocks()

      // Ensure Mail module is properly initialized for these tests
      if (Mail._resetForTesting) {
        Mail._resetForTesting()
      }
    })

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

    test('should setup SSL/TLS configuration with SNI callback', () => {
      // Arrange
      mockConfig.config.ssl = {
        key: '/etc/ssl/private/default.key',
        cert: '/etc/ssl/certs/default.crt'
      }
      fs.existsSync.mockReturnValue(true)
      fs.readFileSync.mockReturnValue('mock-cert-content')

      // Act
      Mail.init()

      // Assert
      const smtpOptions = SMTPServer.mock.calls[1][0] // Second call is for secure SMTP
      expect(smtpOptions).toHaveProperty('SNICallback')
      expect(smtpOptions.secure).toBe(true)
      expect(tls.createSecureContext).toHaveBeenCalled()
    })

    test('should handle database connection errors', () => {
      // Arrange
      const dbError = new Error('Database connection failed')
      sqlite3.verbose.mockReturnValue({
        Database: jest.fn().mockImplementation((path, callback) => {
          callback(dbError)
          return mockDb
        })
      })

      // Mock error logging
      const mockError = jest.fn()
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

    test('should create database directory if it does not exist', () => {
      // Arrange
      fs.existsSync.mockReturnValue(false)

      // Act
      Mail.init()

      // Assert
      expect(fs.mkdirSync).toHaveBeenCalledWith('/home/user/.odac/db', {recursive: true})
    })

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

    test('should handle SMTP server errors', () => {
      // Arrange
      const mockError = jest.fn()
      global.Odac.setMock('server', 'Log', {
        init: jest.fn().mockReturnValue({
          log: mockError,
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
      expect(mockError).toHaveBeenCalledWith('SMTP Server Error: ', testError)
    })

    test('should verify SSL certificate paths exist for SNI callback', () => {
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
      fs.readFileSync.mockReturnValue('mock-cert-content')

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
  })

  describe('Mail Account Management Operations', () => {
    beforeEach(() => {
      Mail.init()
    })

    describe('Account Creation', () => {
      test('should create mail account with valid email and password', async () => {
        // Arrange
        const email = 'newuser@example.com'
        const password = 'testpassword'
        const retype = 'testpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null) // Account doesn't exist
        })

        // Act
        const result = await Mail.create(email, password, retype)

        // Assert
        expect(bcrypt.hash).toHaveBeenCalledWith(password, 10, expect.any(Function))
        expect(mockDb.prepare).toHaveBeenCalledWith("INSERT INTO mail_account ('email', 'password', 'domain') VALUES (?, ?, ?)")
        expect(mockApi.result).toHaveBeenCalledWith(true, expect.stringContaining('created successfully'))
      })

      test('should validate email format during account creation', async () => {
        // Arrange
        const invalidEmail = 'invalid-email'
        const password = 'testpassword'
        const retype = 'testpassword'

        // Act
        const result = await Mail.create(invalidEmail, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Invalid email address'))
        expect(bcrypt.hash).not.toHaveBeenCalled()
      })

      test('should reject account creation if passwords do not match', async () => {
        // Arrange
        const email = 'test@example.com'
        const password = 'password1'
        const retype = 'password2'

        // Act
        const result = await Mail.create(email, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Passwords do not match'))
        expect(bcrypt.hash).not.toHaveBeenCalled()
      })

      test('should reject account creation if required fields are missing', async () => {
        // Act
        const result = await Mail.create('', 'password', 'password')

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('All fields are required'))
      })

      test('should reject account creation if account already exists', async () => {
        // Arrange
        const email = 'existing@example.com'
        const password = 'testpassword'
        const retype = 'testpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, {email: email, password: '$2b$10$hashedpassword'})
        })

        // Act
        const result = await Mail.create(email, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('already exists'))
      })

      test('should reject account creation for unknown domain', async () => {
        // Arrange
        const email = 'test@unknown.com'
        const password = 'testpassword'
        const retype = 'testpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null) // Account doesn't exist
        })

        // Act
        const result = await Mail.create(email, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Domain unknown.com not found'))
      })

      test('should hash password with bcrypt during account creation', async () => {
        // Arrange
        const email = 'test@example.com'
        const password = 'plainpassword'
        const retype = 'plainpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null) // Account doesn't exist
        })

        // Act
        await Mail.create(email, password, retype)

        // Assert
        expect(bcrypt.hash).toHaveBeenCalledWith(password, 10, expect.any(Function))

        const preparedStatement = mockDb.prepare.mock.results[0].value
        expect(preparedStatement.run).toHaveBeenCalledWith(email, '$2b$10$hashedpassword', 'example.com')
      })
    })

    describe('Account Deletion', () => {
      test('should delete existing mail account', async () => {
        // Arrange
        const email = 'delete@example.com'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, {email: email, password: '$2b$10$hashedpassword'})
        })

        // Act
        const result = await Mail.delete(email)

        // Assert
        expect(mockDb.prepare).toHaveBeenCalledWith('DELETE FROM mail_account WHERE email = ?')
        const preparedStatement = mockDb.prepare.mock.results[0].value
        expect(preparedStatement.run).toHaveBeenCalledWith(email)
        expect(mockApi.result).toHaveBeenCalledWith(true, expect.stringContaining('deleted successfully'))
      })

      test('should validate email format during account deletion', async () => {
        // Arrange
        const invalidEmail = 'invalid-email'

        // Act
        const result = await Mail.delete(invalidEmail)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Invalid email address'))
        expect(mockDb.prepare).not.toHaveBeenCalled()
      })

      test('should reject deletion if email is required but not provided', async () => {
        // Act
        const result = await Mail.delete('')

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Email address is required'))
      })

      test('should reject deletion if account does not exist', async () => {
        // Arrange
        const email = 'nonexistent@example.com'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null) // Account doesn't exist
        })

        // Act
        const result = await Mail.delete(email)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('not found'))
        expect(mockDb.prepare).not.toHaveBeenCalled()
      })
    })

    describe('Account Existence Checking', () => {
      test('should return account data if account exists', async () => {
        // Arrange
        const email = 'existing@example.com'
        const mockAccount = {email: email, password: '$2b$10$hashedpassword'}

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, mockAccount)
        })

        // Act
        const result = await Mail.exists(email)

        // Assert
        expect(result).toEqual(mockAccount)
        expect(mockDb.get).toHaveBeenCalledWith('SELECT * FROM mail_account WHERE email = ?', [email], expect.any(Function))
      })

      test('should return false if account does not exist', async () => {
        // Arrange
        const email = 'nonexistent@example.com'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null)
        })

        // Act
        const result = await Mail.exists(email)

        // Assert
        expect(result).toBe(false)
      })

      test('should handle database errors during existence check', async () => {
        // Arrange
        const email = 'test@example.com'
        const dbError = new Error('Database error')

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(dbError, null)
        })

        // Act
        const result = await Mail.exists(email)

        // Assert
        expect(result).toBe(false)
      })
    })

    describe('Password Update', () => {
      test('should update password for existing account', async () => {
        // Arrange
        const email = 'test@example.com'
        const password = 'newpassword'
        const retype = 'newpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, {email: email, password: '$2b$10$oldhashedpassword'})
        })

        // Act
        const result = await Mail.password(email, password, retype)

        // Assert
        expect(bcrypt.hash).toHaveBeenCalledWith(password, 10, expect.any(Function))
        expect(mockDb.prepare).toHaveBeenCalledWith('UPDATE mail_account SET password = ? WHERE email = ?')
        const preparedStatement = mockDb.prepare.mock.results[0].value
        expect(preparedStatement.run).toHaveBeenCalledWith('$2b$10$hashedpassword', email)
        expect(mockApi.result).toHaveBeenCalledWith(true, expect.stringContaining('password updated successfully'))
      })

      test('should validate email format during password update', async () => {
        // Arrange
        const invalidEmail = 'invalid-email'
        const password = 'newpassword'
        const retype = 'newpassword'

        // Act
        const result = await Mail.password(invalidEmail, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Invalid email address'))
        expect(bcrypt.hash).not.toHaveBeenCalled()
      })

      test('should reject password update if passwords do not match', async () => {
        // Arrange
        const email = 'test@example.com'
        const password = 'password1'
        const retype = 'password2'

        // Act
        const result = await Mail.password(email, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Passwords do not match'))
        expect(bcrypt.hash).not.toHaveBeenCalled()
      })

      test('should reject password update if required fields are missing', async () => {
        // Act
        const result = await Mail.password('', 'password', 'password')

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('All fields are required'))
      })

      test('should reject password update if account does not exist', async () => {
        // Arrange
        const email = 'nonexistent@example.com'
        const password = 'newpassword'
        const retype = 'newpassword'

        mockDb.get.mockImplementation((sql, params, callback) => {
          callback(null, null) // Account doesn't exist
        })

        // Act
        const result = await Mail.password(email, password, retype)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('not found'))
        expect(mockDb.prepare).not.toHaveBeenCalled()
      })
    })

    describe('Account Listing', () => {
      test('should list all accounts for a domain', async () => {
        // Arrange
        const domain = 'example.com'
        const mockAccounts = [{email: 'user1@example.com'}, {email: 'user2@example.com'}, {email: 'user3@example.com'}]

        mockDb.each.mockImplementation((sql, params, rowCallback, completeCallback) => {
          mockAccounts.forEach(account => rowCallback(null, account))
          completeCallback(null, mockAccounts.length)
        })

        // Act
        const result = await Mail.list(domain)

        // Assert
        expect(mockDb.each).toHaveBeenCalledWith(
          'SELECT * FROM mail_account WHERE domain = ?',
          [domain],
          expect.any(Function),
          expect.any(Function)
        )
        expect(mockApi.result).toHaveBeenCalledWith(
          true,
          expect.stringContaining('user1@example.com\nuser2@example.com\nuser3@example.com')
        )
      })

      test('should reject listing if domain is not provided', async () => {
        // Act
        const result = await Mail.list('')

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Domain is required'))
      })

      test('should reject listing for unknown domain', async () => {
        // Arrange
        const domain = 'unknown.com'

        // Act
        const result = await Mail.list(domain)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(false, expect.stringContaining('Domain unknown.com not found'))
      })

      test('should handle empty account list for domain', async () => {
        // Arrange
        const domain = 'example.com'

        mockDb.each.mockImplementation((sql, params, rowCallback, completeCallback) => {
          completeCallback(null, 0)
        })

        // Act
        const result = await Mail.list(domain)

        // Assert
        expect(mockApi.result).toHaveBeenCalledWith(true, expect.stringContaining('Mail accounts for domain example.com'))
      })
    })
  })
})
