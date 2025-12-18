// Mock fs before requiring Lang
jest.mock('fs', () => ({
  promises: {
    writeFile: jest.fn().mockResolvedValue()
  },
  readFileSync: jest.fn()
}))

const fs = require('fs')
const path = require('path')
const Lang = require('../../core/Lang')

describe('Lang', () => {
  let originalConsoleError

  beforeEach(() => {
    // Reset mocks before each test
    fs.readFileSync.mockClear()
    fs.promises.writeFile.mockClear()

    // Mock console.error to prevent noise in tests
    originalConsoleError = console.error
    console.error = jest.fn()
  })

  afterEach(() => {
    // Restore console.error
    console.error = originalConsoleError
  })

  it("should return 'Odac' for the 'Odac' key", () => {
    const lang = new Lang()
    expect(lang.get('Odac')).toBe('Odac')
  })

  it('should return the translation for an existing key', () => {
    const mockStrings = {'test.key': 'Test Value'}
    fs.readFileSync.mockReturnValue(JSON.stringify(mockStrings))

    const lang = new Lang()
    expect(lang.get('test.key')).toBe('Test Value')
  })

  it('should replace placeholders with arguments', () => {
    const mockStrings = {greeting: 'Hello, %s! Welcome, %s.'}
    fs.readFileSync.mockReturnValue(JSON.stringify(mockStrings))

    const lang = new Lang()
    expect(lang.get('greeting', 'John', 'Jane')).toBe('Hello, John! Welcome, Jane.')
  })

  it('should return the key and try to save it if it does not exist', () => {
    fs.readFileSync.mockImplementation(() => {
      throw new Error('File not found')
    })

    const lang = new Lang()
    const key = 'non.existent.key'

    expect(lang.get(key)).toBe(key)

    // Verify that save was called
    expect(fs.promises.writeFile).toHaveBeenCalled()
  })

  it('should handle multiple placeholders correctly', () => {
    const mockStrings = {format: '%s %s %s'}
    fs.readFileSync.mockReturnValue(JSON.stringify(mockStrings))
    const lang = new Lang()
    expect(lang.get('format', 'a', 'b', 'c')).toBe('a b c')
  })

  describe('File saving behavior', () => {
    it('should not modify existing file when all keys exist', () => {
      const existingStrings = {
        'existing.key1': 'Value 1',
        'existing.key2': 'Value 2',
        'existing.key3': 'Value 3'
      }

      fs.readFileSync.mockReturnValue(JSON.stringify(existingStrings))

      const lang = new Lang()

      // Access existing keys multiple times
      lang.get('existing.key1')
      lang.get('existing.key2')
      lang.get('existing.key3')
      lang.get('existing.key1') // Access again

      // Verify writeFile was only called once during initialization (if file didn't exist)
      // Since we're mocking readFileSync to return data, it should not call writeFile
      expect(fs.promises.writeFile).not.toHaveBeenCalled()
    })

    it('should save file with correct format when new key is added', () => {
      const existingStrings = {'existing.key': 'Existing Value'}
      fs.readFileSync.mockReturnValue(JSON.stringify(existingStrings))

      const lang = new Lang()

      // Add a new key
      const result = lang.get('new.key')

      expect(result).toBe('new.key')
      expect(fs.promises.writeFile).toHaveBeenCalledTimes(1)

      // Verify the file is saved with correct format and content
      const [filePath, content, encoding] = fs.promises.writeFile.mock.calls[0]

      expect(encoding).toBe('utf8')
      expect(filePath).toMatch(/locale\/.*\.json$/)

      const savedData = JSON.parse(content)
      expect(savedData).toEqual({
        'existing.key': 'Existing Value',
        'new.key': 'new.key'
      })

      // Verify JSON formatting (4 spaces indentation)
      expect(content).toMatch(/{\n    /)
    })

    it('should preserve existing data when adding new keys', () => {
      const existingStrings = {
        key1: 'Value 1',
        key2: 'Value 2'
      }

      fs.readFileSync.mockReturnValue(JSON.stringify(existingStrings))

      const lang = new Lang()

      // Add multiple new keys
      lang.get('new.key1')
      lang.get('new.key2')

      expect(fs.promises.writeFile).toHaveBeenCalledTimes(2)

      // Check the final saved state
      const lastCall = fs.promises.writeFile.mock.calls[fs.promises.writeFile.mock.calls.length - 1]
      const savedContent = JSON.parse(lastCall[1])

      expect(savedContent).toEqual({
        key1: 'Value 1',
        key2: 'Value 2',
        'new.key1': 'new.key1',
        'new.key2': 'new.key2'
      })
    })

    it('should handle file save errors gracefully', () => {
      fs.readFileSync.mockImplementation(() => {
        throw new Error('File not found')
      })

      // Mock writeFile to throw synchronously (simulating immediate error)
      fs.promises.writeFile.mockImplementation(() => {
        throw new Error('Write permission denied')
      })

      const lang = new Lang()

      // This should not throw an error even if save fails
      expect(() => lang.get('test.key')).not.toThrow()

      // Verify error was logged
      expect(console.error).toHaveBeenCalledWith('Error saving language file:', expect.any(Error))
    })

    it('should create file with empty object when file does not exist', () => {
      fs.readFileSync.mockImplementation(() => {
        throw new Error('ENOENT: no such file or directory')
      })

      const lang = new Lang()

      // Should save empty object initially
      expect(fs.promises.writeFile).toHaveBeenCalledTimes(1)

      const [filePath, content, encoding] = fs.promises.writeFile.mock.calls[0]
      expect(content).toBe('{}')
      expect(encoding).toBe('utf8')
    })

    it('should not save duplicate keys', () => {
      const existingStrings = {'existing.key': 'Existing Value'}
      fs.readFileSync.mockReturnValue(JSON.stringify(existingStrings))

      const lang = new Lang()

      // Access the same new key multiple times
      lang.get('new.key')
      lang.get('new.key')
      lang.get('new.key')

      // Should only save once when the key is first added
      expect(fs.promises.writeFile).toHaveBeenCalledTimes(1)
    })
  })

  describe('Locale file path validation', () => {
    it('should use correct locale file path', () => {
      const mockLocale = 'en-US'
      const originalIntl = global.Intl

      // Mock Intl to return specific locale
      global.Intl = {
        DateTimeFormat: () => ({
          resolvedOptions: () => ({locale: mockLocale})
        })
      }

      fs.readFileSync.mockImplementation(() => {
        throw new Error('File not found')
      })

      new Lang()

      expect(fs.promises.writeFile).toHaveBeenCalledWith(expect.stringContaining(`locale/${mockLocale}.json`), expect.any(String), 'utf8')

      // Restore original Intl
      global.Intl = originalIntl
    })
  })

  describe('Mock verification', () => {
    it('should ensure fs operations are mocked and not writing real files', () => {
      // Verify that fs.promises.writeFile is a mock function
      expect(jest.isMockFunction(fs.promises.writeFile)).toBe(true)
      expect(jest.isMockFunction(fs.readFileSync)).toBe(true)

      // Create a Lang instance that would normally write to file
      fs.readFileSync.mockImplementation(() => {
        throw new Error('File not found')
      })

      const lang = new Lang()
      lang.get('test.mock.key')

      // Verify mock was called but no real file operation occurred
      expect(fs.promises.writeFile).toHaveBeenCalled()

      // The mock should have been called with the expected parameters
      const [filePath, content] = fs.promises.writeFile.mock.calls[0]
      expect(filePath).toMatch(/locale\/.*\.json$/)
      expect(content).toBe('{}')
    })
  })
})
