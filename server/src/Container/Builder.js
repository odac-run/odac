const path = require('path')
const fs = require('fs')
const {PassThrough} = require('stream')

// Using Odac.core('Log') is standard practice in this codebase
const {log, error} = Odac.core('Log').init('Builder')

/**
 * ODAC Native Builder
 * Handles secure, 2-stage build process (Compile -> Package)
 * completely avoiding the need for DinD or Privileged mode.
 */
class Builder {
  /**
   * @type {import('dockerode')}
   */
  #docker

  constructor(docker) {
    this.#docker = docker
  }

  /**
   * Main entry point for building an image
   * @param {Object} context
   * @param {string} context.hostPath - Absolute path to source code on HOST
   * @param {string} context.internalPath - Absolute path to source code INSIDE ODAC
   * @param {string} imageName - Target image tag
   */
  async build(context, imageName) {
    const {hostPath, internalPath} = context
    log(`Starting build for ${imageName}`)
    log(`DEBUG: ODAC_HOST_ROOT=${process.env.ODAC_HOST_ROOT}`)
    log(`DEBUG: CWD=${process.cwd()}`)
    log(`Paths - Host: ${hostPath}, Internal: ${internalPath}`)

    // 1. Detect Strategy (Use internal path to read files)
    const strategy = await this.#detect(internalPath)
    if (!strategy) {
      throw new Error('Could not detect project type (no package.json found)')
    }
    log(`Detected project type: ${strategy.name}`)

    try {
      if (strategy.type === 'custom') {
        // FAST TRACK: Custom Dockerfile
        // Skip compile phase, go straight to package mechanism but using existing Dockerfile
        await this.#packageCustom(context, imageName)
      } else {
        // STANDARD TRACK: Auto-Build
        // 2. Compile (Artifact Builder)
        await this.#compile(strategy, context)

        // 3. Package (Image Packager)
        await this.#package(strategy, context, imageName)
      }

      log(`Build completed successfully: ${imageName}`)
      return true
    } catch (err) {
      error(`Build failed: ${err.message}`)
      throw err
    }
  }

  /**
   * Special Package Phase for Custom Dockerfile
   * Directly builds the provided Dockerfile on host context
   */
  async #packageCustom(context, imageName) {
    log(`[Builder] Building from Custom Dockerfile for ${imageName}...`)

    // Use docker:cli to forward build command to host
    const packagerImage = 'docker:cli'
    // Build context is mapped to /app. Dockerfile is at /app/Dockerfile
    const buildCmd = `docker build -t ${imageName} /app`

