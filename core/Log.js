class Log {
  #cliMode = false

  constructor() {
    // Detect if we're running in CLI mode
    // CLI mode is when the main module is in cli/ or bin/ directory
    if (require.main && require.main.filename) {
      const mainFile = require.main.filename
      this.#cliMode = mainFile.includes('/cli/') || mainFile.includes('/bin/')
    }
  }

  init(...arg) {
    this.module = '[' + arg.join('][') + '] '
    return {
      error: this.error.bind(this),
      log: this.log.bind(this),
      warn: this.warn.bind(this)
    }
  }

  #sanitize(arg) {
    if (!arg || typeof arg !== 'object') return arg

    try {
      // Handle array recursion
      if (Array.isArray(arg)) {
        return arg.map(item => this.#sanitize(item))
      }

      // Simple handling for common types to avoid destroying them
      if (arg instanceof Date || arg instanceof RegExp || arg instanceof Error) return arg

      // Shallow copy for objects to avoid mutation
      const copy = {...arg}
      const sensitive = ['token', 'password', 'secret', 'key', 'auth']

      if (copy.env) {
        copy.env = '{ ...redacted... }'
      }

      for (const key of Object.keys(copy)) {
        if (sensitive.some(s => key.toLowerCase().includes(s))) {
          copy[key] = '***'
        } else if (typeof copy[key] === 'object') {
          // Recurse for nested objects (limited depth implicitly by stack, but practically okay for configs)
          copy[key] = this.#sanitize(copy[key])
        }
      }
      return copy
    } catch {
      return arg
    }
  }

  error(...arg) {
    // Always show errors, even in CLI mode
    const cleanArgs = arg.map(a => this.#sanitize(a))
    console.error(this.module, ...cleanArgs)
  }

  log(...arg) {
    // Suppress logs in CLI mode to avoid breaking the interface
    if (this.#cliMode) return

    if (!arg.length) return this

    let cleanArgs = arg.map(a => this.#sanitize(a))

    if (typeof cleanArgs[0] === 'string' && cleanArgs[0].includes('%s')) {
      let message = cleanArgs.shift()
      while (message.includes('%s') && cleanArgs.length > 0) {
        message = message.replace('%s', cleanArgs.shift())
      }
      message = message.replace(/%s/g, '')
      cleanArgs.unshift(message)
    }
    console.log(this.module, ...cleanArgs)
  }

  warn(...arg) {
    // Always show warnings, even in CLI mode
    const cleanArgs = arg.map(a => this.#sanitize(a))
    console.warn(this.module, ...cleanArgs)
  }
}

module.exports = Log
