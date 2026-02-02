const Builder = require('../../server/src/Container/Builder')
const fs = require('fs')
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
jest.mock('fs')
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
      run: jest.fn().mockResolvedValue(mockStream),
      pull: jest.fn((img, cb) => cb(null, {})),
      getImage: jest.fn().mockReturnValue({
        inspect: jest.fn().mockResolvedValue({Id: 'sha256:...'})
      }),
      modem: {
        followProgress: jest.fn((stream, cb) => cb(null, []))
      }
    }

    builder = new Builder(mockDocker)
  })

  describe('detect', () => {
    test('should detect Node.js project if package.json exists', async () => {
      fs.existsSync.mockReturnValue(true) // package.json exists

      // Test via private method exposed functionality (we can't call private directly easily in JS without hacks)
      // Actually, we test via build() public interface

      // We assume compile/package will be called if detect succeeds
      // But to verify detection specifically, we can check if it proceeds to compile steps with correct strategy

      // Let's spy on docker.run

      await builder.build(mockContext, 'test-image')

      // Verify fs.existsSync was called with internal path
      expect(fs.existsSync).toHaveBeenCalledWith(path.join(mockContext.internalPath, 'package.json'))

      // Verify docker run was called with Node image (result of detection)
      expect(mockDocker.run).toHaveBeenCalledWith('node:lts-alpine', expect.any(Array), expect.any(Array), expect.any(Object))
    })

    test('should throw if no project type detected', async () => {
      fs.existsSync.mockReturnValue(false) // No package.json

      await expect(builder.build(mockContext, 'test-image')).rejects.toThrow('Could not detect project type')
    })
  })

  describe('compile', () => {
    beforeEach(() => {
      fs.existsSync.mockReturnValue(true) // Bypass detection
    })

    test('should run compiler container with secure configuration', async () => {
      await builder.build(mockContext, 'test-image')

      // Check compilation step arguments
      const [image, cmd, streams, hostConfig] = mockDocker.run.mock.calls[0]

      expect(image).toBe('node:lts-alpine')
      expect(hostConfig.Privileged).toBe(false) // CRITICAL SECURITY CHECK
      expect(hostConfig.Binds).toContain(`${mockContext.hostPath}:/app`)
      expect(hostConfig.AutoRemove).toBe(true)
    })

    test('should fail if compilation container exits with non-zero', async () => {
      // Mock failure for the first call (compile)
      mockDocker.run.mockResolvedValueOnce({StatusCode: 1})

      await expect(builder.build(mockContext, 'test-image')).rejects.toThrow('Compilation failed with exit code 1')
    })
  })

  describe('package', () => {
    beforeEach(() => {
      fs.existsSync.mockReturnValue(true) // Bypass detection
    })

    test('should generate Dockerfile and run packager container', async () => {
      await builder.build(mockContext, 'test-image')

      // Verify Dockerfile generation
      expect(fs.writeFileSync).toHaveBeenCalledWith(
        path.join(mockContext.internalPath, 'Dockerfile.odac'),
        expect.stringContaining('FROM node:lts-alpine')
      )

      // Verify second docker run call (the packager)
      // The first call is compile, second is package
      const [image, cmd, streams, hostConfig] = mockDocker.run.mock.calls[1]

      expect(image).toBe('docker:cli')
      expect(hostConfig.Binds).toContain('/var/run/docker.sock:/var/run/docker.sock')
      expect(hostConfig.Binds).toContain(`${mockContext.hostPath}:/app`)

      // Verify command uses correct paths
      expect(cmd).toEqual(['sh', '-c', `docker build -f /app/Dockerfile.odac -t test-image /app`])

      // Verify cleanup
      expect(fs.unlinkSync).toHaveBeenCalledWith(path.join(mockContext.internalPath, 'Dockerfile.odac'))
    })
  })
})
