const path = require('path')
const fs = require('fs/promises')
const {constants: fsConstants} = require('fs')
const {PassThrough} = require('stream')

// Using Odac.core('Log') is standard practice in this codebase
const {log, error} = Odac.core('Log').init('Container', 'Builder')
const Logger = require('./Logger')

/**
 * Parses a semver-like major.minor version from raw content.
 * Returns only the major.minor portion (e.g. "1.22" from "1.22.5").
 * @param {string} raw - Raw version string (e.g. "1.22.5", "^3.11", ">=20.0")
 * @returns {string|null} Major.minor version or null if unparseable
 */
function parseMajorMinor(raw) {
  const match = raw.match(/(\d+\.\d+)/)
  return match ? match[1] : null
}

/**
 * Version resolver definitions for each language ecosystem.
 * Each resolver specifies which file to read and how to extract the version.
 * Resolvers return a Docker image tag suffix (e.g. "1.22-alpine") or null for default.
 */
const VERSION_RESOLVERS = {
  /**
   * Reads `go.mod` directive: `go 1.22` or `go 1.22.5`
   * Produces tag suffix: `1.22-alpine`
   */
  GO: {
    file: 'go.mod',
    parse(content) {
      const match = content.match(/^go\s+(\d+\.\d+)/m)
      return match ? `${match[1]}-alpine` : null
    }
  },
  /**
   * Reads `package.json` engines.node field: `">=18"`, `"^20.0"`, `"20.11.0"`
   * Produces tag suffix: `18-alpine`, `20-alpine` (major only for Node LTS alignment)
   */
  NODE: {
    file: 'package.json',
    parse(content) {
      try {
        const pkg = JSON.parse(content)
        const constraint = pkg.engines && pkg.engines.node
        if (!constraint) return null
        const match = constraint.match(/(\d+)/)
        return match ? `${match[1]}-alpine` : null
      } catch {
        return null
      }
    }
  },
  /**
   * Reads `composer.json` require.php field: `">=8.1"`, `"^8.2"`, `"8.3.*"`
   * Produces tag suffix for build image: `lts` (composer), runtime: `8.2-apache`
   */
  PHP: {
    file: 'composer.json',
    parse(content) {
      try {
        const composer = JSON.parse(content)
        const constraint = composer.require && composer.require.php
        if (!constraint) return null
        const version = parseMajorMinor(constraint)
        return version || null
      } catch {
        return null
      }
    }
  },
  /**
   * Reads `pyproject.toml` requires-python or `runtime.txt` for Python version.
   * Falls back to `runtime.txt` (Heroku-style: `python-3.12.1`).
   * Produces tag suffix: `3.12-slim`
   */
  PYTHON: {
    file: 'pyproject.toml',
    fallbackFile: 'runtime.txt',
    parse(content, filename) {
      if (filename === 'runtime.txt') {
        const match = content.match(/python-(\d+\.\d+)/)
        return match ? `${match[1]}-slim` : null
      }
      const match = content.match(/requires-python\s*=\s*["']([^"']+)["']/)
      if (!match) return null
      const version = parseMajorMinor(match[1])
      return version ? `${version}-slim` : null
    }
  },
  /**
   * Reads `rust-toolchain.toml` or `rust-toolchain` for Rust channel.
   * Produces tag suffix: `1.78-alpine`
   */
  RUST: {
    file: 'rust-toolchain.toml',
    fallbackFile: 'rust-toolchain',
    parse(content, filename) {
      if (filename === 'rust-toolchain') {
        const version = parseMajorMinor(content.trim())
        return version ? `${version}-alpine` : null
      }
      const match = content.match(/channel\s*=\s*["']([^"']+)["']/)
      if (!match) return null
      const version = parseMajorMinor(match[1])
      return version ? `${version}-alpine` : null
    }
  }
}

/**
 * Configuration for Build Strategies.
 * Centralized to avoid magic strings and allow easy updates.
 *
 * `imageBase` defines the registry/repo prefix (e.g. "golang").
 * `imageDefault` is the fallback tag when version auto-detection fails.
 * `versionResolver` references a key in VERSION_RESOLVERS for dynamic tag resolution.
 */
