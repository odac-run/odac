/**
 * Unit tests for server/src/System.js
 *
 * Drives the real System module against the real Updater (with a fake Docker
 * daemon and a real unix socket standing in for the old container), because the
 * regression guarded here is purely about *when* the ready callback is
 * registered relative to `await Updater.init()` — a mocked Updater would encode
 * the assumption under test instead of checking it.
 */

const path = require('path')
const fs = require('fs')
const net = require('net')
const os = require('os')

describe('System', () => {
  let System
  let HOME
  let world // Map<name, {policy, running}> — fake Docker daemon state

  function makeFakeDockerClass() {
    return class FakeDocker {
      getContainer(key) {
        const miss = () => {
          const e = new Error(`No such container: ${key}`)
          e.statusCode = 404
          throw e
        }
        return {
          inspect: async () => {
            if (!world.has(key)) miss()
            const c = world.get(key)
            return {
              Id: key,
              Name: '/' + key,
              Config: {Env: []},
              HostConfig: {Binds: [], RestartPolicy: {Name: c.policy}},
              State: {Running: !!c.running},
              Mounts: []
            }
          },
          rename: async ({name: to}) => {
            if (!world.has(key)) miss()
            world.set(to, world.get(key))
            world.delete(key)
          },
          update: async o => {
            if (!world.has(key)) miss()
            world.get(key).policy = o.RestartPolicy.Name
          },
          remove: async () => world.delete(key),
          stop: async () => {}
        }
      }
    }
  }

  beforeEach(() => {
    world = new Map()
    HOME = path.join('/tmp', `odac-system-test-${process.pid}-${Math.random().toString(36).slice(2, 10)}`)
    fs.mkdirSync(path.join(HOME, '.odac', 'run'), {recursive: true})
    jest.spyOn(os, 'homedir').mockReturnValue(HOME)

    for (const name of ['Proxy', 'DNS', 'Hub', 'Mail', 'Api', 'App', 'Container', 'SSL']) {
      const mock = global.Odac.server(name)
      mock.start = jest.fn()
      mock.stop = jest.fn()
      mock.check = jest.fn()
    }
    global.Odac.server('Proxy').waitForReady = jest.fn().mockResolvedValue(true)
    global.Odac.server('DNS').waitForReady = jest.fn().mockResolvedValue(true)

    jest.resetModules()
    jest.doMock('dockerode', () => makeFakeDockerClass())
    jest.doMock('child_process', () => ({exec: jest.fn((cmd, cb) => cb(new Error('exec disabled in tests')))}))
    System = require('../../server/src/System')
  })

  afterEach(() => {
    System.stop()
    jest.restoreAllMocks()
    fs.rmSync(HOME, {recursive: true, force: true})
  })

  test('normal startup starts the services', async () => {
    world.set('odac', {policy: 'unless-stopped', running: true})

    await System.init()

    expect(global.Odac.server('Proxy').start).toHaveBeenCalled()
    expect(global.Odac.server('DNS').start).toHaveBeenCalled()
    expect(global.Odac.server('Hub').start).toHaveBeenCalled()
  })

  test('in update mode the services start on ACK, not after the handover completes', async () => {
    world.set('odac', {policy: 'unless-stopped', running: true})
    world.set('odac-update', {policy: 'no', running: true})

    const socketPath = path.join(HOME, '.odac', 'run', 'update.sock')
    const startedAtWebReady = {}

    // Hand-scripted OLD container implementing the documented handover protocol.
    // It never sends HANDOVER_COMPLETE, which is exactly the point: everything
    // asserted below has to have happened *before* the handover finishes.
    const webReady = new Promise((resolve, reject) => {
      const server = net.createServer(sock => {
        sock.on('data', d => {
          const msg = d.toString().trim()
          if (msg === 'HANDSHAKE_READY') sock.write('HANDSHAKE_ACK')
          else if (msg === 'WEB_READY') {
            startedAtWebReady.proxy = global.Odac.server('Proxy').start.mock.calls.length
            startedAtWebReady.dns = global.Odac.server('DNS').start.mock.calls.length
            sock.destroy()
            server.close()
            resolve()
          }
        })
        sock.on('error', () => {})
      })
      server.on('error', reject)
      server.listen(socketPath, () => {
        // init() stays pending here — it only resolves on HANDOVER_COMPLETE.
        System.init().catch(() => {})
      })
    })

    await webReady

    // The old instance tears down its own Proxy/DNS the moment it sees WEB_READY,
    // so the new instance's listeners must already be up at that point. Before the
    // fix, the ready callback was registered after `await Updater.init()` — nothing
    // was listening when the updater fired ready on ACK, both waitForReady() calls
    // timed out, and the services only started once the handover was fully done.
    expect(startedAtWebReady.proxy).toBe(1)
    expect(startedAtWebReady.dns).toBe(1)
    expect(global.Odac.server('Proxy').waitForReady).toHaveBeenCalled()
  })
})
