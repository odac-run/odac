const fs = require('fs')
const path = require('path')

// Mocks MUST be defined before requiring the module that uses them
global.Odac = {
  core: service => {
    if (service === 'Log') {
      return {
        init: () => ({
          log: (...args) => console.log('[MOCK LOG]', ...args),
          error: (...args) => console.error('[MOCK ERR]', ...args)
        })
      }
    }
    if (service === 'Config') {
      return {
        config: {
          app: {
            path: '/tmp/odac/apps'
          }
        }
      }
    }
    return {}
  },
  server: () => ({})
}

const Logger = require('../../server/src/Container/Logger')

// Temporary test directory
const TEST_DIR = path.join(__dirname, '__logger_test_env__')

describe('Logger', () => {
  let logger

  beforeAll(async () => {
    // Ensuring clean env
    if (fs.existsSync(TEST_DIR)) {
      await fs.promises.rm(TEST_DIR, {recursive: true, force: true})
    }
    await fs.promises.mkdir(TEST_DIR, {recursive: true})
  })

  afterAll(async () => {
    // Cleanup
    if (fs.existsSync(TEST_DIR)) {
      await fs.promises.rm(TEST_DIR, {recursive: true, force: true})
    }
  })

  beforeEach(async () => {
    // Clean builds dir for each test
    const buildsDir = path.join(TEST_DIR, '.odac', 'logs', 'builds')
    if (fs.existsSync(buildsDir)) {
      await fs.promises.rm(buildsDir, {recursive: true, force: true})
    }

    logger = new Logger(TEST_DIR)
    await logger.init()
  })

  test('should create build stream and summary correctly', async () => {
    const buildId = 'build_test_1'
    const ctrl = logger.createBuildStream(buildId, {trigger: 'manual'})

    // Simulate logs
    ctrl.stream.write('Step 1: Installing dependencies...\n')
    ctrl.stream.write('Step 2: npm install completed.\n')
    ctrl.startPhase('compile')
    await new Promise(r => setTimeout(r, 10)) // simulate work
    ctrl.stream.write('Step 3: Compiling...\n')
    ctrl.endPhase('compile', true)

    await ctrl.finalize(true)

    // Check files
    const logPath = path.join(TEST_DIR, '.odac', 'logs', 'builds', `${buildId}.log`)
    const summaryPath = path.join(TEST_DIR, '.odac', 'logs', 'builds', `${buildId}.json`)

    expect(fs.existsSync(logPath)).toBe(true)
    expect(fs.existsSync(summaryPath)).toBe(true)

    const summary = JSON.parse(await fs.promises.readFile(summaryPath, 'utf8'))
    expect(summary.status).toBe('success')
    expect(summary.phases.compile.status).toBe('success')
    expect(summary.duration).toBeGreaterThan(0)
  })

  test('should detect errors in build stream', async () => {
    const buildId = 'build_fail_1'
    const ctrl = logger.createBuildStream(buildId)

    ctrl.stream.write('Step 1: Starting...\n')
    ctrl.stream.write('Error: Module not found\n') // Trigger regex

    await ctrl.finalize(false)

    const summaryPath = path.join(TEST_DIR, '.odac', 'logs', 'builds', `${buildId}.json`)
    const summary = JSON.parse(await fs.promises.readFile(summaryPath, 'utf8'))

    expect(summary.status).toBe('failed')
    expect(summary.errors).toBe(1)
  })

  test('should rotate logs (keep last 10)', async () => {
    // Create 12 fake build logs
    const buildsDir = path.join(TEST_DIR, '.odac', 'logs', 'builds')

    // We need to ensure mtime is different for sorting
    // But since fs.writeFile is fast, we might need manual mtime update or robust creation
    // However, the test uses fileStats.sort((a,b) => b.time - a.time)

    for (let i = 0; i < 12; i++) {
      const id = `build_old_${i}`
      const jsonPath = path.join(buildsDir, `${id}.json`)
      const logPath = path.join(buildsDir, `${id}.log`)

      // Decreasing timestamp for JSON data (not used for rotation, rotation uses mtime)
      await fs.promises.writeFile(jsonPath, JSON.stringify({id, timestamp: Date.now() - 1000 * i}))
      await fs.promises.writeFile(logPath, 'dummy log')

      // Force update access and modification time to ensure sorting works based on creation order logic
      // We make the first created (i=0) the newest, and subsequent ones older?
      // Wait: The loop runs i=0 to 11.
      // If we want i=0 to be the newest (kept) and i=11 to be oldest (deleted).
      // Default fs behavior: last written is newest.
      // So i=11 will be newest.
      // Wait, loop: i=0 writes, then i=1 writes. i=1 is newer.
      // So i=11 is the newest file. i=0 is the oldest.
      // We create 12 files. 10 should be kept.
      // The ones deleted should be i=0 and i=1.

      await new Promise(r => setTimeout(r, 50))
    }

    // Trigger rotation by creating a new one (13th file)
    const ctrl = logger.createBuildStream('build_trigger_rotation')
    await ctrl.finalize(true)

    // Wait for async rotation
    await new Promise(r => setTimeout(r, 1000))

    const files = await fs.promises.readdir(buildsDir)
    const jsonFiles = files.filter(f => f.endsWith('.json'))

    // Should keep 10 files.
    // console.log('Files kept:', jsonFiles)
    expect(jsonFiles.length).toBe(10)
  }, 10000) // Increase timeout

  test('should create runtime stream', () => {
    const ctrl = logger.createRuntimeStream()
    expect(ctrl.stream).toBeDefined()
    expect(ctrl.path).toContain('.odac/logs/runtime')
    ctrl.stream.end()
  })

  test('should generate daily summary', async () => {
    // Prepare fake data
    const buildsDir = path.join(TEST_DIR, '.odac', 'logs', 'builds')
    const now = Date.now()

    // 1 success 2 hours ago
    const build1 = {
      id: 'b1',
      status: 'success',
      timestamp: now - 2 * 60 * 60 * 1000, // 2 hours ago
      duration: 10,
      errors: 0
    }
    await fs.promises.writeFile(path.join(buildsDir, 'b1.json'), JSON.stringify(build1))

    // 1 fail 25 hours ago (should be ignored)
    const build2 = {
      id: 'b2',
      status: 'failed',
      timestamp: now - 25 * 60 * 60 * 1000, // 25 hours ago
      duration: 5,
      errors: 1
    }
    await fs.promises.writeFile(path.join(buildsDir, 'b2.json'), JSON.stringify(build2))

    // Debug
    // const files = await fs.promises.readdir(buildsDir)
    // console.log('Files in summary test:', files)

    const summary = await logger.getDailySummary()

    // console.log('Summary:', summary)

    expect(summary.total).toBe(1)
    expect(summary.success).toBe(1)
    expect(summary.failed).toBe(0)
    expect(summary.totalDuration).toBe(10)
    expect(summary.builds.length).toBe(1)
    expect(summary.builds[0].id).toBe('b1')
  })
})