const BUILD_STRATEGIES = {
  BUN: {
    name: 'Bun',
    triggers: ['bun.lock', 'bun.lockb'],
    imageBase: 'oven/bun',
    imageDefault: 'alpine',
    installCmd: 'bun install --frozen-lockfile',
    buildCmd: 'bun run build --if-present',
    cleanupCmd: 'bun install --production && rm -rf test tests',
    package: {
      baseImage: 'oven/bun:alpine',
      user: 'bun',
      cmd: ['bun', 'run', 'start']
    }
  },
  GO: {
    name: 'Go',
    triggers: ['go.mod'],
    imageBase: 'golang',
    imageDefault: 'alpine',
    versionResolver: 'GO',
    installCmd: 'go mod download',
    buildCmd:
      'PKG=$(go list -f "{{.Name}} {{.ImportPath}}" ./... | grep "^main " | head -n 1 | cut -d" " -f2); if [ -z "$PKG" ]; then PKG="."; fi; go build -o app $PKG',
    cleanupCmd: null,
    package: {
      baseImage: 'alpine:latest',
      user: 'nobody',
      cmd: ['/app/app']
    }
  },
  NODE_NPM: {
    name: 'Node.js (npm)',
    triggers: ['package-lock.json'],
    imageBase: 'node',
    imageDefault: 'lts-alpine',
    versionResolver: 'NODE',
    installCmd: 'if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; else npm install --no-audit --no-fund; fi',
    buildCmd: 'npm run build --if-present',
    cleanupCmd: 'npm prune --production && rm -rf test tests',
    package: {
      baseImage: 'node:lts-alpine',
      user: 'node',
      cmd: ['npm', 'start']
    }
  },
  NODE_PNPM: {
    name: 'Node.js (pnpm)',
    triggers: ['pnpm-lock.yaml'],
    imageBase: 'node',
    imageDefault: 'lts-alpine',
    versionResolver: 'NODE',
    installCmd: 'corepack enable && corepack prepare pnpm@latest --activate && pnpm install --frozen-lockfile',
    buildCmd: 'pnpm run build --if-present',
    cleanupCmd: 'pnpm prune --prod && rm -rf test tests',
    package: {
      baseImage: 'node:lts-alpine',
      user: 'node',
      cmd: ['npm', 'start']
    }
  },
  NODE_YARN: {
    name: 'Node.js (yarn)',
    triggers: ['yarn.lock'],
    imageBase: 'node',
    imageDefault: 'lts-alpine',
    versionResolver: 'NODE',
    installCmd: 'corepack enable && yarn install --frozen-lockfile',
    buildCmd: 'yarn run build --if-present',
    cleanupCmd: 'yarn install --production --frozen-lockfile && rm -rf test tests',
    package: {
      baseImage: 'node:lts-alpine',
      user: 'node',
      cmd: ['npm', 'start']
    }
  },
  PHP: {
    name: 'PHP',
    triggers: ['composer.json', 'index.php'],
    imageBase: 'composer',
    imageDefault: 'lts',
    versionResolver: 'PHP',
    installCmd: 'if [ -f composer.json ]; then composer install --no-dev --ignore-platform-reqs; fi',
    buildCmd: 'true',
    cleanupCmd: null,
    package: {
      baseImage: 'php:8.2-apache',
      user: 'www-data',
      cmd: ['apache2-foreground'],
      setup: [
        // Change DocumentRoot to /app
        'sed -ri -e "s!/var/www/html!/app!g" /etc/apache2/sites-available/*.conf',
        'sed -ri -e "s!/var/www/!/app!g" /etc/apache2/apache2.conf /etc/apache2/conf-available/*.conf',
        // Fix permissions for non-root execution
        'chown -R www-data:www-data /var/run/apache2 /var/log/apache2 /var/lock/apache2 /var/lib/apache2'
      ]
    }
  },
  PYTHON: {
    name: 'Python',
    triggers: ['requirements.txt', 'pyproject.toml'],
    imageBase: 'python',
    imageDefault: '3-slim',
    versionResolver: 'PYTHON',
    installCmd: '[ ! -f requirements.txt ] || pip install --no-cache-dir -r requirements.txt --target /app/deps',
    buildCmd: 'rm -rf __pycache__',
    cleanupCmd: null,
    package: {
      baseImage: 'python:3-slim',
      user: 'nobody',
      cmd: ['sh', '-c', 'if [ -f main.py ]; then python main.py; elif [ -f run.py ]; then python run.py; else python app.py; fi'],
      env: {PYTHONPATH: '/app/deps'}
    }
  },
  RUST: {
    name: 'Rust',
    triggers: ['Cargo.toml', 'Cargo.lock'],
    imageBase: 'rust',
    imageDefault: 'alpine',
    versionResolver: 'RUST',
    installCmd: 'apk add --no-cache musl-dev',
    buildCmd:
      'cargo build --release && find target/release -maxdepth 1 -type f -executable -not -name "*.*" | head -n 1 | xargs -I {} cp {} /app/app',
    cleanupCmd: 'rm -rf target src',
    package: {
      baseImage: 'alpine:latest',
      user: 'nobody',
      cmd: ['/app/app']
    }
  },
  STATIC: {
    name: 'Static Web',
    triggers: ['index.html'],
    imageBase: 'alpine',
    imageDefault: 'latest',
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
   * and resolves the optimal compiler image version from project files.
   * @param {string} internalPath
   * @returns {Promise<Object>} Strategy object with resolved `image` property
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

    let strategy = null

    // Check all strategies in priority order
    if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.PYTHON)) strategy = BUILD_STRATEGIES.PYTHON
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.GO)) strategy = BUILD_STRATEGIES.GO
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.RUST)) strategy = BUILD_STRATEGIES.RUST
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.BUN)) strategy = BUILD_STRATEGIES.BUN
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.NODE_PNPM)) strategy = BUILD_STRATEGIES.NODE_PNPM
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.NODE_YARN)) strategy = BUILD_STRATEGIES.NODE_YARN
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.NODE_NPM)) strategy = BUILD_STRATEGIES.NODE_NPM
    else if (await this.#exists(path.join(internalPath, 'package.json'))) strategy = BUILD_STRATEGIES.NODE_NPM
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.PHP)) strategy = BUILD_STRATEGIES.PHP
    else if (await this.#checkStrategyTriggers(internalPath, BUILD_STRATEGIES.STATIC)) strategy = BUILD_STRATEGIES.STATIC

    if (!strategy) return null

    // Resolve the optimal image tag from project version files
    const image = await this.#resolveImage(internalPath, strategy)
    return {...strategy, image}
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
   * Resolves the optimal Docker image tag by reading project version files.
   * Falls back to the strategy default when version cannot be determined.
   * @param {string} internalPath - Project root inside ODAC container
   * @param {Object} strategy - Build strategy with imageBase, imageDefault, versionResolver
   * @returns {Promise<string>} Full image reference (e.g. "golang:1.22-alpine")
   */
  async #resolveImage(internalPath, strategy) {
    const fallback = `${strategy.imageBase}:${strategy.imageDefault}`

    if (!strategy.versionResolver) return fallback

    const resolver = VERSION_RESOLVERS[strategy.versionResolver]
    if (!resolver) return fallback

    // Try primary version file, then fallback file
    const files = [resolver.file, resolver.fallbackFile].filter(Boolean)

    for (const file of files) {
      try {
        const content = await fs.readFile(path.join(internalPath, file), 'utf8')
        const tag = resolver.parse(content, file)
        if (tag) {
          log(`[Builder] Resolved ${strategy.name} image version: ${strategy.imageBase}:${tag} (from ${file})`)
          return `${strategy.imageBase}:${tag}`
        }
      } catch {
        // File not found or unreadable — try next
      }
    }

    return fallback
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
      Env: [
        'CI=true',
        'NPM_CONFIG_SPIN=false',
        'NPM_CONFIG_PROGRESS=false',
        'HUSKY=0',
        'TERM=dumb',
        'PIP_PROGRESS_BAR=off',
        'COMPOSER_NO_INTERACTION=1'
      ],
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
