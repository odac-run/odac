const Log = require('../../core/Log')

describe('Log', () => {
  let consoleLogSpy
  let consoleErrorSpy
  let consoleWarnSpy

  beforeEach(() => {
    consoleLogSpy = jest.spyOn(console, 'log').mockImplementation(() => {})
    consoleErrorSpy = jest.spyOn(console, 'error').mockImplementation(() => {})
    consoleWarnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {})
  })

  afterEach(() => {
    consoleLogSpy.mockRestore()
    consoleErrorSpy.mockRestore()
    consoleWarnSpy.mockRestore()
  })

  it('should initialize with a module prefix', () => {
    const log = new Log()
    const logger = log.init('TestModule')
    logger.log('test message')
    expect(consoleLogSpy).toHaveBeenCalledWith('[TestModule] ', 'test message')
  })

  it('should handle multiple module prefixes', () => {
    const log = new Log()
    const logger = log.init('Module1', 'Module2')
    logger.log('test message')
    expect(consoleLogSpy).toHaveBeenCalledWith('[Module1][Module2] ', 'test message')
  })

  it('should log messages correctly', () => {
    const log = new Log()
    const logger = log.init('Test')
    logger.log('message1', 'message2')
    expect(consoleLogSpy).toHaveBeenCalledWith('[Test] ', 'message1', 'message2')
  })

  it('should log error messages correctly', () => {
    const log = new Log()
    const logger = log.init('Test')
    logger.error('error message')
    expect(consoleErrorSpy).toHaveBeenCalledWith('[Test] ', 'error message')
  })

  it('should handle %s format specifiers', () => {
    const log = new Log()
    const logger = log.init('FormatTest')
    logger.log('Hello, %s!', 'World')
    expect(consoleLogSpy).toHaveBeenCalledWith('[FormatTest] ', 'Hello, World!')
  })

  it('should handle multiple %s format specifiers', () => {
    const log = new Log()
    const logger = log.init('FormatTest')
    logger.log('Hello, %s! Welcome, %s.', 'John', 'Jane')
    expect(consoleLogSpy).toHaveBeenCalledWith('[FormatTest] ', 'Hello, John! Welcome, Jane.')
  })

  it('should handle missing arguments for %s', () => {
    const log = new Log()
    const logger = log.init('FormatTest')
    logger.log('Hello, %s!')
    expect(consoleLogSpy).toHaveBeenCalledWith('[FormatTest] ', 'Hello, !')
  })

  it('should not log anything if no arguments are provided to log()', () => {
    const log = new Log()
    const logger = log.init('Test')
    const result = logger.log()
    expect(consoleLogSpy).not.toHaveBeenCalled()
    expect(result).toBeInstanceOf(Log)
  })

  it('should log warn messages correctly', () => {
    const log = new Log()
    const logger = log.init('Test')
    logger.warn('warning message')
    expect(consoleWarnSpy).toHaveBeenCalledWith('[Test] ', 'warning message')
  })

  it('should sanitize sensitive data in warn messages', () => {
    const log = new Log()
    const logger = log.init('Test')
    logger.warn('data', {password: 'secret123', name: 'test'})
    expect(consoleWarnSpy).toHaveBeenCalledWith('[Test] ', 'data', {password: '***', name: 'test'})
  })
})
