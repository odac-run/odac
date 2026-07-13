/**
 * Unit tests for server/src/System/Updater.js
 *
 * These tests drive the real Updater module against a stateful fake Docker
 * daemon (a Map of container name -> {policy, running}) instead of mocking
 * individual method calls, because the bugs this file guards against are
 * about *sequences* of Docker operations (rename/update/remove ordering)
 * producing an unsafe intermediate state, not about any single call.
 *
 * Real `fs` and `net` are used (not mocked) for the socket-handshake tests:
 * the regressions covered here are about actual unix-socket behavior and
 * actual container identity resolution, which a mocked fs/net would hide.
 */

const path = require('path')
const fs = require('fs')
const net = require('net')
const os = require('os')

function waitUntil(predicate, {timeout = 2000, interval = 10} = {}) {
  return new Promise((resolve, reject) => {
    const start = Date.now()
    const tick = () => {
      if (predicate()) return resolve()
      if (Date.now() - start > timeout) return reject(new Error('waitUntil: timed out waiting for condition'))
      setTimeout(tick, interval)
    }
    tick()
  })
}

// Force the Linux zero-downtime branch everywhere. The Windows/Mac branch of
// execute() calls process.exit(0), which would kill the test worker.
const originalPlatform = process.platform
beforeAll(() => {
  Object.defineProperty(process, 'platform', {value: 'linux', configurable: true})
})
afterAll(() => {
  Object.defineProperty(process, 'platform', {value: originalPlatform, configurable: true})
})

