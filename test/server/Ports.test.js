const Ports = require('../../server/src/Ports')

describe('Ports', () => {
  describe('PROXY sentinel', () => {
    test('exposes the sentinel host value', () => {
      expect(Ports.PROXY).toBe('proxy')
    })
  })

  describe('isProxy()', () => {
    test('treats the explicit sentinel as proxy-routed', () => {
      expect(Ports.isProxy({host: 'proxy', container: 3000})).toBe(true)
    })

    test('treats a legacy entry with no host as proxy-routed', () => {
      expect(Ports.isProxy({container: 3000})).toBe(true)
    })

    test('treats an empty host as proxy-routed', () => {
      expect(Ports.isProxy({host: '', container: 3000})).toBe(true)
    })

    test('rejects a published numeric host', () => {
      expect(Ports.isProxy({host: 8080, container: 3000})).toBe(false)
    })

    test('rejects a published numeric host given as a string', () => {
      expect(Ports.isProxy({host: '8080', container: 3000})).toBe(false)
    })

    test('is safe on nullish entries', () => {
      expect(Ports.isProxy(undefined)).toBe(false)
      expect(Ports.isProxy(null)).toBe(false)
    })
  })

  describe('isPublished()', () => {
    test('is true only for entries carrying a host binding', () => {
      expect(Ports.isPublished({host: 8080, container: 3000})).toBe(true)
      expect(Ports.isPublished({host: 'proxy', container: 3000})).toBe(false)
      expect(Ports.isPublished({container: 3000})).toBe(false)
    })

    test('is safe on nullish entries', () => {
      expect(Ports.isPublished(undefined)).toBe(false)
      expect(Ports.isPublished(null)).toBe(false)
    })
  })

  describe('isPublic()', () => {
    test('is true only when a published entry opts in', () => {
      expect(Ports.isPublic({host: 8080, container: 3000, public: true})).toBe(true)
      expect(Ports.isPublic({host: 8080, container: 3000, public: false})).toBe(false)
      expect(Ports.isPublic({host: 8080, container: 3000})).toBe(false)
    })

    test('never reports a proxy-routed entry as public', () => {
      expect(Ports.isPublic({host: 'proxy', container: 3000, public: true})).toBe(false)
      expect(Ports.isPublic({container: 3000, public: true})).toBe(false)
    })

    test('does not accept a truthy non-boolean as opt-in', () => {
      expect(Ports.isPublic({host: 8080, container: 3000, public: 'true'})).toBe(false)
      expect(Ports.isPublic({host: 8080, container: 3000, public: 1})).toBe(false)
    })

    test('is safe on nullish entries', () => {
      expect(Ports.isPublic(undefined)).toBe(false)
      expect(Ports.isPublic(null)).toBe(false)
    })
  })

  describe('bindIp()', () => {
    test('binds loopback by default', () => {
      expect(Ports.bindIp({host: 8080, container: 3000})).toBe('127.0.0.1')
      expect(Ports.bindIp({host: 8080, container: 3000, public: false})).toBe('127.0.0.1')
    })

    test('binds every interface for a public entry', () => {
      // Empty is Docker's own spelling of "0.0.0.0 and [::]"; naming the
      // families would break hosts without IPv6.
      expect(Ports.bindIp({host: 8080, container: 3000, public: true})).toBe('')
    })

    test('binds loopback for a proxy entry that wrongly claims to be public', () => {
      expect(Ports.bindIp({host: 'proxy', container: 3000, public: true})).toBe('127.0.0.1')
    })
  })

  describe('parsePublic()', () => {
    test('passes booleans through', () => {
      expect(Ports.parsePublic(true)).toBe(true)
      expect(Ports.parsePublic(false)).toBe(false)
    })

    test('treats an absent flag as not public', () => {
      expect(Ports.parsePublic(undefined)).toBe(false)
      expect(Ports.parsePublic(null)).toBe(false)
      expect(Ports.parsePublic('')).toBe(false)
    })

    test('accepts the stringified booleans a form or JSON payload may send', () => {
      expect(Ports.parsePublic('true')).toBe(true)
      expect(Ports.parsePublic('false')).toBe(false)
    })

    test('rejects values that are not booleans', () => {
      expect(Ports.parsePublic('yes')).toBeNull()
      expect(Ports.parsePublic(1)).toBeNull()
      expect(Ports.parsePublic(0)).toBeNull()
      expect(Ports.parsePublic({})).toBeNull()
    })
  })

  describe('isAuto()', () => {
    test('is true only for an entry ODAC guessed', () => {
      expect(Ports.isAuto({host: 'proxy', container: 3000, auto: true})).toBe(true)
      expect(Ports.isAuto({host: 'proxy', container: 3000})).toBe(false)
      expect(Ports.isAuto({host: 'proxy', container: 3000, auto: false})).toBe(false)
    })

    test('does not accept a truthy non-boolean as a marker', () => {
      expect(Ports.isAuto({host: 'proxy', container: 3000, auto: 'true'})).toBe(false)
    })

    test('is safe on nullish entries', () => {
      expect(Ports.isAuto(undefined)).toBe(false)
      expect(Ports.isAuto(null)).toBe(false)
    })
  })

  describe('primary()', () => {
    test('picks the proxy entry regardless of its position', () => {
      const published = {host: 9000, container: 9000}
      const proxy = {host: 'proxy', container: 3000}

      expect(Ports.primary([proxy, published])).toBe(proxy)
      expect(Ports.primary([published, proxy])).toBe(proxy)
    })

    test('picks a legacy entry with no host as the proxy entry', () => {
      const legacy = {container: 3000}
      expect(Ports.primary([{host: 9000, container: 9000}, legacy])).toBe(legacy)
    })

    test('falls back to the first entry when nothing is proxy-routed', () => {
      const first = {host: 8080, container: 3000}
      expect(Ports.primary([first, {host: 9090, container: 3000}])).toBe(first)
    })

    test('returns undefined when there are no entries', () => {
      expect(Ports.primary([])).toBeUndefined()
      expect(Ports.primary(undefined)).toBeUndefined()
      expect(Ports.primary(null)).toBeUndefined()
    })
  })

  describe('discovered()', () => {
    test('builds a proxy-routed entry marked as a guess', () => {
      expect(Ports.discovered(3000)).toEqual({host: 'proxy', container: 3000, auto: true})
    })

    test('builds an entry that isAuto and primary both recognize', () => {
      const entry = Ports.discovered(8080)

      expect(Ports.isAuto(entry)).toBe(true)
      expect(Ports.isProxy(entry)).toBe(true)
      expect(Ports.primary([{host: 9000, container: 9000}, entry])).toBe(entry)
    })
  })

  describe('normalize()', () => {
    test('stamps the sentinel onto legacy entries in place', () => {
      const ports = [{container: 3000}]
      const returned = Ports.normalize(ports)

      expect(returned).toBe(ports)
      expect(ports[0]).toEqual({host: 'proxy', container: 3000, auto: true})
    })

    test('marks a legacy proxy entry as a guess', () => {
      // No shipped surface could set a proxy container port by hand, so every
      // legacy one was inferred and stays correctable by auto-discovery.
      const ports = [{container: 3000}, {host: '', container: 4000}]
      Ports.normalize(ports)

      expect(Ports.isAuto(ports[0])).toBe(true)
      expect(Ports.isAuto(ports[1])).toBe(true)
    })

    test('does not mark an explicit proxy entry as a guess', () => {
      // Written by setPorts, so it is the user's choice and must survive restarts.
      const ports = [{host: 'proxy', container: 3000}]
      Ports.normalize(ports)

      expect(ports[0]).toEqual({host: 'proxy', container: 3000})
      expect(Ports.isAuto(ports[0])).toBe(false)
    })

    test('leaves published entries untouched', () => {
      const ports = [{host: 8080, container: 3000}]
      Ports.normalize(ports)

      expect(ports[0]).toEqual({host: 8080, container: 3000})
    })

    test('normalizes every entry, not just the first', () => {
      const ports = [{host: 8080, container: 3000}, {container: 9000}]
      Ports.normalize(ports)

      expect(ports[1].host).toBe('proxy')
    })

    test('tolerates a missing or non-array ports field', () => {
      expect(Ports.normalize(undefined)).toBeUndefined()
      expect(() => Ports.normalize(null)).not.toThrow()
    })

    test('skips nullish entries', () => {
      const ports = [null, {container: 3000}]
      expect(() => Ports.normalize(ports)).not.toThrow()
      expect(ports[1].host).toBe('proxy')
    })
  })
})