    try {
      await this.#ensureImage(packagerImage)

      const logStream = new PassThrough()
      logStream.on('data', chunk => log(chunk.toString().trim()))

      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        Binds: ['/var/run/docker.sock:/var/run/docker.sock', `${context.hostPath}:/app`],
        AutoRemove: true,
        Privileged: false
      })

      if (data && data.StatusCode !== 0) {
        throw new Error(`Custom build failed with exit code ${data.StatusCode}`)
      }
      log('[Builder] Custom build successful.')
    } catch (err) {
      error(`Custom build failed: ${err.message}`)
      throw err
    }
  }

  /**
   * Detects project language/framework
   * @param {string} internalPath
   * @returns {Promise<Object>} Strategy object
   */
  async #detect(internalPath) {
    // 0. CUSTOM DOCKERFILE Strategy (Joker - Highest Priority)
    if (fs.existsSync(path.join(internalPath, 'Dockerfile'))) {
      return {
        name: 'Custom Dockerfile',
        type: 'custom', // Special type flag
        // No compile/install needed, direct build
        image: null,
        installCmd: null,
        buildCmd: null,
        cleanupCmd: null,
        package: null // Will bypass standard package logic
      }
    }

    // 1. PYTHON Strategy
    if (fs.existsSync(path.join(internalPath, 'requirements.txt')) || fs.existsSync(path.join(internalPath, 'pyproject.toml'))) {
      return {
        name: 'Python',
        image: 'python:3.11-slim',
        installCmd: 'pip install --no-cache-dir -r requirements.txt --target /app/deps || exit 0', // Install to local dir for extraction
        buildCmd: 'rm -rf __pycache__',
        cleanupCmd: 'rm -rf .git',
        package: {
          baseImage: 'python:3.11-slim',
          user: 'nobody',
          // Env to find deps installed in phase 1
          cmd: ['python', 'app.py'],
          env: {PYTHONPATH: '/app/deps'}
        }
      }
    }

    // 2. GO Strategy (Static Binary)
    if (fs.existsSync(path.join(internalPath, 'go.mod'))) {
      return {
        name: 'Go',
        image: 'golang:1.22-alpine',
        installCmd: 'go mod download',
        buildCmd: 'go build -o app .',
        cleanupCmd: 'rm -rf .git',
        package: {
          baseImage: 'alpine:latest', // Tiny runtime
          user: 'nobody',
          cmd: ['/app/app']
        }
      }
    }

    // 3. NODE.JS Strategy
    if (fs.existsSync(path.join(internalPath, 'package.json'))) {
      return {
        name: 'Node.js',
        image: 'node:lts-alpine',
        installCmd:
          'if [ -f package-lock.json ]; then npm ci --omit=dev --no-audit --no-fund; else npm install --omit=dev --no-audit --no-fund; fi',
        buildCmd: 'if [ -f "tsconfig.json" ] || grep -q "build" package.json; then npm run build --if-present; fi',
        cleanupCmd: 'rm -rf .git .github test tests',
        package: {
          baseImage: 'node:lts-alpine',
          user: 'node',
          cmd: ['npm', 'start']
        }
      }
    }

    // 4. PHP Strategy
    if (fs.existsSync(path.join(internalPath, 'composer.json')) || fs.existsSync(path.join(internalPath, 'index.php'))) {
      return {
        name: 'PHP',
        image: 'composer:lts',
        installCmd: 'if [ -f composer.json ]; then composer install --no-dev --ignore-platform-reqs; fi',
        buildCmd: 'true',
        cleanupCmd: 'rm -rf .git',
        package: {
          baseImage: 'php:8.2-apache',
          user: 'www-data',
          cmd: ['apache2-foreground'] // Standard Apache endpoint
        }
      }
    }

    // 4. STATIC WEB Strategy (Fallback if index.html exists but no specific backend match)
    // Note: React/Vue apps often have package.json, so they hit Node strategy.
    // If the intent is to serve static build output, the user might need to specify 'type: static' explicitly.
    // For now, if ONLY index.html exists:
    if (fs.existsSync(path.join(internalPath, 'index.html'))) {
      return {
        name: 'Static Web',
        image: 'alpine:latest', // No compile needed for pure static, or Node if build needed logic added later
        installCmd: 'true',
        buildCmd: 'true',
        cleanupCmd: 'rm -rf .git',
        package: {
          baseImage: 'nginx:alpine',
          user: 'nginx',
          cmd: ['nginx', '-g', 'daemon off;']
        }
      }
    }

    return null
  }

  /**
   * Stage 1: Compile
   * Runs a temporary, unprivileged container to compile assets in-place.
   */
  async #compile(strategy, context) {
    log(`[Phase 1] Compiling artifacts using ${strategy.image}...`)

    const commands = [strategy.installCmd, strategy.buildCmd, strategy.cleanupCmd].filter(Boolean).join(' && ')

    const containerConfig = {
      Image: strategy.image,
      Cmd: ['sh', '-c', commands],
      WorkingDir: '/app',
      HostConfig: {
        Binds: [`${context.hostPath}:/app`], // Mount HOST path
        AutoRemove: true,
        Privileged: false, // SECURITY: Strict NO to privileged
        NetworkMode: 'host' // Use host network for speed and simplicity in caching
      }
    }

    try {
      await this.#ensureImage(strategy.image)

      // Create a safe stream that won't close process.stdout
      const logStream = new PassThrough()
      logStream.on('data', chunk => log(chunk.toString().trim()))

      // Pass full containerConfig as createOptions (4th param)
      // docker.run signature: run(image, cmd, outputStream, createOptions, startOptions)
      const [data] = await this.#docker.run(strategy.image, ['sh', '-c', commands], logStream, {
        WorkingDir: '/app',
        HostConfig: containerConfig.HostConfig
      })

      if (data && data.StatusCode !== 0) {
        throw new Error(`Compilation failed with exit code ${data.StatusCode}`)
      }
      log('[Phase 1] Compilation successful.')
    } catch (err) {
      error(`Compilation failed: ${err.message}`)
      throw err
    }
  }

  /**
   * Stage 2: Package
   * Builds the final image using the compiled artifacts.
   */
  async #package(strategy, context, imageName) {
    log(`[Phase 2] Packaging final image ${imageName}...`)

    // 1. Generate ephemeral Dockerfile in the source directory
    let dockerfileContent = `
FROM ${strategy.package.baseImage}
WORKDIR /app
COPY . .
USER root
RUN chown -R ${strategy.package.user}:${strategy.package.user} /app
USER ${strategy.package.user}
`
    // Add Environment Variables if defined
    if (strategy.package.env) {
      for (const [key, val] of Object.entries(strategy.package.env)) {
        dockerfileContent += `ENV ${key}="${val}"\n`
      }
    }

    dockerfileContent += `CMD ${JSON.stringify(strategy.package.cmd)}\n`

    const dockerfilePath = path.join(context.internalPath, 'Dockerfile.odac')
    fs.writeFileSync(dockerfilePath, dockerfileContent)

    // 2. Use a specialized "Docker Client" container to perform the build on HOST
    // using the socket. This avoids DinD (we just talk to the socket).
    const packagerImage = 'docker:cli'

    // Commands to run inside the packager container
    // It basically tells the HOST docker daemon to build the context at /app (which is context.hostPath)
    const buildCmd = `docker build -f /app/Dockerfile.odac -t ${imageName} /app`

    try {
      await this.#ensureImage(packagerImage)

      const packagerConfig = {
        Image: packagerImage,
        Cmd: ['sh', '-c', buildCmd],
        HostConfig: {
          Binds: [
            '/var/run/docker.sock:/var/run/docker.sock', // Access Host Docker
            `${context.hostPath}:/app` // Access Source Context
          ],
          AutoRemove: true,
          Privileged: false // Not needed, just socket access
        }
      }

      const logStream = new PassThrough()
      logStream.on('data', chunk => log(chunk.toString().trim()))

      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        HostConfig: packagerConfig.HostConfig
      })

      if (data && data.StatusCode !== 0) {
        throw new Error(`Packaging failed with exit code ${data.StatusCode}`)
      }
      log('[Phase 2] Packaging successful.')
    } catch (err) {
      error(`Packaging failed: ${err.message}`)
      throw err
    } finally {
      // Cleanup ephemeral Dockerfile
      if (fs.existsSync(dockerfilePath)) {
        fs.unlinkSync(dockerfilePath)
      }
    }
  }

  /**
   * Helper to ensure image exists
   */
  async #ensureImage(imageName) {
    try {
      const image = this.#docker.getImage(imageName)
      const info = await image.inspect().catch(() => null)
      if (!info) {
        log(`Pulling image ${imageName}...`)
        await new Promise((resolve, reject) => {
          this.#docker.pull(imageName, (err, stream) => {
            if (err) return reject(err)
            this.#docker.modem.followProgress(stream, (err, res) => (err ? reject(err) : resolve(res)))
          })
        })
      }
    } catch (e) {
      error(`Failed to pull ${imageName}: ${e.message}`)
    }
  }
}

module.exports = Builder
