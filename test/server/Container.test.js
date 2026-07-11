const Ports = require('../../server/src/Ports')

const mockApp = {list: jest.fn()}

// Mock Odac before requiring Container: its module scope calls Odac.core('Log').
global.Odac = {
  core: jest.fn(name => {
    if (name === 'Log') return {init: () => ({log: jest.fn(), error: jest.fn()})}
    if (name === 'Config') return {config: {container: {available: true}}}
    return {}
  }),
  server: jest.fn(name => {
    if (name === 'Ports') return Ports
    if (name === 'App') return mockApp
    return {}
  })
}

jest.mock('dockerode')

const Docker = require('dockerode')

// Container is a singleton that news up Docker at require time, so the mock has
// to be in place before the module is loaded.
const mockDocker = {
  ping: jest.fn().mockResolvedValue(true),
  listNetworks: jest.fn().mockResolvedValue([{Name: 'odac-network'}]),
  createNetwork: jest.fn().mockResolvedValue({}),
  getImage: jest.fn().mockReturnValue({inspect: jest.fn().mockResolvedValue({Id: 'sha256:abc'})}),
  getContainer: jest.fn().mockReturnValue({
    stop: jest.fn().mockRejectedValue({statusCode: 404}),
    remove: jest.fn().mockRejectedValue({statusCode: 404}),
    inspect: jest.fn().mockRejectedValue({statusCode: 404})
  }),
  createContainer: jest.fn().mockResolvedValue({start: jest.fn().mockResolvedValue(true)})
}

Docker.mockImplementation(() => mockDocker)

const Container = require('../../server/src/Container')

describe('Container.runApp() port publishing', () => {
  beforeEach(() => {
    mockDocker.createContainer.mockClear()
  })

  /** Runs the app and returns the config handed to Docker. */
  const createOptionsFor = async ports => {
    await Container.runApp('web', {image: 'nginx', ports})

    return mockDocker.createContainer.mock.calls[0][0]
  }

  test('binds a published port to loopback', async () => {
    const config = await createOptionsFor([{host: 8080, container: 3000}])

    expect(config.HostConfig.PortBindings).toEqual({'3000/tcp': [{HostPort: '8080', HostIp: '127.0.0.1'}]})
    expect(config.ExposedPorts).toEqual({'3000/tcp': {}})
  })

  test('binds a public port to every interface', async () => {
    const config = await createOptionsFor([{host: 8080, container: 3000, public: true}])

    expect(config.HostConfig.PortBindings).toEqual({'3000/tcp': [{HostPort: '8080', HostIp: ''}]})
  })

  test('never publishes a proxy-routed entry', async () => {
    const config = await createOptionsFor([
      {host: 'proxy', container: 3000},
      {host: 9000, container: 9000}
    ])

    expect(config.HostConfig.PortBindings).toEqual({'9000/tcp': [{HostPort: '9000', HostIp: '127.0.0.1'}]})
    expect(config.ExposedPorts).toEqual({'9000/tcp': {}})
  })

  test('keeps both host bindings when one container port is published twice', async () => {
    // Assigning instead of appending here would silently drop the 8080 binding.
    const config = await createOptionsFor([
      {host: 8080, container: 3000},
      {host: 9090, container: 3000}
    ])

    expect(config.HostConfig.PortBindings).toEqual({
      '3000/tcp': [
        {HostPort: '8080', HostIp: '127.0.0.1'},
        {HostPort: '9090', HostIp: '127.0.0.1'}
      ]
    })
  })

  test('lets one container port be both proxy-routed and published', async () => {
    const config = await createOptionsFor([
      {host: 'proxy', container: 3000},
      {host: 8080, container: 3000, public: true}
    ])

    expect(config.HostConfig.PortBindings).toEqual({'3000/tcp': [{HostPort: '8080', HostIp: ''}]})
  })

  test('publishes nothing when every entry is proxy-routed', async () => {
    const config = await createOptionsFor([{host: 'proxy', container: 3000}])

    expect(config.HostConfig.PortBindings).toEqual({})
    expect(config.ExposedPorts).toEqual({})
  })
})

