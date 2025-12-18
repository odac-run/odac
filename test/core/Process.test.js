const Process = require('../../core/Process')
const findProcess = require('find-process')

// Note: jest.mock is automatically hoisted.
// Since Process.js uses `require('find-process').default`, jest will automatically
// create a mock with a `default` property that is a jest.fn().
jest.mock('find-process')
const processKillSpy = jest.spyOn(process, 'kill').mockImplementation(() => {})

global.Odac = {
  core: () => ({
    config: {}
  })
}

describe('Process', () => {
  let proc

  beforeEach(() => {
    proc = new Process()
    // The mock function is on the .default property of the module mock
    findProcess.default.mockClear()
    processKillSpy.mockClear()
    global.Odac.core = () => ({config: {}})
  })

  it('should be defined', () => {
    expect(proc).toBeDefined()
  })

  describe('stop(pid)', () => {
    it('should kill a "node" process with the given pid', async () => {
      const pid = 123
      findProcess.default.mockResolvedValue([{pid: pid, name: 'node'}])

      await proc.stop(pid)

      expect(findProcess.default).toHaveBeenCalledWith('pid', pid)
      expect(processKillSpy).toHaveBeenCalledWith(pid, 'SIGTERM')
    })

    it('should not kill a process if it is not a "node" process', async () => {
      const pid = 456
      findProcess.default.mockResolvedValue([{pid: pid, name: 'other-process'}])

      await proc.stop(pid)

      expect(findProcess.default).toHaveBeenCalledWith('pid', pid)
      expect(processKillSpy).not.toHaveBeenCalled()
    })

    it('should not try to kill a process if no process is found', async () => {
      const pid = 789
      findProcess.default.mockResolvedValue([])

      await proc.stop(pid)

      expect(findProcess.default).toHaveBeenCalledWith('pid', pid)
      expect(processKillSpy).not.toHaveBeenCalled()
    })

    it('should resolve even if find-process throws an error', async () => {
      const pid = 101
      findProcess.default.mockRejectedValue(new Error('find-process failed'))

      await expect(proc.stop(pid)).resolves.toBeUndefined()
      expect(processKillSpy).not.toHaveBeenCalled()
    })
  })

  describe('stopAll()', () => {
    let stopSpy

    beforeEach(() => {
      // It's safer to store the spy in a variable and restore it from there.
      stopSpy = jest.spyOn(proc, 'stop').mockResolvedValue(undefined)
    })

    afterEach(() => {
      stopSpy.mockRestore()
    })

    it('should call stop for all configured pids', async () => {
      global.Odac.core = () => ({
        config: {
          server: {
            watchdog: 100,
            pid: 200
          },
          websites: {
            'example.com': {pid: 301},
            'test.com': {pid: 302}
          },
          services: [
            {name: 'service1', pid: 401},
            {name: 'service2', pid: 402}
          ]
        }
      })

      await proc.stopAll()

      expect(stopSpy).toHaveBeenCalledTimes(6)
      expect(stopSpy).toHaveBeenCalledWith(100)
      expect(stopSpy).toHaveBeenCalledWith(200)
      expect(stopSpy).toHaveBeenCalledWith(301)
      expect(stopSpy).toHaveBeenCalledWith(302)
      expect(stopSpy).toHaveBeenCalledWith(401)
      expect(stopSpy).toHaveBeenCalledWith(402)
    })

    it('should handle partial configurations gracefully', async () => {
      global.Odac.core = () => ({
        config: {
          server: {
            pid: 200
          },
          websites: {
            'example.com': {pid: 301}
          },
          services: []
        }
      })

      await proc.stopAll()

      expect(stopSpy).toHaveBeenCalledTimes(2)
      expect(stopSpy).toHaveBeenCalledWith(200)
      expect(stopSpy).toHaveBeenCalledWith(301)
    })

    it('should not call stop if no pids are configured', async () => {
      global.Odac.core = () => ({
        config: {
          server: {},
          websites: {},
          services: []
        }
      })

      await proc.stopAll()

      expect(stopSpy).not.toHaveBeenCalled()
    })

    it('should handle missing top-level config keys', async () => {
      global.Odac.core = () => ({
        config: {}
      })

      await proc.stopAll()

      expect(stopSpy).not.toHaveBeenCalled()
    })
  })
})
