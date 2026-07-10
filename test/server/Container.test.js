const Ports = require('../../server/src/Ports')

// Mock Odac before requiring Container: its module scope calls Odac.core('Log').
global.Odac = {
  core: jest.fn(name => {
    if (name === 'Log') return {init: () => ({log: jest.fn(), error: jest.fn()})}
    if (name === 'Config') return {config: {container: {available: true}}}
    return {}
  }),
  server: jest.fn(name => {
    if (name === 'Ports') return Ports
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