describe('Updater', () => {
  let world // Map<name, {policy, running, env, binds}>
  let idIndex // Map<fakeContainerId, name> - used only by the identity-probe test
  let events // string[] - log of mutating Docker calls, in order
  let HOME
  let Updater

  const realExistsSync = fs.existsSync
  const realReadFileSync = fs.readFileSync
  let dockerenvExists
  let procFiles // Map<path, string> - content served for /proc/* reads; absent => ENOENT

  function makeFakeDockerClass() {
    return class FakeDocker {
      getContainer(key) {
        const resolved = world.has(key) ? key : idIndex.get(key)
        const miss = () => {
          const e = new Error(`No such container: ${key}`)
          e.statusCode = 404
          throw e
        }
        return {
          inspect: async () => {
            if (!resolved || !world.has(resolved)) miss()
            const c = world.get(resolved)
            return {
              Id: resolved,
              Name: '/' + resolved,
              Config: {Env: c.env || []},
              HostConfig: {Binds: c.binds || [], RestartPolicy: {Name: c.policy}},
              State: {Running: !!c.running},
              Mounts: []
            }
          },
          rename: async ({name: to}) => {
            if (!resolved || !world.has(resolved)) miss()
            if (world.has(to)) throw new Error(`name ${to} already in use`)
            world.set(to, world.get(resolved))
            world.delete(resolved)
            events.push(`rename:${resolved}->${to}`)
          },
          update: async o => {
            if (!resolved || !world.has(resolved)) miss()
            world.get(resolved).policy = o.RestartPolicy.Name
            events.push(`policy:${resolved}=${o.RestartPolicy.Name}`)
          },
          remove: async () => {
            if (!resolved || !world.has(resolved)) miss()
            world.delete(resolved)
            events.push(`remove:${resolved}`)
          },
          start: async () => {
            if (resolved && world.has(resolved)) world.get(resolved).running = true
            events.push(`start:${resolved}`)
          },
          stop: async () => {
            if (resolved && world.has(resolved)) world.get(resolved).running = false
          },
          logs: async () => {
            throw new Error('no logs available in test harness')
          }
        }
      }

      async createContainer(opts) {
        world.set(opts.name, {policy: opts.HostConfig.RestartPolicy.Name, running: false, env: opts.Env, binds: opts.HostConfig.Binds})
        events.push(`create:${opts.name}:policy=${opts.HostConfig.RestartPolicy.Name}`)
        return this.getContainer(opts.name)
      }
    }
  }

  function loadUpdater() {
    jest.resetModules()
    jest.doMock('dockerode', () => makeFakeDockerClass())
    // Real docker/exec is never wanted in a unit test; fail fast and deterministically.
    jest.doMock('child_process', () => ({exec: jest.fn((cmd, cb) => cb(new Error('exec disabled in tests')))}))
    Updater = require('../../server/src/System/Updater')
    return Updater
  }

  beforeEach(() => {
    world = new Map()
    idIndex = new Map()
    events = []
    dockerenvExists = true
    procFiles = new Map()

    HOME = path.join('/tmp', `odac-test-${process.pid}-${Math.random().toString(36).slice(2, 10)}`)
    fs.mkdirSync(path.join(HOME, '.odac', 'run'), {recursive: true})
    // os.homedir() reads the process's native env snapshot, which jest's test
    // environment does not keep in sync with JS-level process.env writes -
    // spy on the function directly instead of relying on process.env.HOME.
    jest.spyOn(os, 'homedir').mockReturnValue(HOME)

    jest.spyOn(fs, 'existsSync').mockImplementation(p => {
      if (p === '/.dockerenv') return dockerenvExists
      return realExistsSync(p)
    })
    jest.spyOn(fs, 'readFileSync').mockImplementation((p, enc) => {
      if (procFiles.has(p)) return procFiles.get(p)
      if (typeof p === 'string' && p.startsWith('/proc/')) {
        const e = new Error(`ENOENT: ${p}`)
        e.code = 'ENOENT'
        throw e
      }
      return realReadFileSync(p, enc)
    })

    // #performHandshake's ACK branch awaits these; the default global Odac
    // mock doesn't define them at all.
    global.Odac.server('Proxy').waitForReady = jest.fn().mockResolvedValue(true)
    global.Odac.server('DNS').waitForReady = jest.fn().mockResolvedValue(true)

    if (process.env.UPDATER_TEST_DEBUG) {
      global.Odac.core('Log').init = jest.fn(name => ({
        log: (...a) => fs.appendFileSync('/tmp/updater-test-trace.log', `[${name}] ${a.join(' ')}\n`),
        error: (...a) => fs.appendFileSync('/tmp/updater-test-trace.log', `[${name}:ERR] ${a.join(' ')}\n`),
        warn: (...a) => fs.appendFileSync('/tmp/updater-test-trace.log', `[${name}:WARN] ${a.join(' ')}\n`)
      }))
    }

    loadUpdater()
  })

  afterEach(() => {
    jest.restoreAllMocks()
    fs.rmSync(HOME, {recursive: true, force: true})
  })

  describe('startup self-heal (init)', () => {
    test('does nothing on a healthy host', async () => {
      world.set('odac', {policy: 'unless-stopped', running: true})
      await Updater.init()
      expect(events).toEqual([])
    })

    test("preserves an operator's deliberate 'always' policy", async () => {
      world.set('odac', {policy: 'always', running: true})
      await Updater.init()
      expect(events).toEqual([])
    })

    test('repairs a container left on RestartPolicy "no"', async () => {
      world.set('odac', {policy: 'no', running: true})
      await Updater.init()
      expect(events).toEqual(['policy:odac=unless-stopped'])
    })

    test('reclaims identity and repairs policy when stranded as odac-backup', async () => {
      // This is the exact state a host is left in when a crashed update's
      // rollback renamed the survivor back... except the rename itself never
      // happened, or a pre-fix version of the code left it here.
      world.set('odac-backup', {policy: 'no', running: true})
      await Updater.init()
      expect(events).toEqual(['rename:odac-backup->odac', 'policy:odac=unless-stopped'])
    })

    test('reclaims identity and repairs policy when stranded as odac-update', async () => {
      world.set('odac-update', {policy: 'no', running: true})
      await Updater.init()
      expect(events).toEqual(['rename:odac-update->odac', 'policy:odac=unless-stopped'])
    })

    test('leaves a stale exited backup alone next to a healthy odac', async () => {
      world.set('odac', {policy: 'unless-stopped', running: true})
      world.set('odac-backup', {policy: 'no', running: false})
      await Updater.init()
      expect(events).toEqual([])
    })

    test('does nothing outside a container (dev host)', async () => {
      dockerenvExists = false
      world.set('odac-backup', {policy: 'no', running: true})
      await Updater.init()
      expect(events).toEqual([])
    })

    test('does nothing when identity cannot be resolved at all', async () => {
      // No 'odac', no 'odac-backup', no 'odac-update' anywhere.
      await Updater.init()
      expect(events).toEqual([])
    })

    test('resolves identity via /proc/self/cgroup when name lookup fails', async () => {
      const fakeId = 'a'.repeat(64)
      procFiles.set('/proc/self/cgroup', `0::/system.slice/docker-${fakeId}.scope\n`)
      world.set('odac-backup', {policy: 'no', running: true})
      idIndex.set(fakeId, 'odac-backup')

      await Updater.init()
      expect(events).toEqual(['rename:odac-backup->odac', 'policy:odac=unless-stopped'])
    })
  })

  describe('zero-downtime takeOver invariant', () => {
    test('never leaves more than one container able to auto-start, and reboot resolves to the new odac', async () => {
      world.set('odac', {policy: 'unless-stopped', running: true}) // old, live
      world.set('odac-update', {policy: 'no', running: true}) // new, that's the real Updater under test

      const socketPath = path.join(HOME, '.odac', 'run', 'update.sock')
      const PERSISTENT = p => p === 'always' || p === 'unless-stopped'
      const snapshots = []
      const snap = label => {
        const autostart = [...world.entries()].filter(([, c]) => PERSISTENT(c.policy) && c.running).map(([n]) => n)
        snapshots.push({label, autostart: autostart.join(',')})
      }
      snap('before')

      // Wrap the fake docker to snapshot after every mutation, mirroring what
      // an OS reboot would resurrect at that instant.
      const wrappedPush = events.push.bind(events)
      events.push = (...a) => {
        const r = wrappedPush(...a)
        snap(a[0])
        return r
      }

      // Hand-scripted OLD container side: implements the documented protocol
      // (see comments in Updater.js #createUpdateListener) without calling any
      // real Updater code, so the real #takeOver() below is the only thing
      // actually under test.
      await new Promise((resolve, reject) => {
        const dbg = process.env.UPDATER_TEST_DEBUG ? line => fs.appendFileSync('/tmp/updater-test-trace.log', line + '\n') : () => {}
        dbg(`--- test start, socketPath=${socketPath} ---`)
        const server = net.createServer(sock => {
          dbg('[test-server] connection accepted')
          sock.on('data', d => {
            const msg = d.toString().trim()
            dbg(`[test-server] got: ${JSON.stringify(msg)}`)
            if (msg === 'HANDSHAKE_READY') sock.write('HANDSHAKE_ACK')
            else if (msg === 'WEB_READY') {
              server.close()
              resolve()
            }
          })
          sock.on('error', e => dbg(`[test-server] sock error: ${e.message}`))
          sock.on('close', () => dbg('[test-server] sock closed'))
        })
        server.on('error', reject)
        server.listen(socketPath, () => {
          Updater.init().catch(reject) // the NEW container joining: real performHandshake + real takeOver()
        })
      })

      expect(snapshots.every(s => s.autostart.split(',').filter(Boolean).length <= 1)).toBe(true)
      expect(snapshots[snapshots.length - 1].autostart).toBe('odac')
      expect(world.get('odac')?.policy).toBe('unless-stopped')
      expect(world.get('odac-backup')?.policy).toBe('no')
      expect(world.has('odac-update')).toBe(false)
    })
  })

  describe('rollback safety', () => {
    test('crash right after takeOver does not self-handshake and leaves the new odac running', async () => {
      world.set('odac', {
        policy: 'unless-stopped',
        running: true,
        env: ['ODAC_CHANNEL=stable'],
        binds: [`${HOME}/.odac:/app/.odac`]
      })

      // Rollback restarts services via System.init(), which for real would
      // re-enter Updater.init(). Point that straight at the real Updater
      // singleton under test so the self-handshake regression has a real
      // listener to reconnect to if it reappears.
      global.Odac.server('System').init = jest.fn(() => Updater.init())

      const execPromise = Updater.execute()

      await waitUntil(() => events.some(e => e.startsWith('create:odac-update')))
      const socketPath = path.join(HOME, '.odac', 'run', 'update.sock')
      await waitUntil(() => fs.existsSync(socketPath))

      const sock = net.createConnection(socketPath)
      await new Promise((resolve, reject) => {
        sock.on('connect', () => sock.write('HANDSHAKE_READY'))
        sock.on('error', reject)
        sock.on('data', async d => {
          if (d.toString().trim() !== 'HANDSHAKE_ACK') return
          // Perform the real #takeOver() semantics as the new container would,
          // then crash before TAKEOVER_COMPLETE.
          const docker = new (makeFakeDockerClass())()
          await docker.getContainer('odac').rename({name: 'odac-backup'})
          await docker.getContainer('odac-backup').update({RestartPolicy: {Name: 'no'}})
          await docker.getContainer('odac-update').rename({name: 'odac'})
          await docker.getContainer('odac').update({RestartPolicy: {Name: 'unless-stopped'}})
          sock.destroy()
          resolve()
        })
      })

      await expect(execPromise).rejects.toThrow()

      // The critical assertion: rollback must not have handshaked with itself
      // and undone the takeover. If it had, this would read 'odac-backup' only.
      expect(world.has('odac')).toBe(true)
      expect(world.get('odac').policy).toBe('unless-stopped')
      expect(world.get('odac').running).toBe(true)
      // The crashed new instance must be gone, but the live container that was
      // running inside the process handling rollback must never be force-removed.
      expect(events).not.toContain('remove:odac')
    })

    test('crash before takeOver never force-removes the still-live odac container', async () => {
      world.set('odac', {
        policy: 'unless-stopped',
        running: true,
        env: ['ODAC_CHANNEL=stable'],
        binds: [`${HOME}/.odac:/app/.odac`]
      })
      global.Odac.server('System').init = jest.fn(() => Updater.init())

      const execPromise = Updater.execute()

      await waitUntil(() => events.some(e => e.startsWith('create:odac-update')))
      const socketPath = path.join(HOME, '.odac', 'run', 'update.sock')
      await waitUntil(() => fs.existsSync(socketPath))

      const sock = net.createConnection(socketPath)
      await new Promise((resolve, reject) => {
        sock.on('connect', () => sock.write('HANDSHAKE_READY'))
        sock.on('error', reject)
        sock.on('data', d => {
          if (d.toString().trim() !== 'HANDSHAKE_ACK') return
          // Crash immediately, before doing anything takeOver() would do.
          sock.destroy()
          resolve()
        })
      })

      await expect(execPromise).rejects.toThrow()

      // Pre-fix behavior: rollback force-removed whatever was named 'odac',
      // which at this point is the live container itself.
      expect(events).not.toContain('remove:odac')
      expect(world.has('odac')).toBe(true)
      expect(world.get('odac').running).toBe(true)
      expect(world.get('odac').policy).toBe('unless-stopped')
    })
  })

  describe('update env accumulation', () => {
    test('strips all update-cycle env vars from the inherited env instead of two of five', async () => {
      world.set('odac', {
        policy: 'unless-stopped',
        running: true,
        env: [
          'PATH=/usr/bin',
          'ODAC_CHANNEL=stable',
          // Simulates a host that went through the pre-fix update path at
          // least once: these should have been stripped and weren't.
          'ODAC_UPDATE_MODE=true',
          'ODAC_INSTANCE_ID=stale-instance',
          'ODAC_PREVIOUS_INSTANCE_ID=stale-previous',
          'ODAC_UPDATE_SOCKET_PATH=/stale/path.sock',
          'ODAC_LOG_NAME=.odac-update'
        ],
        binds: [`${HOME}/.odac:/app/.odac`]
      })
      global.Odac.server('System').init = jest.fn(() => Updater.init())

      const execPromise = Updater.execute()
      await waitUntil(() => events.some(e => e.startsWith('create:odac-update')))

      const newEnv = world.get('odac-update').env
      const countOf = key => newEnv.filter(e => e.startsWith(`${key}=`)).length

      for (const key of ['ODAC_UPDATE_MODE', 'ODAC_INSTANCE_ID', 'ODAC_PREVIOUS_INSTANCE_ID', 'ODAC_UPDATE_SOCKET_PATH', 'ODAC_LOG_NAME']) {
        expect(countOf(key)).toBe(1)
      }
      expect(newEnv.find(e => e.startsWith('ODAC_INSTANCE_ID='))).not.toBe('ODAC_INSTANCE_ID=stale-instance')
      expect(newEnv).toContain('PATH=/usr/bin')
      expect(newEnv).toContain('ODAC_CHANNEL=stable')

      // Unblock execute() so the test doesn't leave dangling timers/handles.
      const socketPath = path.join(HOME, '.odac', 'run', 'update.sock')
      await waitUntil(() => fs.existsSync(socketPath))
      const sock = net.createConnection(socketPath)
      await new Promise(resolve => {
        sock.on('connect', () => sock.write('HANDSHAKE_READY'))
        sock.on('data', d => {
          if (d.toString().trim() === 'HANDSHAKE_ACK') {
            sock.destroy()
            resolve()
          }
        })
        sock.on('error', resolve)
      })
      await expect(execPromise).rejects.toThrow()
    })
  })

  describe('update latch', () => {
    // The exec mock in this harness makes every `docker pull` fail, which is the
    // real-world case being reproduced: an unreachable registry.
    test('a failing update check releases the latch instead of blocking every later attempt', async () => {
      const first = await Updater.start()
      expect(first.result).toBe(false)

      // Pre-fix, #updating stayed true forever and this came back as
      // 'Update already in progress' until the container was restarted.
      const second = await Updater.start()
      expect(second.message).not.toMatch(/already in progress/i)
    })
  })
})
