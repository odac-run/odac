const nodeCrypto = require('crypto')

const {log, error} = Odac.core('Log', false).init('Container', 'Terminal')

const SESSION_ENV = 'ODAC_TTY_SESSION'

// Pick bash when the image ships it, fall back to sh. `exec` so the shell replaces
// this probe process and stays the tagged root of the session tree.
const DEFAULT_SHELL = ['/bin/sh', '-lc', 'command -v bash >/dev/null 2>&1 && exec bash || exec sh']

const DEFAULT_COLS = 80
const DEFAULT_ROWS = 24
const MAX_DIMENSION = 1000

const REAP_GRACE_MS = 1000
const DEFAULT_IDLE_TIMEOUT = 15 * 60 * 1000
const DEFAULT_MAX_LIFETIME = 4 * 60 * 60 * 1000

/**
 * Signals every process carrying our session tag.
 *
 * Docker exposes no "kill exec" API and `exec.inspect().Pid` lives in the daemon's PID
 * namespace, unreachable from the odac container. Instead each session tags its exec with
 * a unique env var; children inherit environ, so one /proc sweep finds the whole tree —
 * including background jobs, which a process-group kill would miss. The reaping exec is
 * itself untagged, so it can never signal itself.
 *
 * Children first, then the shell — with a pause in between.
 *
 * Only a parent can wait() away its dead children, and here that parent is the shell. Kill
 * the shell first and its dying children reparent onto the container's pid 1, which is the
 * app rather than an init and never reaps: one zombie per abandoned job, forever. Kill them
 * in the same breath as the shell and it exits before it can reap them. So: signal every
 * child (highest pid first — the shell is the oldest and therefore lowest), give it a moment
 * to collect them, and only then hang up the shell itself.
 *
 * The pause is skipped when the shell is alone, which is the common case.
 *
 * Must stay a single line: a multi-line `for … done` followed by a newline and `| sort`
 * parses as `done \n | sort`, a shell syntax error.
 */
const reapScript = (tag, signal) =>
  `pids=$(for p in /proc/[0-9]*; do pid=\${p##*/}; [ -r "$p/environ" ] || continue; ` +
  `if tr '\\0' '\\n' < "$p/environ" 2>/dev/null | grep -qx "${SESSION_ENV}=${tag}"; then echo "$pid"; fi; ` +
  `done | sort -rn); ` +
  `[ -n "$pids" ] || exit 0; ` +
  `shell=$(echo "$pids" | tail -n 1); others=$(echo "$pids" | sed '$d'); ` +
  `if [ -n "$others" ]; then for p in $others; do kill -${signal} "$p" 2>/dev/null; done; sleep 1; fi; ` +
  `kill -${signal} "$shell" 2>/dev/null; true`

const clampDimension = (value, fallback) => {
  const n = Math.trunc(Number(value))
  if (!Number.isFinite(n) || n < 1) return fallback
  return Math.min(n, MAX_DIMENSION)
}

/**
 * One interactive shell inside a running container, over a Docker exec with a real TTY.
 *
 * Lifecycle: open() → write()/resize() → close(). The session also closes itself when the
 * shell exits, when input goes idle, or when it outlives maxLifetime.
 */
class Terminal {
  #container
  #name
  #tag // secret, ours — never derived from caller input, it lands in a shell command
  #exec = null
  #stream = null

  #closing = false
  #closed = false

  #onData
  #onExit

  #cols
  #rows
  #command
  #user
  #env
  #workdir

  #idleTimeout
  #maxLifetime
  #idleTimer = null
  #lifeTimer = null

  #openedAt = null

