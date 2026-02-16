const Builder = require('../../server/src/Container/Builder')
const fsPromises = require('fs/promises')
const path = require('path')

// Mock Odac core
global.Odac = {
  core: jest.fn().mockReturnValue({
    init: jest.fn().mockReturnValue({
      log: jest.fn(),
      error: jest.fn()
    })
  })
}

// Mock Dependencies
jest.mock('fs/promises')
jest.mock('dockerode')

describe('Builder', () => {
  let builder
  let mockDocker
  let mockStream

  const mockContext = {
    internalPath: '/internal/app/path',
    hostPath: '/host/app/path'
  }

  beforeEach(() => {
    // Reset mocks
    jest.clearAllMocks()

    // Mock Docker stream (result object)
    mockStream = {
      StatusCode: 0,
      on: jest.fn(),
      setEncoding: jest.fn()
    }

    // Mock Docker instance
    mockDocker = {
      run: jest.fn().mockResolvedValue([mockStream]),
      pull: jest.fn((img, cb) => cb(null, {})),
      getImage: jest.fn().mockReturnValue({
        inspect: jest.fn().mockResolvedValue({Id: 'sha256:...'})
      }),
      modem: {
        followProgress: jest.fn((stream, cb) => cb(null, []))
      }
    }

    // Ensure fs methods return promises for catch() to work
    fsPromises.writeFile.mockResolvedValue()
    fsPromises.unlink.mockResolvedValue()
    fsPromises.access.mockResolvedValue()

    builder = new Builder(mockDocker)
  })

  describe('detect', () => {
    test('should detect Custom Dockerfile at top priority', async () => {
      // Return true for Dockerfile AND package.json to test priority
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('Dockerfile') || p.endsWith('package.json')) return
        throw new Error('ENOENT')
      })

      await builder.build(mockContext, 'custom-image')

      // Should NOT run node:lts-alpine (compile)
      // Should run docker:cli directly
      const calls = mockDocker.run.mock.calls
      const images = calls.map(c => c[0])
      expect(images).toContain('docker:cli')
      expect(images).not.toContain('node:lts-alpine')

      // Verify build command targets existing Dockerfile
      const cmd = calls.find(c => c[0] === 'docker:cli')[1]
      expect(cmd).toEqual(['sh', '-c', 'docker build --progress=plain -t custom-image /app'])
    })

    test('should detect PHP project if composer.json exists', async () => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('composer.json')) return
        throw new Error('ENOENT')
      })
      await builder.build(mockContext, 'php-image')
      expect(mockDocker.run).toHaveBeenCalledWith('composer:lts', expect.any(Array), expect.any(Object), expect.any(Object))
    })

    test('should detect Node.js project if package.json exists', async () => {
      // Return true only for package.json
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('package.json')) return
        throw new Error('ENOENT')
      })

      // Test via private method exposed functionality (we can't call private directly easily in JS without hacks)
      // Actually, we test via build() public interface

      // We assume compile/package will be called if detect succeeds
      // But to verify detection specifically, we can check if it proceeds to compile steps with correct strategy

      // Let's spy on docker.run

      await builder.build(mockContext, 'test-image')

      // Verify fs.existsSync was called with internal path
      expect(fsPromises.access).toHaveBeenCalledWith(path.join(mockContext.internalPath, 'package.json'), expect.anything())

      // Verify docker run was called with Node image (result of detection)
      expect(mockDocker.run).toHaveBeenCalledWith('node:lts-alpine', expect.any(Array), expect.any(Object), expect.any(Object))
    })

    test('should detect Python project if requirements.txt exists', async () => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('requirements.txt')) return
        throw new Error('ENOENT')
      })
      await builder.build(mockContext, 'python-image')
      expect(mockDocker.run).toHaveBeenCalledWith('python:3.11-slim', expect.any(Array), expect.any(Object), expect.any(Object))
    })

    test('should detect Go project if go.mod exists', async () => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('go.mod')) return
        throw new Error('ENOENT')
      })
      await builder.build(mockContext, 'go-image')
      expect(mockDocker.run).toHaveBeenCalledWith('golang:1.22-alpine', expect.any(Array), expect.any(Object), expect.any(Object))
    })

    test('should detect Static Web project if index.html exists', async () => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('index.html')) return
        throw new Error('ENOENT')
      })
      // Static web doesn't run compilation (image: alpine), so we expect alpine
      // Or whatever configured for "Static Web" strategy's image which is 'alpine:latest'
      // Note: Logic in detect says image: alpine:latest, installCmd: true
      await builder.build(mockContext, 'static-image')
      expect(mockDocker.run).toHaveBeenCalledWith('alpine:latest', expect.any(Array), expect.any(Object), expect.any(Object))
    })

    test('should throw if no project type detected', async () => {
      fsPromises.access.mockRejectedValue(new Error('ENOENT'))
      await expect(builder.build(mockContext, 'test-image')).rejects.toThrow('Could not detect project type')
    })
  })

  describe('compile', () => {
    beforeEach(() => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('package.json')) return
        throw new Error('ENOENT')
      })
    })

    test('should run compiler container with secure configuration', async () => {
      await builder.build(mockContext, 'test-image')

      // Check compilation step arguments
      const [image, cmd, , createOptions] = mockDocker.run.mock.calls[0]
      const hostConfig = createOptions.HostConfig

      expect(image).toBe('node:lts-alpine')

      // Validate command execution
      expect(cmd[0]).toBe('sh')
      expect(cmd[1]).toBe('-c')
      expect(cmd[2]).toContain('npm install')

      expect(hostConfig.Privileged).toBe(false) // CRITICAL SECURITY CHECK
      expect(hostConfig.Binds).toContain(`${mockContext.hostPath}:/app`)
      expect(hostConfig.AutoRemove).toBe(true)
    })

    test('should fail if compilation container exits with non-zero', async () => {
      // Mock failure for the first call (compile)
      mockDocker.run.mockResolvedValueOnce([{StatusCode: 1}])

      await expect(builder.build(mockContext, 'test-image')).rejects.toThrow('Compilation failed with exit code 1')
    })
  })

  describe('package', () => {
    beforeEach(() => {
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('package.json') || p.endsWith('Dockerfile.odac')) return
        throw new Error('ENOENT')
      })
    })

    test('should generate Dockerfile and run packager container', async () => {
      await builder.build(mockContext, 'test-image')

      // Verify Dockerfile generation
      expect(fsPromises.writeFile).toHaveBeenCalledWith(
        path.join(mockContext.internalPath, 'Dockerfile.odac'),
        expect.stringContaining('FROM node:lts-alpine')
      )

      // Verify second docker run call (the packager)
      // The first call is compile, second is package
      const [image, cmd, , createOptions] = mockDocker.run.mock.calls[1]
      const hostConfig = createOptions.HostConfig

      expect(image).toBe('docker:cli')
      expect(hostConfig.Binds).toContain('/var/run/docker.sock:/var/run/docker.sock')
      expect(hostConfig.Binds).toContain(`${mockContext.hostPath}:/app`)

      // Verify command uses correct paths
      expect(cmd).toEqual(['sh', '-c', `docker build --progress=plain -f /app/Dockerfile.odac -t test-image /app`])

      // Verify cleanup
      expect(fsPromises.unlink).toHaveBeenCalledWith(path.join(mockContext.internalPath, 'Dockerfile.odac'))
    })
  })

  describe('logging', () => {
    test('should use external logger phases if provided', async () => {
      const mockLogger = {
        stream: {write: jest.fn()},
        start: jest.fn(),
        end: jest.fn(),
        finalize: jest.fn()
      }

      const contextWithLogger = {...mockContext, appName: 'test-app', logger: mockLogger}

      // Setup detection (Node)
      fsPromises.access.mockImplementation(async p => {
        if (p.endsWith('package.json')) return
        throw new Error('ENOENT')
      })

      await builder.build(contextWithLogger, 'test-image')

      // Verify logger phases are tracked
      // Analysis Phase
      expect(mockLogger.start).toHaveBeenCalledWith('analysis')
      expect(mockLogger.end).toHaveBeenCalledWith('analysis', true)

      // Compile Phase
      expect(mockLogger.start).toHaveBeenCalledWith('pull_compiler')
      expect(mockLogger.end).toHaveBeenCalledWith('pull_compiler', true)
      expect(mockLogger.start).toHaveBeenCalledWith('run_compile')
      expect(mockLogger.end).toHaveBeenCalledWith('run_compile', true)

      // Package Phase
      expect(mockLogger.start).toHaveBeenCalledWith('prepare_context')
      expect(mockLogger.end).toHaveBeenCalledWith('prepare_context', true)
      expect(mockLogger.start).toHaveBeenCalledWith('pull_packager')
      expect(mockLogger.end).toHaveBeenCalledWith('pull_packager', true)
      expect(mockLogger.start).toHaveBeenCalledWith('run_package')
      expect(mockLogger.end).toHaveBeenCalledWith('run_package', true)
    })
  })
})
