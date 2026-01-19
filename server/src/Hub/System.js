const os = require('os')
const fs = require('fs')

class System {
  #lastNetworkStats = null
  #lastNetworkTime = null
  #lastCpuStats = null

  getStatus() {
    const serverStarted = Odac.core('Config').config.server.started
    const odacUptime = serverStarted ? Math.floor((Date.now() - serverStarted) / 1000) : 0

    return {
      cpu: this.getCpuUsage(),
      memory: this.getMemoryUsage(),
      disk: this.getDiskUsage(),
      network: this.getNetworkUsage(),
      services: this.getServicesInfo(),
      uptime: odacUptime,
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      node: process.version
    }
  }

  getServicesInfo() {
    try {
      const config = Odac.core('Config').config

      return {
        websites: config.websites ? Object.keys(config.websites).length : 0,
        apps: config.apps ? config.apps.length : 0,
        mail: config.mail?.accounts ? Object.keys(config.mail.accounts).length : 0
      }
    } catch {
      return {websites: 0, apps: 0, mail: 0}
    }
  }

  getMemoryUsage() {
    const totalMem = os.totalmem()

    if (os.platform() === 'darwin') {
      try {
        const {execSync} = require('child_process')
        const output = execSync('vm_stat', {encoding: 'utf8'})
        const stats = this.#parseDarwinMemory(output)
        return {used: stats.used, total: totalMem}
      } catch {
        // Fall through to default
      }
    }

    return {
      used: totalMem - os.freemem(),
      total: totalMem
    }
  }

  #parseDarwinMemory(output) {
    const lines = output.split('\n')
    let pageSize = 4096
    let pagesActive = 0
    let pagesWired = 0
    let pagesCompressed = 0

    for (const line of lines) {
      if (line.includes('page size of')) {
        pageSize = parseInt(line.match(/(\d+)/)[1])
      } else if (line.includes('Pages active')) {
        pagesActive = parseInt(line.match(/:\s*(\d+)/)[1])
      } else if (line.includes('Pages wired down')) {
        pagesWired = parseInt(line.match(/:\s*(\d+)/)[1])
      } else if (line.includes('Pages occupied by compressor')) {
        pagesCompressed = parseInt(line.match(/:\s*(\d+)/)[1])
      }
    }

