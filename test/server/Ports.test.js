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

  describe('normalize()', () => {
    test('stamps the sentinel onto legacy entries in place', () => {
      const ports = [{container: 3000}]
      const returned = Ports.normalize(ports)

      expect(returned).toBe(ports)
      expect(ports[0]).toEqual({host: 'proxy', container: 3000})
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
