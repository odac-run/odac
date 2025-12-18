/**
 * Mock implementation of the fs module for server tests
 * Provides comprehensive mocking of file system operations
 */

const mockFiles = new Map()
const mockDirectories = new Set()

// Helper to simulate file system state
const resetFileSystem = () => {
  mockFiles.clear()
  mockDirectories.clear()

  // Add some default directories
  mockDirectories.add('/var/odac')
  mockDirectories.add('/etc/ssl/private')
  mockDirectories.add('/etc/ssl/certs')
  mockDirectories.add('/home/user/.odac')
}

// Initialize with default state
resetFileSystem()

const fs = {
  // Synchronous file operations
  readFileSync: jest.fn((path, encoding) => {
    if (!mockFiles.has(path)) {
      const error = new Error(`ENOENT: no such file or directory, open '${path}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = path
      throw error
    }

    const content = mockFiles.get(path)
    return encoding ? content : Buffer.from(content)
  }),

  writeFileSync: jest.fn((path, data, options) => {
    const content = typeof data === 'string' ? data : data.toString()
    mockFiles.set(path, content)
  }),

  appendFileSync: jest.fn((path, data, options) => {
    const content = typeof data === 'string' ? data : data.toString()
    const existing = mockFiles.get(path) || ''
    mockFiles.set(path, existing + content)
  }),

  existsSync: jest.fn(path => {
    return mockFiles.has(path) || mockDirectories.has(path)
  }),

  statSync: jest.fn(path => {
    if (!mockFiles.has(path) && !mockDirectories.has(path)) {
      const error = new Error(`ENOENT: no such file or directory, stat '${path}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = path
      throw error
    }

    const isDirectory = mockDirectories.has(path)
    return {
      isFile: () => !isDirectory,
      isDirectory: () => isDirectory,
      isSymbolicLink: () => false,
      size: isDirectory ? 0 : (mockFiles.get(path) || '').length,
      mtime: new Date(),
      ctime: new Date(),
      atime: new Date(),
      mode: isDirectory ? 16877 : 33188,
      uid: 1000,
      gid: 1000
    }
  }),

  lstatSync: jest.fn(path => fs.statSync(path)),

  readdirSync: jest.fn(path => {
    if (!mockDirectories.has(path)) {
      const error = new Error(`ENOENT: no such file or directory, scandir '${path}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = path
      throw error
    }

    const entries = []

    // Find files in this directory
    for (const filePath of mockFiles.keys()) {
      if (filePath.startsWith(path + '/')) {
        const relativePath = filePath.substring(path.length + 1)
        if (!relativePath.includes('/')) {
          entries.push(relativePath)
        }
      }
    }

    // Find subdirectories
    for (const dirPath of mockDirectories) {
      if (dirPath.startsWith(path + '/')) {
        const relativePath = dirPath.substring(path.length + 1)
        if (!relativePath.includes('/')) {
          entries.push(relativePath)
        }
      }
    }

    return entries
  }),

  mkdirSync: jest.fn((path, options) => {
    mockDirectories.add(path)

    // Create parent directories if recursive option is set
    if (options && options.recursive) {
      const parts = path.split('/')
      let currentPath = ''
      for (const part of parts) {
        if (part) {
          currentPath += '/' + part
          mockDirectories.add(currentPath)
        }
      }
    }
  }),

  rmdirSync: jest.fn((path, options) => {
    mockDirectories.delete(path)

    // Remove files in directory if recursive
    if (options && options.recursive) {
      for (const filePath of mockFiles.keys()) {
        if (filePath.startsWith(path + '/')) {
          mockFiles.delete(filePath)
        }
      }

      for (const dirPath of mockDirectories) {
        if (dirPath.startsWith(path + '/')) {
          mockDirectories.delete(dirPath)
        }
      }
    }
  }),

  unlinkSync: jest.fn(path => {
    if (!mockFiles.has(path)) {
      const error = new Error(`ENOENT: no such file or directory, unlink '${path}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = path
      throw error
    }
    mockFiles.delete(path)
  }),

  copyFileSync: jest.fn((src, dest) => {
    if (!mockFiles.has(src)) {
      const error = new Error(`ENOENT: no such file or directory, open '${src}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = src
      throw error
    }
    mockFiles.set(dest, mockFiles.get(src))
  }),

  cpSync: jest.fn((src, dest, options) => {
    // Mock recursive copy operation
    if (mockDirectories.has(src)) {
      // Copy directory
      mockDirectories.add(dest)

      // Copy all files in the directory
      for (const filePath of mockFiles.keys()) {
        if (filePath.startsWith(src + '/')) {
          const relativePath = filePath.substring(src.length)
          mockFiles.set(dest + relativePath, mockFiles.get(filePath))
        }
      }

      // Copy all subdirectories
      for (const dirPath of mockDirectories) {
        if (dirPath.startsWith(src + '/')) {
          const relativePath = dirPath.substring(src.length)
          mockDirectories.add(dest + relativePath)
        }
      }
    } else if (mockFiles.has(src)) {
      // Copy single file
      mockFiles.set(dest, mockFiles.get(src))
    }
  }),

  rmSync: jest.fn((path, options) => {
    if (mockDirectories.has(path)) {
      // Remove directory
      mockDirectories.delete(path)

      // Remove files in directory if recursive
      if (options && options.recursive) {
        for (const filePath of mockFiles.keys()) {
          if (filePath.startsWith(path + '/')) {
            mockFiles.delete(filePath)
          }
        }

        for (const dirPath of mockDirectories) {
          if (dirPath.startsWith(path + '/')) {
            mockDirectories.delete(dirPath)
          }
        }
      }
    } else if (mockFiles.has(path)) {
      // Remove single file
      mockFiles.delete(path)
    }
  }),

  chmodSync: jest.fn((path, mode) => {
    // Mock implementation - just verify file exists
    if (!mockFiles.has(path) && !mockDirectories.has(path)) {
      const error = new Error(`ENOENT: no such file or directory, chmod '${path}'`)
      error.code = 'ENOENT'
      error.errno = -2
      error.path = path
      throw error
    }
  }),

  // Callback-style asynchronous operations
  readFile: jest.fn((path, encoding, callback) => {
    if (typeof encoding === 'function') {
      callback = encoding
      encoding = 'utf8'
    }

    setTimeout(() => {
      try {
        const content = fs.readFileSync(path, encoding)
        callback(null, content)
      } catch (error) {
        callback(error)
      }
    }, 0)
  }),

  writeFile: jest.fn((path, data, encoding, callback) => {
    if (typeof encoding === 'function') {
      callback = encoding
      encoding = 'utf8'
    }

    setTimeout(() => {
      try {
        fs.writeFileSync(path, data, {encoding})
        callback(null)
      } catch (error) {
        callback(error)
      }
    }, 0)
  }),

  appendFile: jest.fn((path, data, encoding, callback) => {
    if (typeof encoding === 'function') {
      callback = encoding
      encoding = 'utf8'
    }

    setTimeout(() => {
      try {
        fs.appendFileSync(path, data, {encoding})
        callback(null)
      } catch (error) {
        callback(error)
      }
    }, 0)
  }),

  // Asynchronous file operations
  promises: {
    readFile: jest.fn(async (path, encoding) => {
      return fs.readFileSync(path, encoding)
    }),

    writeFile: jest.fn(async (path, data, options) => {
      return fs.writeFileSync(path, data, options)
    }),

    appendFile: jest.fn(async (path, data, options) => {
      return fs.appendFileSync(path, data, options)
    }),

    access: jest.fn(async (path, mode) => {
      if (!mockFiles.has(path) && !mockDirectories.has(path)) {
        const error = new Error(`ENOENT: no such file or directory, access '${path}'`)
        error.code = 'ENOENT'
        error.errno = -2
        error.path = path
        throw error
      }
    }),

    stat: jest.fn(async path => {
      return fs.statSync(path)
    }),

    lstat: jest.fn(async path => {
      return fs.lstatSync(path)
    }),

    readdir: jest.fn(async path => {
      return fs.readdirSync(path)
    }),

    mkdir: jest.fn(async (path, options) => {
      return fs.mkdirSync(path, options)
    }),

    rmdir: jest.fn(async (path, options) => {
      return fs.rmdirSync(path, options)
    }),

    unlink: jest.fn(async path => {
      return fs.unlinkSync(path)
    }),

    copyFile: jest.fn(async (src, dest) => {
      return fs.copyFileSync(src, dest)
    }),

    chmod: jest.fn(async (path, mode) => {
      return fs.chmodSync(path, mode)
    })
  },

  // Stream operations
  createReadStream: jest.fn((path, options) => {
    const {createMockStream} = require('./testHelpers')
    const stream = createMockStream(true, false)

    // Simulate reading file content
    setTimeout(() => {
      if (mockFiles.has(path)) {
        stream.emit('data', Buffer.from(mockFiles.get(path)))
        stream.emit('end')
      } else {
        const error = new Error(`ENOENT: no such file or directory, open '${path}'`)
        error.code = 'ENOENT'
        stream.emit('error', error)
      }
    }, 0)

    return stream
  }),

  createWriteStream: jest.fn((path, options) => {
    const {createMockStream} = require('./testHelpers')
    const stream = createMockStream(false, true)

    let content = ''
    stream.write.mockImplementation(chunk => {
      content += chunk.toString()
      return true
    })

    stream.end.mockImplementation(chunk => {
      if (chunk) content += chunk.toString()
      mockFiles.set(path, content)
      stream.emit('finish')
    })

    return stream
  }),

  // Watch operations
  watch: jest.fn((filename, options, listener) => {
    const {createMockEventEmitter} = require('./testHelpers')
    const watcher = createMockEventEmitter()

    watcher.close = jest.fn()

    if (typeof options === 'function') {
      listener = options
      options = {}
    }

    if (listener) {
      watcher.on('change', listener)
    }

    return watcher
  }),

  watchFile: jest.fn((filename, options, listener) => {
    if (typeof options === 'function') {
      listener = options
      options = {}
    }

    // Mock implementation - just call listener immediately
    if (listener) {
      setTimeout(() => {
        const stats = fs.statSync(filename)
        listener(stats, stats)
      }, 0)
    }
  }),

  unwatchFile: jest.fn((filename, listener) => {
    // Mock implementation - no-op
  }),

  // Constants
  constants: {
    F_OK: 0,
    R_OK: 4,
    W_OK: 2,
    X_OK: 1,
    O_RDONLY: 0,
    O_WRONLY: 1,
    O_RDWR: 2,
    O_CREAT: 64,
    O_EXCL: 128,
    O_TRUNC: 512,
    O_APPEND: 1024
  },

  // Test helpers
  __setMockFiles: files => {
    mockFiles.clear()
    Object.entries(files).forEach(([path, content]) => {
      mockFiles.set(path, content)
    })
  },

  __setMockDirectories: directories => {
    mockDirectories.clear()
    directories.forEach(dir => mockDirectories.add(dir))
  },

  __getMockFiles: () => new Map(mockFiles),
  __getMockDirectories: () => new Set(mockDirectories),
  __resetFileSystem: resetFileSystem
}

module.exports = fs