  /**
   * @param {import('dockerode')} docker
   * @param {string} name - Container name
   * @param {Object} [options]
   * @param {function(Buffer): void} [options.onData] - Raw pty output
   * @param {function(Object): void} [options.onExit] - {reason, exitCode} — fires exactly once
   * @param {number} [options.cols=80]
   * @param {number} [options.rows=24]
   * @param {string[]} [options.command] - Overrides the bash-or-sh probe
   * @param {string} [options.user] - Exec as this user instead of the image default
   * @param {string[]} [options.env] - Extra `KEY=value` entries
   * @param {string} [options.workdir]
   * @param {number} [options.idleTimeout] - Since last *input*; 0 disables
   * @param {number} [options.maxLifetime] - Hard cap since open(); 0 disables
   */
  constructor(docker, name, options = {}) {
    this.#name = name
    this.#container = docker.getContainer(name)
    this.#tag = nodeCrypto.randomBytes(16).toString('hex')

    this.#onData = options.onData || (() => {})
    this.#onExit = options.onExit || (() => {})

    this.#cols = clampDimension(options.cols, DEFAULT_COLS)
    this.#rows = clampDimension(options.rows, DEFAULT_ROWS)
    this.#command = Array.isArray(options.command) && options.command.length ? options.command : DEFAULT_SHELL
    this.#user = options.user
    this.#env = Array.isArray(options.env) ? options.env : []
    this.#workdir = options.workdir

    this.#idleTimeout = options.idleTimeout ?? DEFAULT_IDLE_TIMEOUT
    this.#maxLifetime = options.maxLifetime ?? DEFAULT_MAX_LIFETIME
  }

  get closed() {
    return this.#closed
  }

  get container() {
    return this.#name
  }

