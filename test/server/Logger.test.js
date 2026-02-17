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
    return {}
  },
  server: () => ({})
}

// Temporary test directory
const mockTestDir = path.join(__dirname, '__logger_test_env__')

// Mock os.homedir to return mockTestDir
jest.mock('os', () => ({
  ...jest.requireActual('os'),
  homedir: () => mockTestDir
}))

const Logger = require('../../server/src/Container/Logger')

describe('Logger', () => {
  let logger
  const appName = 'test-app'

  beforeAll(async () => {
    // Ensuring clean env
    if (fs.existsSync(mockTestDir)) {
      await fs.promises.rm(mockTestDir, {recursive: true, force: true})
    }
    await fs.promises.mkdir(mockTestDir, {recursive: true})
  })

  afterAll(async () => {
    // Cleanup
    if (fs.existsSync(mockTestDir)) {
      await fs.promises.rm(mockTestDir, {recursive: true, force: true})
    }
  })

  beforeEach(async () => {
    // Clean app logs dir for each test
    // Path: mockTestDir/.odac/logs/test-app
    const appLogsDir = path.join(mockTestDir, '.odac', 'logs', appName)
    if (fs.existsSync(appLogsDir)) {
      await fs.promises.rm(appLogsDir, {recursive: true, force: true})
    }

    logger = new Logger(appName)
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
    // New Path: logs/appName/builds/...
    const logPath = path.join(mockTestDir, '.odac', 'logs', appName, 'builds', `${buildId}.log`)
    const summaryPath = path.join(mockTestDir, '.odac', 'logs', appName, 'builds', `${buildId}.json`)

    expect(fs.existsSync(logPath)).toBe(true)
    expect(fs.existsSync(summaryPath)).toBe(true)

    const summary = JSON.parse(await fs.promises.readFile(summaryPath, 'utf8'))
    expect(summary.status).toBe('success')
    expect(summary.phases.find(p => p.name === 'compile').status).toBe('success')
    expect(summary.duration).toBeGreaterThan(0)
  })

  test('should detect errors in build stream', async () => {
    const buildId = 'build_fail_1'
    const ctrl = logger.createBuildStream(buildId)

    ctrl.stream.write('Step 1: Starting...\n')
    ctrl.stream.write('Error: Module not found\n') // Trigger regex

    await ctrl.finalize(false)

    const summaryPath = path.join(mockTestDir, '.odac', 'logs', appName, 'builds', `${buildId}.json`)
    const summary = JSON.parse(await fs.promises.readFile(summaryPath, 'utf8'))

    expect(summary.status).toBe('failed')
    expect(summary.errors).toBe(1)
  })

  test('should rotate logs (keep last 10)', async () => {
    const buildsDir = path.join(mockTestDir, '.odac', 'logs', appName, 'builds')

    for (let i = 0; i < 12; i++) {
      const id = `build_old_${i}`
      const jsonPath = path.join(buildsDir, `${id}.json`)
      const logPath = path.join(buildsDir, `${id}.log`)

      await fs.promises.writeFile(jsonPath, JSON.stringify({id, timestamp: Date.now() - 1000 * i}))
      await fs.promises.writeFile(logPath, 'dummy log')
      await new Promise(r => setTimeout(r, 50))
    }

    const ctrl = logger.createBuildStream('build_trigger_rotation')
    await ctrl.finalize(true)

    // Wait for async rotation
    await new Promise(r => setTimeout(r, 1000))

    const files = await fs.promises.readdir(buildsDir)
    const jsonFiles = files.filter(f => f.endsWith('.json'))

    expect(jsonFiles.length).toBe(10)
  }, 10000)

  test('should create runtime stream', () => {
    const ctrl = logger.createRuntimeStream()
    expect(ctrl.stream).toBeDefined()
    // Path should contain appName
    expect(ctrl.path).toContain(`.odac/logs/${appName}/runtime`)
    ctrl.stream.end()
  })

  test('should generate daily summary', async () => {
    const buildsDir = path.join(mockTestDir, '.odac', 'logs', appName, 'builds')
    const now = Date.now()

    const build1 = {
      id: 'b1',
      status: 'success',
      timestamp: now - 2 * 60 * 60 * 1000,
      duration: 10,
      errors: 0
    }
    await fs.promises.writeFile(path.join(buildsDir, 'b1.json'), JSON.stringify(build1))

    const build2 = {
      id: 'b2',
      status: 'failed',
      timestamp: now - 25 * 60 * 60 * 1000,
      duration: 5,
      errors: 1
    }
    await fs.promises.writeFile(path.join(buildsDir, 'b2.json'), JSON.stringify(build2))

    const summary = await logger.getDailySummary()

    expect(summary.total).toBe(1)
    expect(summary.success).toBe(1)
    expect(summary.failed).toBe(0)
    expect(summary.totalDuration).toBe(10)
    expect(summary.builds.length).toBe(1)
    expect(summary.builds[0].id).toBe('b1')
  })
})
