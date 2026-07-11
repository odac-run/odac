const {PassThrough} = require('stream')

// Terminal's module scope calls Odac.core('Log'); jest.setup.js installs the global mock.
const Terminal = require('../../server/src/Container/Terminal')

/**
 * Fake dockerode container.
 *
 * `exec()` serves two very different callers: the TTY session (Tty:true) and the throwaway
 * reap execs (Tty absent). Reap scripts are captured so tests can assert on the signal sent.
 */
function createContainerMock({runningSequence = [false]} = {}) {
  const sessionStream = new PassThrough()
  sessionStream.destroy = jest.fn(sessionStream.destroy.bind(sessionStream))

  const inspectResults = [...runningSequence]
  const sessionExec = {
    start: jest.fn(async opts => {
      sessionExec.startOptions = opts
      return sessionStream
    }),
    resize: jest.fn().mockResolvedValue(undefined),
    inspect: jest.fn(async () => {
      const running = inspectResults.length > 1 ? inspectResults.shift() : inspectResults[0]
      return {Running: running, ExitCode: running ? null : 0}
    })
  }

  const execOptions = []
  const reapScripts = []

  const container = {
    exec: jest.fn(async options => {
      execOptions.push(options)

      if (options.Tty) return sessionExec

      reapScripts.push(options.Cmd[2])
      return {
        start: async () => {
          const stream = new PassThrough()
          // End synchronously: Jest's fake timers also fake process.nextTick, so deferring
          // this would stall the reap's `await stream.on('end')` in the timeout tests.
          // 'end' still fires once #runInContainer attaches its drain listener.
          stream.end()
          return stream
        }
      }
    })
  }

  const docker = {getContainer: jest.fn(() => container)}

  return {docker, container, sessionExec, sessionStream, execOptions, reapScripts}
}

const sessionOptions = mock => mock.execOptions.find(o => o.Tty)

/**
 * Opens a session with the idle/lifetime timers off. Tests that leave a session open would
 * otherwise strand a 15-minute timer and keep the Jest worker alive. The timeout suite below
 * sets its own durations.
 */
const openTerminal = (mock, options = {}) => new Terminal(mock.docker, 'web', {idleTimeout: 0, maxLifetime: 0, ...options}).open()

describe('Terminal.open()', () => {
  test('creates a TTY exec sized up front and tagged with a session env var', async () => {
    const mock = createContainerMock()
    await openTerminal(mock, {cols: 120, rows: 40})

    const options = sessionOptions(mock)
    expect(options.Tty).toBe(true)
    expect(options.AttachStdin).toBe(true)
    // [h, w] — sizing at create avoids a first paint at 0x0.
    expect(options.ConsoleSize).toEqual([40, 120])
    expect(options.Env).toEqual([expect.stringMatching(/^ODAC_TTY_SESSION=[a-f0-9]{32}$/)])
  })

  test('repeats Tty on start, or the daemon streams multiplexed frames', async () => {
    const mock = createContainerMock()
    await openTerminal(mock)

    expect(mock.sessionExec.startOptions).toEqual({hijack: true, stdin: true, Tty: true})
  })

  test('defaults to a bash-or-sh probe and honours an explicit command', async () => {
    const mock = createContainerMock()
    await openTerminal(mock)
    expect(sessionOptions(mock).Cmd).toEqual(Terminal.DEFAULT_SHELL)

    const custom = createContainerMock()
    await openTerminal(custom, {command: ['/bin/zsh']})
    expect(sessionOptions(custom).Cmd).toEqual(['/bin/zsh'])
  })

  test('passes user, workdir and extra env through', async () => {
    const mock = createContainerMock()
    await openTerminal(mock, {user: 'app', workdir: '/srv', env: ['FOO=bar']})

    const options = sessionOptions(mock)
    expect(options.User).toBe('app')
    expect(options.WorkingDir).toBe('/srv')
    expect(options.Env).toEqual([expect.stringMatching(/^ODAC_TTY_SESSION=/), 'FOO=bar'])
  })

  test('refuses to open twice', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock)

    await expect(terminal.open()).rejects.toThrow('already opened')
  })
})

