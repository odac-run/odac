const {log, error} = Odac.core('Log', false).init('Service')

const childProcess = require('child_process')
const fs = require('fs')
const os = require('os')
const path = require('path')

class Service {
  #services = []
  #watcher = {}
  #loaded = false
  #logs = {}
  #errs = {}
  #error_counts = {}
  #active = {}

  #get(id) {
    if (!this.#loaded && this.#services.length == 0) {
      this.#services = Odac.core('Config').config.services ?? []
      this.#loaded = true
    }
    for (const service of this.#services) {
      if (service.id == id || service.name == id || service.file == id) return service
    }
    return false
  }

  #add(file) {
    let name = path.basename(file)
    if (name.substr(-3) == '.js') name = name.substr(0, name.length - 3)
    let service = {
      id: this.#services.length,
      name: path.basename(file),
      file: file,
      active: true
    }
    this.#services.push(service)
    this.#services[service.id] = service
    Odac.core('Config').config.services = this.#services
    return true
  }

  #set(id, key, value) {
    let service = this.#get(id)
    let index = this.#services.indexOf(service)
    if (service) {
      if (typeof key == 'object') {
        for (const k in key) service[k] = key[k]
      } else {
        service[key] = value
      }
    } else {
      return false
    }
    this.#services[index] = service
    Odac.core('Config').config.services = this.#services
  }

  async check() {
    this.#services = Odac.core('Config').config.services ?? []
    for (const service of this.#services) {
      if (service.active) {
        if (!service.pid) {
          this.#run(service.id)
        } else {
          if (!this.#watcher[service.pid]) {
            log('Service %s (PID %s) is not running. Restarting...', service.name, service.pid)
            Odac.core('Process').stop(service.pid)
            this.#run(service.id)
            this.#set(service.id, 'pid', null)
          }
        }
      }
      if (this.#logs[service.id])
        fs.writeFile(os.homedir() + '/.odac/logs/' + service.name + '.log', this.#logs[service.id], 'utf8', function (err) {
          if (err) error(err)
        })
      if (this.#errs[service.id])
        fs.writeFile(os.homedir() + '/.odac/logs/' + service.name + '.err.log', this.#errs[service.id], 'utf8', function (err) {
          if (err) error(err)
        })
    }
  }

  async delete(id) {
    return new Promise(resolve => {
      let service = this.#get(id)
      if (service) {
        this.stop(service.id)
        this.#services = this.#services.filter(s => s.id != service.id)
        Odac.core('Config').config.services = this.#services
        return resolve(Odac.server('Api').result(true, __('Service %s deleted successfully.', service.name)))
      } else {
        return resolve(Odac.server('Api').result(false, __('Service %s not found.', id)))
      }
    })
  }

  async #run(id) {
    if (this.#active[id]) return
    this.#active[id] = true
    let service = this.#get(id)
    if (!service) return
    log('Service %s is not running. Starting...', service.name)
    if (this.#error_counts[id] > 10) {
      this.#active[id] = false
      log('Service %s has exceeded the maximum error limit. Please check the logs for more details.', service.name)
      return
    }
    if ((service.status == 'errored' || service.status == 'stopped') && Date.now() - service.updated < this.#error_counts[id] * 1000) {
      this.#active[id] = false
      log('Service %s is in a cooldown period.', service.name)
      return
    }
    log('Starting service %s...', service.name)
    this.#set(id, 'updated', Date.now())
    var child = childProcess.spawn('node', [service.file], {
      cwd: path.dirname(service.file)
    })
    let pid = child.pid
    child.stdout.on('data', data => {
      if (!this.#logs[id]) this.#logs[id] = ''
      this.#logs[id] +=
        '[LOG][' +
        Date.now() +
        '] ' +
        data
          .toString()
          .trim()
          .split('\n')
          .join('\n[LOG][' + Date.now() + '] ') +
        '\n'
      if (this.#logs[id].length > 1000000) this.#logs[id] = this.#logs[id].substr(this.#logs[id].length - 1000000)
    })
    child.stderr.on('data', data => {
      if (!this.#errs[id]) this.#errs[id] = ''
      this.#logs[id] +=
        '[ERR][' +
        Date.now() +
        '] ' +
        data
          .toString()
          .trim()
          .split('\n')
          .join('\n[ERR][' + Date.now() + '] ') +
        '\n'
      this.#errs[id] += data.toString()
      if (this.#errs[id].length > 1000000) this.#errs[id] = this.#errs[id].substr(this.#errs[id].length - 1000000)
      this.#set(id, {
        status: 'errored',
        updated: Date.now()
      })
    })
    child.on('exit', () => {
      if (this.#get(service.id).status == 'running') {
        this.#set(id, {
          pid: null,
          started: null,
          status: 'stopped',
          updated: Date.now()
        })
      }
      this.#watcher[pid] = false
      this.#error_counts[id] = this.#error_counts[id] ?? 0
      this.#error_counts[id]++
      this.#active[id] = false
    })
    this.#set(id, {
      active: true,
      pid: pid,
      started: Date.now(),
      status: 'running'
    })
    this.#watcher[pid] = true
  }

  async init() {
    log('Initializing services...')
    this.#services = Odac.core('Config').config.services ?? []
    for (const service of this.#services) {
      fs.readFile(os.homedir() + '/.odac/logs/' + service.name + '.log', 'utf8', (err, data) => {
        if (!err) this.#logs[service.id] = data.toString()
      })
    }
    this.#loaded = true
  }

  async start(file) {
    return new Promise(resolve => {
      if (file && file.length > 0) {
        file = path.resolve(file)
        if (fs.existsSync(file)) {
          if (!this.#get(file)) {
            this.#add(file)
            return resolve(Odac.server('Api').result(true, __('Service %s added successfully.', file)))
          } else {
            return resolve(Odac.server('Api').result(false, __('Service %s already exists.', file)))
          }
        } else {
          return resolve(Odac.server('Api').result(false, __('Service file %s not found.', file)))
        }
      } else {
        return resolve(Odac.server('Api').result(false, __('Service file not specified.')))
      }
    })
  }

  stop(id) {
    let service = this.#get(id)
    if (service) {
      if (service.pid) {
        Odac.core('Process').stop(service.pid)
        this.#set(id, 'pid', null)
        this.#set(id, 'started', null)
        this.#set(id, 'active', false)
      } else {
        log(__('Service %s is not running.', id))
      }
    } else {
      log(__('Service %s not found.', id))
    }
  }

  async status() {
    let services = Odac.core('Config').config.services ?? []
    for (const service of services) {
      if (service.status == 'running') {
        var uptime = Date.now() - service.started
        let seconds = Math.floor(uptime / 1000)
        let minutes = Math.floor(seconds / 60)
        let hours = Math.floor(minutes / 60)
        let days = Math.floor(hours / 24)
        seconds %= 60
        minutes %= 60
        hours %= 24
        let uptimeString = ''
        if (days) uptimeString += days + 'd '
        if (hours) uptimeString += hours + 'h '
        if (minutes) uptimeString += minutes + 'm '
        if (seconds) uptimeString += seconds + 's'
        service.uptime = uptimeString
      }
    }
    return services
  }
}

module.exports = new Service()