describe('Container.createTerminalSession()', () => {
  const {PassThrough} = require('stream')

  let containerMock

  const appList = names => ({result: true, data: names.map(name => ({name}))})

  beforeEach(() => {
    containerMock = {
      inspect: jest.fn().mockResolvedValue({State: {Running: true}}),
      exec: jest.fn(async options => {
        if (!options.Tty) {
          const stream = new PassThrough()
          stream.end()
          return {start: async () => stream}
        }
        return {
          start: jest.fn().mockResolvedValue(new PassThrough()),
          inspect: jest.fn().mockResolvedValue({Running: false, ExitCode: 0}),
          resize: jest.fn()
        }
      })
    }
    mockDocker.getContainer.mockReturnValue(containerMock)
    mockApp.list.mockResolvedValue(appList(['web', 'api']))
  })

  afterAll(() => {
    mockDocker.getContainer.mockReturnValue({
      stop: jest.fn().mockRejectedValue({statusCode: 404}),
      remove: jest.fn().mockRejectedValue({statusCode: 404}),
      inspect: jest.fn().mockRejectedValue({statusCode: 404})
    })
  })

  test('opens a TTY session for a managed, running app', async () => {
    // Timers off: the default idle/lifetime timers would outlive the test run.
    const terminal = await Container.createTerminalSession('web', {cols: 100, rows: 30, idleTimeout: 0, maxLifetime: 0})

    expect(terminal.container).toBe('web')
    expect(containerMock.exec).toHaveBeenCalledWith(expect.objectContaining({Tty: true, ConsoleSize: [30, 100]}))
  })

  test('refuses a container that is not a managed app', async () => {
    // The odac container itself mounts the Docker socket: a shell in it is host root.
    await expect(Container.createTerminalSession('odac')).rejects.toThrow('Unknown app: odac')
    expect(containerMock.exec).not.toHaveBeenCalled()
  })

  test('refuses an app that is not running', async () => {
    containerMock.inspect.mockResolvedValue({State: {Running: false}})

    await expect(Container.createTerminalSession('web')).rejects.toThrow('is not running')
    expect(containerMock.exec).not.toHaveBeenCalled()
  })

  test('refuses when the app list cannot be read', async () => {
    mockApp.list.mockResolvedValue({result: false})

    await expect(Container.createTerminalSession('web')).rejects.toThrow('Unknown app: web')
    expect(containerMock.exec).not.toHaveBeenCalled()
  })

  test('refuses a shell in a privileged container', async () => {
    // A --privileged container has host devices and kernel caps: a shell in it is host root.
    containerMock.inspect.mockResolvedValue({State: {Running: true}, HostConfig: {Privileged: true}})

    await expect(Container.createTerminalSession('web')).rejects.toThrow('privileged')
    expect(containerMock.exec).not.toHaveBeenCalled()
  })

  test('allows a privileged container only when explicitly opted in', async () => {
    containerMock.inspect.mockResolvedValue({State: {Running: true}, HostConfig: {Privileged: true}})

    const terminal = await Container.createTerminalSession('web', {allowPrivileged: true, idleTimeout: 0, maxLifetime: 0})

    expect(terminal.container).toBe('web')
    expect(containerMock.exec).toHaveBeenCalledWith(expect.objectContaining({Tty: true}))
  })

  test('defaults the exec to the app user rather than root', async () => {
    containerMock.inspect.mockResolvedValue({State: {Running: true}, Config: {User: 'app'}})

    await Container.createTerminalSession('web', {idleTimeout: 0, maxLifetime: 0})

    expect(containerMock.exec).toHaveBeenCalledWith(expect.objectContaining({User: 'app'}))
  })

  test('an explicit user overrides the container user', async () => {
    containerMock.inspect.mockResolvedValue({State: {Running: true}, Config: {User: 'app'}})

    await Container.createTerminalSession('web', {user: 'root', idleTimeout: 0, maxLifetime: 0})

    expect(containerMock.exec).toHaveBeenCalledWith(expect.objectContaining({User: 'root'}))
  })
})
