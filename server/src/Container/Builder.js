const path = require('path')
const fs = require('fs/promises')
const {constants: fsConstants} = require('fs')
const {PassThrough} = require('stream')

// Using Odac.core('Log') is standard practice in this codebase
const {log, error} = Odac.core('Log').init('Container', 'Builder')
const Logger = require('./Logger')

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
    cleanupCmd: null,
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
    cleanupCmd: null,
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
    cleanupCmd: 'rm -rf test tests',
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
    cleanupCmd: null,
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
    cleanupCmd: null,
    package: {
      baseImage: 'nginx:alpine',
      user: 'nginx',
      cmd: ['nginx', '-g', 'daemon off;'],
      setup: [
        // Fix permissions for non-root execution
        'chown -R nginx:nginx /var/cache/nginx /var/log/nginx /etc/nginx/conf.d',
        'touch /var/run/nginx.pid && chown nginx:nginx /var/run/nginx.pid',
        // Remove "user" directive from default config as we are already running as that user
        'sed -i "/user  nginx;/d" /etc/nginx/nginx.conf',
        // Configure Nginx to serve from /app (avoid copying files)
        'sed -i "s|root   /usr/share/nginx/html;|root   /app;|g" /etc/nginx/conf.d/default.conf'
      ]
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
    const {internalPath} = context

    // Initialize Logger if appName is provided AND no external logger passed
    let buildLogger = null

    if (context.appName && !context.logger) {
      try {
        buildLogger = new Logger(context.appName)
        await buildLogger.init()

        const buildId = `build_${Date.now()}`
        const logCtrl = buildLogger.createBuildStream(buildId, {
          image: imageName,
          strategy: 'detecting...'
        })

        // Wrap logger control to be passed to phases
        context.logger = {
          stream: logCtrl.stream,
          start: logCtrl.startPhase,
          end: logCtrl.endPhase,
          finalize: logCtrl.finalize
        }
      } catch (e) {
        error('Failed to initialize build logger: %s', e.message)
      }
    }

    // 1. Detect Strategy (Use internal path to read files)
    if (context.logger) context.logger.start('analysis')
    const strategy = await this.#detect(internalPath)
    if (context.logger) {
      context.logger.end('analysis', true)
      if (strategy) {
        context.logger.stream.write(`[Builder] Detected project type: ${strategy.name}\n`)
      }
    }

    if (!strategy) {
      throw new Error('Could not detect project type (no package.json, requirements.txt, etc. found)')
    }

    try {
      if (strategy.type === 'custom') {
        // FAST TRACK: Custom Dockerfile
        if (context.logger) context.logger.start('custom')
        await this.#packageCustom(context, imageName)
        if (context.logger) context.logger.end('custom', true)
      } else {
        // STANDARD TRACK: Auto-Build
        // 2. Compile (Artifact Builder)
        if (context.logger) context.logger.start('compile')
        await this.#compile(strategy, context)
        if (context.logger) context.logger.end('compile', true)

        // 3. Package (Image Packager)
        if (context.logger) context.logger.start('package')
        await this.#package(strategy, context, imageName)
        if (context.logger) context.logger.end('package', true)
      }

      log(`Build completed successfully: ${imageName}`)
      if (context.logger) await context.logger.finalize(true)
      return true
    } catch (err) {
      error(`Build failed: ${err.message}`)
      if (context.logger) await context.logger.finalize(false)
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
      if (context.logger) context.logger.start('pull_builder')
      await this.#ensureImage(packagerImage)
      if (context.logger) context.logger.end('pull_builder', true)

      const logStream = new PassThrough()
      const chunks = []
      logStream.on('data', chunk => {
        const str = chunk.toString()
        chunks.push(str)
        if (context.logger) context.logger.stream.write(str)
      })

      if (context.logger) context.logger.start('run_custom_build')
      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        Binds: ['/var/run/docker.sock:/var/run/docker.sock', `${context.hostPath}:/app`],
        AutoRemove: true,
        Privileged: false
      })
      if (context.logger) context.logger.end('run_custom_build', true)

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
      if (context.logger) context.logger.start('pull_compiler')
      await this.#ensureImage(strategy.image)
      if (context.logger) context.logger.end('pull_compiler', true)

      const logStream = new PassThrough()
      const chunks = []
      logStream.on('data', chunk => {
        const str = chunk.toString()
        chunks.push(str)
        if (context.logger) context.logger.stream.write(str)
      })

      if (context.logger) context.logger.start('run_compile')
      const [data] = await this.#docker.run(strategy.image, ['sh', '-c', commands], logStream, {
        WorkingDir: '/app',
        HostConfig: containerConfig.HostConfig
      })
      if (context.logger) context.logger.end('run_compile', true)

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
`

    // Apply Strategy-specific Setup Commands (if any)
    if (strategy.package.setup && Array.isArray(strategy.package.setup)) {
      for (const cmd of strategy.package.setup) {
        dockerfileContent += `RUN ${cmd}\n`
      }
    }

    dockerfileContent += `USER ${strategy.package.user}\n`
    // Add Environment Variables if defined
    if (strategy.package.env) {
      for (const [key, val] of Object.entries(strategy.package.env)) {
        dockerfileContent += `ENV ${key}="${val}"\n`
      }
    }

    dockerfileContent += `CMD ${JSON.stringify(strategy.package.cmd)}\n`

    const dockerfilePath = path.join(context.internalPath, 'Dockerfile.odac')
    const dockerignorePath = path.join(context.internalPath, '.dockerignore')

    if (context.logger) context.logger.start('prepare_context')
    await Promise.all([fs.writeFile(dockerfilePath, dockerfileContent), fs.writeFile(dockerignorePath, '.git\n.github\nDockerfile.odac\n')])
    if (context.logger) context.logger.end('prepare_context', true)

    // 2. Use a specialized "Docker Client" container to perform the build on HOST
    // using the socket. This avoids DinD (we just talk to the socket).
    const packagerImage = 'docker:cli'

    // Commands to run inside the packager container
    const buildCmd = `docker build --progress=plain -f /app/Dockerfile.odac -t ${imageName} /app`

    try {
      if (context.logger) context.logger.start('pull_packager')
      await this.#ensureImage(packagerImage)
      if (context.logger) context.logger.end('pull_packager', true)

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
      logStream.on('data', chunk => {
        const str = chunk.toString()
        chunks.push(str)
        if (context.logger) context.logger.stream.write(str)
      })

      if (context.logger) context.logger.start('run_package')
      const [data] = await this.#docker.run(packagerImage, ['sh', '-c', buildCmd], logStream, {
        HostConfig: packagerConfig.HostConfig
      })
      if (context.logger) context.logger.end('run_package', true)

      if (data && data.StatusCode !== 0) {
        error(`Packaging logs:\n${chunks.join('')}`)
        throw new Error(`Packaging failed with exit code ${data.StatusCode}`)
      }
      log('[Phase 2] Packaging successful.')
    } catch (err) {
      error(`Packaging failed: ${err.message}`)
      throw err
    } finally {
      // Cleanup ephemeral build files
      await Promise.all([fs.unlink(dockerfilePath).catch(() => {}), fs.unlink(dockerignorePath).catch(() => {})])
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