describe('Terminal I/O', () => {
  test('forwards output to onData', async () => {
    const mock = createContainerMock()
    const chunks = []
    await openTerminal(mock, {onData: c => chunks.push(c.toString())})

    mock.sessionStream.write('hello')
    await new Promise(r => setImmediate(r))

    expect(chunks).toEqual(['hello'])
  })

  test('forwards input to the pty', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock)

    const written = []
    mock.sessionStream.on('data', c => written.push(c.toString()))

    expect(terminal.write('ls\n')).toBe(true)
    await new Promise(r => setImmediate(r))

    expect(written).toEqual(['ls\n'])
  })

  test('resize maps cols/rows onto Docker w/h', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock)

    await expect(terminal.resize(100, 30)).resolves.toBe(true)
    expect(mock.sessionExec.resize).toHaveBeenCalledWith({h: 30, w: 100})
  })

  test('resize clamps nonsense dimensions instead of passing them to Docker', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock, {cols: 80, rows: 24})

    await terminal.resize(99999, 0)

    // width capped, height fell back to the previous value rather than 0
    expect(mock.sessionExec.resize).toHaveBeenCalledWith({h: 24, w: 1000})
  })

  test('resize survives a shell that exited mid-call', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock)
    mock.sessionExec.resize.mockRejectedValueOnce(new Error('no such exec'))

    await expect(terminal.resize(90, 20)).resolves.toBe(false)
  })

  test('write and resize are no-ops once closed', async () => {
    const mock = createContainerMock()
    const terminal = await openTerminal(mock)
    await terminal.close()

    expect(terminal.write('x')).toBe(false)
    await expect(terminal.resize(90, 20)).resolves.toBe(false)
    expect(mock.sessionExec.resize).not.toHaveBeenCalled()
  })
})

describe('Terminal.close() reaping', () => {
  test('destroys the stream and hangs up the session tree', async () => {
    const mock = createContainerMock({runningSequence: [false]})
    const terminal = await openTerminal(mock)

    await terminal.close()

    expect(mock.sessionStream.destroy).toHaveBeenCalled()
    expect(mock.reapScripts).toHaveLength(1)
    expect(mock.reapScripts[0]).toContain('kill -HUP')
    expect(terminal.closed).toBe(true)
  })

  test('escalates to SIGKILL when the shell ignores the hangup', async () => {
    // Running: true after HUP, still true after the grace wait, false once KILLed.
    const mock = createContainerMock({runningSequence: [true, true, false, false]})
    const terminal = await openTerminal(mock)

    await terminal.close()

    expect(mock.reapScripts).toHaveLength(2)
    expect(mock.reapScripts[0]).toContain('kill -HUP')
    expect(mock.reapScripts[1]).toContain('kill -KILL')
  }, 10000)

  test('reaps as the session user, so a non-root shell can be read and signalled', async () => {
    // The reap scans /proc/<pid>/environ; reading a process owned by a different uid needs
    // CAP_SYS_PTRACE, which a stock container's exec lacks. Running the reaper as the shell's
    // own user keeps both the environ read and the kill on the same-uid path.
    const mock = createContainerMock({runningSequence: [false]})
    const terminal = await openTerminal(mock, {user: 'app'})

    await terminal.close()

    const reapOptions = mock.execOptions.find(o => !o.Tty)
    expect(reapOptions.User).toBe('app')
  })

  test('reap script targets only this session and cannot match itself', async () => {
    const mock = createContainerMock()
    await openTerminal(mock)

    const tag = sessionOptions(mock).Env[0].split('=')[1]
    await new Promise(r => setImmediate(r))
    mock.sessionStream.end() // triggers close('exited')
    await new Promise(r => setTimeout(r, 20))

    const script = mock.reapScripts[0]
    expect(script).toContain(`ODAC_TTY_SESSION=${tag}`)
    // Single line: `done \n | sort` is a shell syntax error.
    expect(script).not.toContain('\n')
    // The reaping exec is untagged, so the sweep can never signal itself.
    const reapExec = mock.execOptions.find(o => !o.Tty)
    expect(reapExec.Env).toBeUndefined()
  })

  test('reap signals children before the shell, so no zombies are left behind', async () => {
    const mock = createContainerMock({runningSequence: [false]})
    const terminal = await openTerminal(mock)

    await terminal.close()

    const script = mock.reapScripts[0]
    // Highest pid first; the shell is the oldest and lowest, and gets hung up last so it
    // can wait() on the children it just outlived. Killing it first strands them on pid 1.
    expect(script).toContain('sort -rn')
    expect(script.indexOf('kill -HUP "$p"')).toBeLessThan(script.indexOf('kill -HUP "$shell"'))
    expect(script).toMatch(/kill -HUP "\$p".*sleep 1.*kill -HUP "\$shell"/)
  })

  test('the session tag is generated internally, never taken from options', async () => {
    const mock = createContainerMock()
    // A caller-supplied id would land inside a shell command run in the container.
    await openTerminal(mock, {id: '"; rm -rf /; #', tag: 'evil'})

    expect(sessionOptions(mock).Env[0]).toMatch(/^ODAC_TTY_SESSION=[a-f0-9]{32}$/)
  })

  test('fires onExit exactly once with the exit code, even on repeated close', async () => {
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    const terminal = await openTerminal(mock, {onExit})

    await terminal.close()
    await terminal.close()

    expect(onExit).toHaveBeenCalledTimes(1)
    expect(onExit).toHaveBeenCalledWith({reason: 'closed', exitCode: 0})
  })

  test('a shell that exits on its own closes the session as "exited"', async () => {
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    const terminal = await openTerminal(mock, {onExit})

    mock.sessionStream.end()
    await new Promise(r => setTimeout(r, 20))

    expect(onExit).toHaveBeenCalledWith({reason: 'exited', exitCode: 0})
    expect(terminal.closed).toBe(true)
  })

  test('survives a container that vanished before the reap', async () => {
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    const terminal = await openTerminal(mock, {onExit})

    mock.container.exec.mockRejectedValue(new Error('No such container'))

    await expect(terminal.close()).resolves.toBeUndefined()
    expect(onExit).toHaveBeenCalledTimes(1)
    expect(terminal.closed).toBe(true)
  })
})

