const path = require('path')
const fs = require('fs')

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
    log(`Paths - Host: ${hostPath}, Internal: ${internalPath}`)

    // 1. Detect Strategy (Use internal path to read files)
    const strategy = await this.#detect(internalPath)
    if (!strategy) {
      throw new Error('Could not detect project type (no package.json found)')
    }
    log(`Detected project type: ${strategy.name}`)

    // 2. Compile (Artifact Builder)
    await this.#compile(strategy, context)

    // 3. Package (Image Packager)
    await this.#package(strategy, context, imageName)

    log(`Build completed successfully: ${imageName}`)
    return true
  }

  /**
   * Detects project language/framework
   * @param {string} internalPath
   * @returns {Promise<Object>} Strategy object
   */
  async #detect(internalPath) {
    // Check for Node.js
    if (fs.existsSync(path.join(internalPath, 'package.json'))) {
      return {
        name: 'Node.js',
        image: 'node:lts-alpine', // Compiler image
        installCmd: 'npm ci --production || npm install --production',
        buildCmd: 'if [ -f "tsconfig.json" ] || grep -q "build" package.json; then npm run build --if-present; fi',
        cleanupCmd: 'rm -rf .git .github test tests',
        package: {
          baseImage: 'node:lts-alpine',
          user: 'node',
          cmd: ['npm', 'start']
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

      // We stream valid stdout to debug log, but capture stderr > log on error
      const stream = await this.#docker.run(
        strategy.image,
        ['sh', '-c', commands],
        [process.stdout, process.stderr],
        containerConfig.HostConfig
      )

      const output = await stream
      if (output && output.StatusCode !== 0) {
        throw new Error(`Compilation failed with exit code ${output.StatusCode}`)
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
    const dockerfileContent = `
FROM ${strategy.package.baseImage}
WORKDIR /app
COPY . .
USER root
RUN chown -R ${strategy.package.user}:${strategy.package.user} /app
USER ${strategy.package.user}
CMD ${JSON.stringify(strategy.package.cmd)}
`
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

      const stream = await this.#docker.run(
        packagerImage,
        ['sh', '-c', buildCmd],
        [process.stdout, process.stderr],
        packagerConfig.HostConfig
      )

      const output = await stream
      if (output && output.StatusCode !== 0) {
        throw new Error(`Packaging failed with exit code ${output.StatusCode}`)
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
