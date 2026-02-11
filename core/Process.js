const findProcess = require('find-process').default

class Process {
  stop(pid) {
    return new Promise(resolve => {
      findProcess('pid', pid)
        .then(list => {
          for (const proc of list) if (proc.name == 'node') process.kill(proc.pid, 'SIGTERM')
        })
        .catch(() => {})
        .finally(() => {
          resolve()
        })
    })
  }

  async stopAll() {
    if (Odac.core('Config').config.server?.watchdog) await this.stop(Odac.core('Config').config.server.watchdog)
    if (Odac.core('Config').config.server?.pid) await this.stop(Odac.core('Config').config.server.pid)
    for (const domain of Object.keys(Odac.core('Config').config?.domains ?? {}))
      if (Odac.core('Config').config.domains[domain].pid) await this.stop(Odac.core('Config').config.domains[domain].pid)
    for (const service of Odac.core('Config').config.services ?? []) if (service.pid) await this.stop(service.pid)
  }
}

module.exports = Process