    return {used: (pagesActive + pagesWired + pagesCompressed) * pageSize}
  }

  getDiskUsage() {
    try {
      const {execSync} = require('child_process')
      const platform = os.platform()

      if (platform === 'win32') {
        return this.#parseWindowsDisk(execSync('wmic logicaldisk get size,freespace,caption', {encoding: 'utf8'}))
      }

      return this.#parseUnixDisk(execSync("df -k / | tail -1 | awk '{print $2,$3}'", {encoding: 'utf8'}))
    } catch {
      return {used: 0, total: 0}
    }
  }

  #parseWindowsDisk(output) {
    const lines = output.trim().split('\n')
    if (lines.length > 1) {
      const parts = lines[1].trim().split(/\s+/)
      const free = parseInt(parts[1]) || 0
      const total = parseInt(parts[2]) || 0
      return {used: total - free, total}
    }
    return {used: 0, total: 0}
  }

  #parseUnixDisk(output) {
    const parts = output.trim().split(/\s+/)
    const total = parseInt(parts[0]) * 1024
    const used = parseInt(parts[1]) * 1024
    return {used, total}
  }

  getNetworkUsage() {
    try {
      const currentStats = this.#getCurrentNetworkStats()
      const now = Date.now()

      if (this.#lastNetworkStats && this.#lastNetworkTime) {
        const timeDiff = (now - this.#lastNetworkTime) / 1000
        const receivedDiff = currentStats.received - this.#lastNetworkStats.received
        const sentDiff = currentStats.sent - this.#lastNetworkStats.sent

        if (receivedDiff < 0 || sentDiff < 0 || timeDiff <= 0) {
          this.#lastNetworkStats = currentStats
          this.#lastNetworkTime = now
          return {download: 0, upload: 0}
        }

        const bandwidth = {
          download: Math.max(0, Math.round(receivedDiff / timeDiff)),
          upload: Math.max(0, Math.round(sentDiff / timeDiff))
        }

        this.#lastNetworkStats = currentStats
        this.#lastNetworkTime = now
        return bandwidth
      }

      this.#lastNetworkStats = currentStats
      this.#lastNetworkTime = now
      return {download: 0, upload: 0}
    } catch {
      return {download: 0, upload: 0}
    }
  }

  #getCurrentNetworkStats() {
    const {execSync} = require('child_process')
    const platform = os.platform()
    let command

    if (platform === 'win32') {
      command = 'netstat -e'
    } else if (platform === 'darwin') {
      command = "netstat -ib | grep -e 'en0' | head -1 | awk '{print $7,$10}'"
    } else {
      command = "cat /proc/net/dev | grep -E 'eth0|ens|enp' | head -1 | awk '{print $2,$10}'"
    }

    const output = execSync(command, {encoding: 'utf8', timeout: 5000})
    return this.#parseNetworkOutput(output, platform)
  }

  #parseNetworkOutput(output, platform) {
    if (platform === 'win32') {
      for (const line of output.split('\n')) {
        if (line.includes('Bytes')) {
          const parts = line.trim().split(/\s+/)
          return {
            received: parseInt(parts[1]) || 0,
            sent: parseInt(parts[2]) || 0
          }
        }
      }
      return {received: 0, sent: 0}
    }

    const parts = output.trim().split(/\s+/)
    return {
      received: parseInt(parts[0]) || 0,
      sent: parseInt(parts[1]) || 0
    }
  }

  getCpuUsage() {
    const cpus = os.cpus()
    let totalIdle = 0
    let totalTick = 0

    for (const cpu of cpus) {
      for (const type in cpu.times) {
        totalTick += cpu.times[type]
      }
      totalIdle += cpu.times.idle
    }

    const currentStats = {idle: totalIdle, total: totalTick}

    if (!this.#lastCpuStats) {
      this.#lastCpuStats = currentStats
      return 0
    }

    const idleDiff = currentStats.idle - this.#lastCpuStats.idle
    const totalDiff = currentStats.total - this.#lastCpuStats.total

    if (idleDiff < 0 || totalDiff <= 0) {
      this.#lastCpuStats = currentStats
      return 0
    }

    const usage = 100 - ~~((100 * idleDiff) / totalDiff)
    this.#lastCpuStats = currentStats

    return Math.max(0, Math.min(100, usage))
  }

  getLinuxDistro() {
    if (os.platform() !== 'linux') {
      return null
    }

    try {
      const osRelease = fs.readFileSync('/etc/os-release', 'utf8')
      const distro = {}

      for (const line of osRelease.split('\n')) {
        const [key, value] = line.split('=')
        if (key && value) {
          distro[key] = value.replace(/"/g, '')
        }
      }

      return {
        name: distro.NAME || distro.ID || 'Unknown',
        version: distro.VERSION_ID || distro.VERSION || 'Unknown',
        id: distro.ID || 'unknown'
      }
    } catch {
      return null
    }
  }

  getSystemInfo() {
    const cpus = os.cpus()
    const info = {
      hostname: os.hostname(),
      platform: os.platform(),
      arch: os.arch(),
      release: os.release(),
      uptime: os.uptime(),
      load: os.loadavg(),
      memory: {
        total: Math.floor(os.totalmem() / 1024),
        free: Math.floor(os.freemem() / 1024)
      },
      cpu: {
        count: cpus.length,
        model: cpus.length > 0 ? cpus[0].model : 'unknown'
      },
      container_engine: Odac.server('Container').available
    }

    if (os.platform() === 'linux') {
      const distro = this.getLinuxDistro()
      if (distro?.name) {
        info.release = `${distro.name} ${distro.version}`
      }
    }

    return info
  }
}

module.exports = new System()
