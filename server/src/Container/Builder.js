const path = require('path')
const fs = require('fs/promises')
const {constants: fsConstants} = require('fs')
const {PassThrough} = require('stream')

// Using Odac.core('Log') is standard practice in this codebase
const {log, error} = Odac.core('Log').init('Container', 'Builder')

/**
 * Configuration for Build Strategies.
 * Centralized to avoid magic strings and allow easy updates.
 */
const BUILD_STRATEGIES = {
  PYTHON: {
    name: 'Python',
    triggers: ['requirements.txt', 'pyproject.toml'],
    image: 'python:3.11-slim',
    installCmd: '[ ! -f requirements.txt ] || pip install --no-cache-dir -r requirements.txt --target /app/deps',
    buildCmd: 'rm -rf __pycache__',
    cleanupCmd: 'rm -rf .git',
    package: {
      baseImage: 'python:3.11-slim',
      user: 'nobody',
      cmd: ['python', 'app.py'],
      env: {PYTHONPATH: '/app/deps'}
    }
  },
  GO: {
    name: 'Go',
    triggers: ['go.mod'],
    image: 'golang:1.22-alpine',
    installCmd: 'go mod download',
    buildCmd: 'go build -o app .',
    cleanupCmd: 'rm -rf .git',
    package: {
      baseImage: 'alpine:latest',
      user: 'nobody',
      cmd: ['/app/app']
    }
  },
  NODE: {
    name: 'Node.js',
    triggers: ['package.json'],
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
  },
  PHP: {
    name: 'PHP',
    triggers: ['composer.json', 'index.php'],
    image: 'composer:lts',
    installCmd: 'if [ -f composer.json ]; then composer install --no-dev --ignore-platform-reqs; fi',
    buildCmd: 'true',
    cleanupCmd: 'rm -rf .git',
    package: {
      baseImage: 'php:8.2-apache',
      user: 'www-data',
      cmd: ['apache2-foreground']
    }
  },
  STATIC: {
    name: 'Static Web',
    triggers: ['index.html'],
    image: 'alpine:latest',
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

/**
 * ODAC Native Builder
 * Handles secure, 2-stage build process (Compile -> Package)
 * completely avoiding the need for DinD or Privileged mode.
 *
 * Performance Note:
 * - Uses Host Bind Mounts for zero-copy I/O during builds.
 * - Uses Socket Mounting for "Docker-out-of-Docker" (DooD) to avoid DinD overhead.
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
   * @returns {Promise<boolean>}
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
      throw new Error('Could not detect project type (no package.json, requirements.txt, etc. found)')
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
      // Preserve stack trace by re-throwing the original error object
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
    const buildCmd = `docker build --progress=plain -t ${imageName} /app`

    try {
      await this.#ensureImage(packagerImage)

      const logStream = new PassThrough()
      const chunks = []
      logStream.on('data', chunk => chunks.push(chunk.toString()))

      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        Binds: ['/var/run/docker.sock:/var/run/docker.sock', `${context.hostPath}:/app`],
        AutoRemove: true,
        Privileged: false
      })

      if (data && data.StatusCode !== 0) {
        error(`Custom build logs:\n${chunks.join('')}`)
        throw new Error(`Custom build failed with exit code ${data.StatusCode}`)
      }
      log('[Builder] Custom build successful.')
    } catch (err) {
      error(`Custom build failed: ${err.message}`)
      throw err
    }
  }

  /**
   * Detects project language/framework using non-blocking I/O
   * @param {string} internalPath
   * @returns {Promise<Object>} Strategy object
   */
  async #detect(internalPath) {
    // 0. CUSTOM DOCKERFILE Strategy (Highest Priority)
    if (await this.#exists(path.join(internalPath, 'Dockerfile'))) {
      return {
        name: 'Custom Dockerfile',
        type: 'custom',
        image: null,
        installCmd: null,
        buildCmd: null,
        cleanupCmd: null,
        package: null
      }
    }

    // Check all strategies defined in order (Priority implicitly defined by insertion order in constant if iterated)
    // But we manually order them for safety: Python, Go, Node, PHP, Static

    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.PYTHON)) return BUILD_STRATEGIES.PYTHON
    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.GO)) return BUILD_STRATEGIES.GO
    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.NODE)) return BUILD_STRATEGIES.NODE
    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.PHP)) return BUILD_STRATEGIES.PHP
    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.STATIC)) return BUILD_STRATEGIES.STATIC

    return null
  }

  /**
   * Helper to check if any trigger file exists for a strategy
   * @param {string} internalPath
   * @param {Object} strategy
   */
  async #checkStrategyTriggers(internalPath, strategy) {
    for (const trigger of strategy.triggers) {
      if (await this.#exists(path.join(internalPath, trigger))) {
        return true
      }
    }
    return false
  }

  // Non-blocking file existence check
  async #exists(filePath) {
    try {
      await fs.access(filePath, fsConstants.F_OK)
      return true
    } catch {
      return false
    }
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
      Env: ['CI=true', 'NPM_CONFIG_SPIN=false', 'NPM_CONFIG_PROGRESS=false'],
      HostConfig: {
        Binds: [`${context.hostPath}:/app`], // Mount HOST path
        AutoRemove: true,
        Privileged: false, // SECURITY: Strict NO to privileged
        NetworkMode: 'host' // Use host network for speed and simplicity in caching
      }
    }

    try {
      await this.#ensureImage(strategy.image)

      const logStream = new PassThrough()
      const chunks = []
      logStream.on('data', chunk => chunks.push(chunk.toString()))

      const [data] = await this.#docker.run(strategy.image, ['sh', '-c', commands], logStream, {
        WorkingDir: '/app',
        HostConfig: containerConfig.HostConfig
      })

      if (data && data.StatusCode !== 0) {
        error(`Compilation logs:\n${chunks.join('')}`)
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

    // Async file write
    await fs.writeFile(dockerfilePath, dockerfileContent)

    // 2. Use a specialized "Docker Client" container to perform the build on HOST
    // using the socket. This avoids DinD (we just talk to the socket).
    const packagerImage = 'docker:cli'

    // Commands to run inside the packager container
    const buildCmd = `docker build --progress=plain -f /app/Dockerfile.odac -t ${imageName} /app`

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
      const chunks = []
      logStream.on('data', chunk => chunks.push(chunk.toString()))

      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        HostConfig: packagerConfig.HostConfig
      })

      if (data && data.StatusCode !== 0) {
        error(`Packaging logs:\n${chunks.join('')}`)
        throw new Error(`Packaging failed with exit code ${data.StatusCode}`)
      }
      log('[Phase 2] Packaging successful.')
    } catch (err) {
      error(`Packaging failed: ${err.message}`)
      throw err
    } finally {
      // Cleanup ephemeral Dockerfile
      try {
        await fs.unlink(dockerfilePath)
      } catch {
        // Ignore unlink errors (already deleted or access denied)
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
      throw e // Re-throw critical image pull errors
    }
  }
}

module.exports = Builder
