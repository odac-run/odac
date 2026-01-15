// eslint-disable-next-line no-redeclare
class Odac {
  constructor() {
    this._registry = new Map()
    this._singletons = new Map()
    process.stdout.on('error', err => {
      if (err.code === 'EPIPE') return
      throw err
    })
    process.stderr.on('error', err => {
      if (err.code === 'EPIPE') return
      throw err
    })

    process.on('uncaughtException', err => {
      if (err.code === 'ECONNRESET' || err.code === 'EPIPE') {
        // Ignore network reset errors to prevent server crash
        // These can happen when a TLS stream is closed abruptly
        if (process.env.ODAC_DEBUG) {
          console.error(`[Ignored] Uncaught network error: ${err.code}`, err)
        }
        return
      }

      console.error('Uncaught Exception:', err)
      process.exit(1)
    })
  }

  #instantiate(value) {
    if (typeof value === 'function') return new value()
    return value
  }

  #register(key, value, singleton = true) {
    this._registry.set(key, {value, singleton})
  }

  #resolve(key, requestedSingleton = null) {
    const entry = this._registry.get(key)
    if (!entry) throw new Error(`Odac: '${key}' not found`)

    const useSingleton = requestedSingleton !== null ? requestedSingleton : entry.singleton

    if (useSingleton) {
      if (!this._singletons.has(key)) {
        const instance = this.#instantiate(entry.value)
        if (instance && typeof instance.init === 'function') {
          instance.init()
        }
        this._singletons.set(key, instance)
      }
      return this._singletons.get(key)
    }

    const instance = this.#instantiate(entry.value)
    if (instance && typeof instance.init === 'function') {
      instance.init()
    }
    return instance
  }

  core(name, singleton = true) {
    const key = `core:${name}`
    if (!this._registry.has(key)) {
      const modPath = `../core/${name}`
      let Mod = require(modPath)
      this.#register(key, Mod, singleton)
    }

    return this.#resolve(key, singleton)
  }

  cli(name, singleton = true) {
    const key = `cli:${name}`
    if (!this._registry.has(key)) {
      const modPath = `../cli/src/${name}`
      const Mod = require(modPath)
      this.#register(key, Mod, singleton)
    }
    return this.#resolve(key, singleton)
  }

  server(name, singleton = true) {
    const key = `server:${name}`
    if (!this._registry.has(key)) {
      const modPath = `../server/src/${name}`
      const Mod = require(modPath)
      this.#register(key, Mod, singleton)
    }
    return this.#resolve(key, singleton)
  }

  watchdog(name, singleton = true) {
    const key = `watchdog:${name}`
    if (!this._registry.has(key)) {
      const modPath = `../watchdog/src/${name}`
      const Mod = require(modPath)
      this.#register(key, Mod, singleton)
    }
    return this.#resolve(key, singleton)
  }
}

if (!global.Odac) {
  global.Odac = new Odac()
  global.__ = (...args) => global.Odac.core('Lang').get(...args)
}
