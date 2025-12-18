/**
 * Unit tests for Mail.js account management operations
 * Tests mail account CRUD operations with validation
 */

const {setupGlobalMocks, cleanupGlobalMocks} = require('./__mocks__/testHelpers')
const {createMockWebsiteConfig, createMockMailAccount} = require('./__mocks__/testFactories')

// Mock external dependencies
jest.mock('bcrypt')
jest.mock('sqlite3')

const bcrypt = require('bcrypt')
const sqlite3 = require('sqlite3')

describe('Mail Module - Account Management Operations', () => {
  let Mail
  let mockDb
  let mockConfig
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
            }
          })
        }
      }
    }

    // Setup mock API service
    mockApi = {
      result: jest.fn((success, data) => ({success, data}))
    }

    // Setup global Odac mocks
    global.Odac.setMock('core', 'Config', mockConfig)
    global.Odac.setMock('server', 'Api', mockApi)

    // Setup bcrypt mock
    bcrypt.hash.mockImplementation((password, rounds, callback) => {
      callback(null, '$2b$10$hashedpassword')
    })

    // Setup global __ function mock
    global.__ = jest.fn().mockImplementation((key, ...args) => {
      return Promise.resolve(key.replace(/%s/g, () => args.shift() || '%s'))
    })

    // Clear module cache and require fresh instance
    jest.resetModules()
    Mail = require('../../server/src/Mail')

    // Initialize Mail module
    Mail.init()
  })

  afterEach(() => {
    cleanupGlobalMocks()
    jest.clearAllMocks()
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
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Invalid email address.')
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
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Passwords do not match.')
      expect(bcrypt.hash).not.toHaveBeenCalled()
    })

    test('should reject account creation if required fields are missing', async () => {
      // Act
      const result = await Mail.create('', 'password', 'password')

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'All fields are required.')
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
      expect(mockApi.result).toHaveBeenCalledWith(false, `Mail account ${email} already exists.`)
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
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Domain unknown.com not found.')
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
      expect(mockApi.result).toHaveBeenCalledWith(true, `Mail account ${email} deleted successfully.`)
    })

    test('should validate email format during account deletion', async () => {
      // Arrange
      const invalidEmail = 'invalid-email'

      // Act
      const result = await Mail.delete(invalidEmail)

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Invalid email address.')
      expect(mockDb.prepare).not.toHaveBeenCalled()
    })

    test('should reject deletion if email is required but not provided', async () => {
      // Act
      const result = await Mail.delete('')

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Email address is required.')
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
      expect(mockApi.result).toHaveBeenCalledWith(false, `Mail account ${email} not found.`)
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
      expect(mockApi.result).toHaveBeenCalledWith(true, `Mail account ${email} password updated successfully.`)
    })

    test('should validate email format during password update', async () => {
      // Arrange
      const invalidEmail = 'invalid-email'
      const password = 'newpassword'
      const retype = 'newpassword'

      // Act
      const result = await Mail.password(invalidEmail, password, retype)

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Invalid email address.')
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
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Passwords do not match.')
      expect(bcrypt.hash).not.toHaveBeenCalled()
    })

    test('should reject password update if required fields are missing', async () => {
      // Act
      const result = await Mail.password('', 'password', 'password')

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'All fields are required.')
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
      expect(mockApi.result).toHaveBeenCalledWith(false, `Mail account ${email} not found.`)
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
      expect(mockApi.result).toHaveBeenCalledWith(true, expect.stringContaining('user1@example.com\nuser2@example.com\nuser3@example.com'))
    })

    test('should reject listing if domain is not provided', async () => {
      // Act
      const result = await Mail.list('')

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Domain is required.')
    })

    test('should reject listing for unknown domain', async () => {
      // Arrange
      const domain = 'unknown.com'

      // Act
      const result = await Mail.list(domain)

      // Assert
      expect(mockApi.result).toHaveBeenCalledWith(false, 'Domain unknown.com not found.')
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
      expect(mockApi.result).toHaveBeenCalledWith(true, `Mail accounts for domain ${domain}.\n`)
    })
  })
})