describe('Terminal timeouts', () => {
  afterEach(() => jest.useRealTimers())

  test('closes an idle session and input pushes the deadline back', async () => {
    jest.useFakeTimers()
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    const terminal = await openTerminal(mock, {onExit, idleTimeout: 1000, maxLifetime: 0})

    await jest.advanceTimersByTimeAsync(900)
    terminal.write('x') // resets the idle timer
    await jest.advanceTimersByTimeAsync(900)
    expect(onExit).not.toHaveBeenCalled()

    await jest.advanceTimersByTimeAsync(200)
    expect(onExit).toHaveBeenCalledWith(expect.objectContaining({reason: 'idle'}))
  })

  test('output alone does not keep an abandoned session alive', async () => {
    jest.useFakeTimers()
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    await openTerminal(mock, {onExit, idleTimeout: 1000, maxLifetime: 0})

    await jest.advanceTimersByTimeAsync(600)
    mock.sessionStream.write('chatty process output')
    await jest.advanceTimersByTimeAsync(600)

    expect(onExit).toHaveBeenCalledWith(expect.objectContaining({reason: 'idle'}))
  })

  test('enforces a hard lifetime cap regardless of activity', async () => {
    jest.useFakeTimers()
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    const terminal = await openTerminal(mock, {onExit, idleTimeout: 1000, maxLifetime: 2500})

    // Steady input keeps resetting the idle timer (last reset at 2400 → would fire at 3400),
    // so the cap at 2500 is the only thing that can end this session.
    for (let i = 0; i < 3; i++) {
      await jest.advanceTimersByTimeAsync(800)
      terminal.write('x')
    }
    expect(onExit).not.toHaveBeenCalled()

    await jest.advanceTimersByTimeAsync(200)
    expect(onExit).toHaveBeenCalledWith(expect.objectContaining({reason: 'lifetime'}))
  })

  test('a timeout of 0 disables the timer', async () => {
    jest.useFakeTimers()
    const mock = createContainerMock({runningSequence: [false]})
    const onExit = jest.fn()
    await openTerminal(mock, {onExit, idleTimeout: 0, maxLifetime: 0})

    await jest.advanceTimersByTimeAsync(60 * 60 * 1000)
    expect(onExit).not.toHaveBeenCalled()
  })
})