  /**
   * Creates the exec, attaches, and starts the timers.
   * @returns {Promise<Terminal>}
   */
  async open() {
    if (this.#exec) throw new Error('Terminal already opened')

    const options = {
      Cmd: this.#command,
      AttachStdin: true,
      AttachStdout: true,
      AttachStderr: true,
      Tty: true,
      // Sizing the pty up front avoids a first paint at 0x0.
      ConsoleSize: [this.#rows, this.#cols],
      Env: [`${SESSION_ENV}=${this.#tag}`, ...this.#env]
    }
    if (this.#user) options.User = this.#user
    if (this.#workdir) options.WorkingDir = this.#workdir

    this.#exec = await this.#container.exec(options)

    // Tty must be repeated here. dockerode's ExecStart body defaults it to false and the
    // daemon uses *that* flag to choose raw-vs-multiplexed framing — omit it and the stream
    // arrives with 8-byte stream headers even though the exec really does own a pty.
    this.#stream = await this.#exec.start({hijack: true, stdin: true, Tty: true})

    this.#stream.on('data', chunk => this.#onData(chunk))
    this.#stream.on('end', () => this.close('exited'))
    this.#stream.on('error', err => {
      // destroy() during close() surfaces here; only real failures are worth reporting.
      if (!this.#closing && !this.#closed) {
        error('Terminal stream error on %s: %s', this.#name, err.message)
        this.close('error')
      }
    })

    this.#armIdleTimer()
    if (this.#maxLifetime > 0) {
      this.#lifeTimer = setTimeout(() => this.close('lifetime'), this.#maxLifetime)
    }

    this.#openedAt = Date.now()
    // Audit trail: who got a shell where, and as which user. Correlate with the Hub-side
    // session record for the human behind it.
    log('[AUDIT] terminal opened: container=%s user=%s', this.#name, this.#user || 'container-default')
    return this
  }

  /**
   * Forwards user input to the pty. Resets the idle timer — output alone must not, or a
   * chatty process would keep an abandoned session alive forever.
   * @param {Buffer|string} data
   * @returns {boolean}
   */
  write(data) {
    if (this.#closed || !this.#stream) return false
    this.#armIdleTimer()
    this.#stream.write(data)
    return true
  }

  /**
   * @param {number} cols
   * @param {number} rows
   */
  async resize(cols, rows) {
    if (this.#closed || !this.#exec) return false

    this.#cols = clampDimension(cols, this.#cols)
    this.#rows = clampDimension(rows, this.#rows)

    try {
      await this.#exec.resize({h: this.#rows, w: this.#cols})
      return true
    } catch (e) {
      // The shell can exit between our check and the call; that is not an error worth raising.
      log('Terminal resize failed on %s: %s', this.#name, e.message)
      return false
    }
  }

  /**
   * Tears the session down and reaps the process tree. Idempotent; onExit fires once.
   * @param {string} [reason] - exited | closed | idle | lifetime | error
   */
  async close(reason = 'closed') {
    if (this.#closed || this.#closing) return
    this.#closing = true

    this.#clearTimers()

    if (this.#stream) {
      this.#stream.destroy()
      this.#stream = null
    }

    // Closing the stream does NOT kill the exec: the shell and its children keep running and
    // exec.inspect().Running stays true. Reap explicitly, every time.
    await this.#reap()

    const exitCode = await this.#exitCode()

    this.#closed = true
    this.#closing = false

    const durationSec = this.#openedAt ? Math.round((Date.now() - this.#openedAt) / 1000) : 0
    log('[AUDIT] terminal closed: container=%s reason=%s exitCode=%s duration=%ss', this.#name, reason, exitCode, durationSec)
    this.#onExit({reason, exitCode})
  }

  /**
   * SIGHUP, then SIGKILL for whatever ignored it.
   *
   * SIGTERM is the wrong signal here: an interactive shell ignores it, so the children die
   * and the shell is left orphaned. SIGHUP is the real terminal-hangup signal — the shell
   * runs its traps, exits, and hangs up its jobs.
   */
  async #reap() {
    try {
      await this.#runInContainer(reapScript(this.#tag, 'HUP'))

      if (!(await this.#isExecRunning())) return

      await new Promise(resolve => setTimeout(resolve, REAP_GRACE_MS))
      if (!(await this.#isExecRunning())) return

      await this.#runInContainer(reapScript(this.#tag, 'KILL'))

      if (await this.#isExecRunning()) {
        error('Terminal reap incomplete on %s: session survived SIGKILL', this.#name)
      }
    } catch (e) {
      // A stopped or removed container took the session down with it; nothing left to reap.
      if (e.statusCode === 404 || e.statusCode === 409) {
        log('Terminal reap skipped on %s: container gone', this.#name)
        return
      }
      error('Terminal reap failed on %s: %s', this.#name, e.message)
    }
  }

  async #isExecRunning() {
    try {
      return (await this.#exec.inspect()).Running === true
    } catch {
      return false
    }
  }

  async #exitCode() {
    try {
      const data = await this.#exec.inspect()
      return data.Running ? null : data.ExitCode
    } catch {
      return null
    }
  }

  /** Fire-and-forget exec used for reaping. Untagged, so it never matches itself. */
  async #runInContainer(script) {
    const options = {
      Cmd: ['/bin/sh', '-c', script],
      AttachStdout: true,
      AttachStderr: true
    }

    // Reap as the same user the session runs as. The scan reads /proc/<pid>/environ, and
    // reading a process owned by a *different* uid needs CAP_SYS_PTRACE — which a stock
    // container's exec doesn't hold — so a root reaper can't even see a nobody shell's environ,
    // never mind signal it. Matching the user gives both the read (same-uid) and the kill.
    if (this.#user) options.User = this.#user

    const exec = await this.#container.exec(options)
    const stream = await exec.start({})

    await new Promise((resolve, reject) => {
      stream.on('data', () => {}) // drain, or 'end' never fires
      stream.on('end', resolve)
      stream.on('error', reject)
    })
  }

  #armIdleTimer() {
    if (this.#idleTimeout <= 0) return
    if (this.#idleTimer) clearTimeout(this.#idleTimer)
    this.#idleTimer = setTimeout(() => this.close('idle'), this.#idleTimeout)
  }

  #clearTimers() {
    if (this.#idleTimer) clearTimeout(this.#idleTimer)
    if (this.#lifeTimer) clearTimeout(this.#lifeTimer)
    this.#idleTimer = null
    this.#lifeTimer = null
  }
}

module.exports = Terminal
module.exports.SESSION_ENV = SESSION_ENV
module.exports.DEFAULT_SHELL = DEFAULT_SHELL
