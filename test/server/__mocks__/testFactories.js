/**
 * Test data factory functions for consistent mock data generation
 * Provides builders for common data structures used across server tests
 */

/**
 * Factory for creating mock website configurations
 */
const createMockWebsiteConfig = (domain = 'example.com', overrides = {}) => {
  return {
    domain,
    path: `/var/odac/${domain}`,
    subdomain: ['www'],
    cert: {
      ssl: {
        key: `/etc/ssl/private/${domain}.key`,
        cert: `/etc/ssl/certs/${domain}.crt`,
        expiry: Date.now() + 86400000 * 30 // 30 days from now
      }
    },
    DNS: {
      A: [{name: domain, value: '127.0.0.1'}],
      AAAA: [{name: domain, value: '::1'}],
      CNAME: [{name: `www.${domain}`, value: domain}],
      MX: [{name: domain, value: `mail.${domain}`, priority: 10}],
      TXT: [{name: domain, value: 'v=spf1 mx ~all'}],
      NS: [
        {name: domain, value: `ns1.${domain}`},
        {name: domain, value: `ns2.${domain}`}
      ]
    },
    ...overrides
  }
}

/**
 * Factory for creating mock service configurations
 */
const createMockServiceConfig = (name = 'test-service.js', overrides = {}) => {
  return {
    id: Math.floor(Math.random() * 1000),
    name,
    file: `/path/to/${name}`,
    active: true,
    pid: Math.floor(Math.random() * 10000) + 1000,
    status: 'running',
    started: Date.now(),
    restarts: 0,
    errors: 0,
    ...overrides
  }
}

/**
 * Factory for creating mock mail account data
 */
const createMockMailAccount = (email = 'test@example.com', overrides = {}) => {
  const [localPart, domain] = email.split('@')
  return {
    email,
    localPart,
    domain,
    password: '$2b$10$hashedpasswordexample',
    created: Date.now(),
    lastLogin: null,
    ...overrides
  }
}

/**
 * Factory for creating mock DNS records
 */
const createMockDNSRecord = (type = 'A', name = 'example.com', value = '127.0.0.1', overrides = {}) => {
  const baseRecord = {
    name,
    type,
    value,
    ttl: 300
  }

  // Add type-specific properties
  switch (type) {
    case 'MX':
      baseRecord.priority = 10
      break
    case 'SOA':
      baseRecord.serial = Math.floor(Date.now() / 1000)
      baseRecord.refresh = 3600
      baseRecord.retry = 1800
      baseRecord.expire = 604800
      baseRecord.minimum = 86400
      break
    case 'SRV':
      baseRecord.priority = 10
      baseRecord.weight = 5
      baseRecord.port = 443
      break
  }

  return {
    ...baseRecord,
    ...overrides
  }
}

/**
 * Factory for creating mock SSL certificate data
 */
const createMockSSLCertificate = (domain = 'example.com', overrides = {}) => {
  return {
    domain,
    keyPath: `/etc/ssl/private/${domain}.key`,
    certPath: `/etc/ssl/certs/${domain}.crt`,
    chainPath: `/etc/ssl/certs/${domain}-chain.crt`,
    issued: Date.now() - 86400000 * 10, // 10 days ago
    expiry: Date.now() + 86400000 * 80, // 80 days from now
    issuer: "Let's Encrypt Authority X3",
    algorithm: 'RSA',
    keySize: 2048,
    ...overrides
  }
}

/**
 * Factory for creating mock HTTP request objects
 */
const createMockRequest = (url = '/', method = 'GET', overrides = {}) => {
  return {
    url,
    method,
    headers: {
      host: 'example.com',
      'user-agent': 'Mozilla/5.0 Test Browser',
      accept: 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
      ...overrides.headers
    },
    connection: {
      remoteAddress: '127.0.0.1',
      remotePort: 12345
    },
    body: '',
    query: {},
    params: {},
    ...overrides
  }
}

/**
 * Factory for creating mock HTTP response objects
 */
const createMockResponse = (overrides = {}) => {
  const response = {
    statusCode: 200,
    headers: {},
    headersSent: false,
    finished: false,
    setHeader: jest.fn((name, value) => {
      response.headers[name.toLowerCase()] = value
    }),
    getHeader: jest.fn(name => response.headers[name.toLowerCase()]),
    removeHeader: jest.fn(name => {
      delete response.headers[name.toLowerCase()]
    }),
    writeHead: jest.fn((statusCode, headers) => {
      response.statusCode = statusCode
      if (headers) {
        Object.entries(headers).forEach(([name, value]) => {
          response.setHeader(name, value)
        })
      }
      response.headersSent = true
    }),
    write: jest.fn(),
    end: jest.fn(data => {
      if (data) response.write(data)
      response.finished = true
    }),
    ...overrides
  }

  return response
}

/**
 * Factory for creating mock TCP socket objects
 */
const createMockSocket = (overrides = {}) => {
  return {
    remoteAddress: '127.0.0.1',
    remotePort: 12345,
    localAddress: '127.0.0.1',
    localPort: 1453,
    readable: true,
    writable: true,
    destroyed: false,
    write: jest.fn(),
    end: jest.fn(),
    destroy: jest.fn(() => {
      socket.destroyed = true
    }),
    on: jest.fn(),
    once: jest.fn(),
    emit: jest.fn(),
    removeListener: jest.fn(),
    removeAllListeners: jest.fn(),
    ...overrides
  }
}

/**
 * Factory for creating mock child process objects
 */
const createMockChildProcess = (pid = 12345, overrides = {}) => {
  return {
    pid,
    killed: false,
    exitCode: null,
    signalCode: null,
    connected: true,
    stdout: {
      on: jest.fn(),
      pipe: jest.fn(),
      readable: true
    },
    stderr: {
      on: jest.fn(),
      pipe: jest.fn(),
      readable: true
    },
    stdin: {
      write: jest.fn(),
      end: jest.fn(),
      writable: true
    },
    kill: jest.fn((signal = 'SIGTERM') => {
      process.killed = true
      process.signalCode = signal
      return true
    }),
    send: jest.fn(),
    disconnect: jest.fn(),
    on: jest.fn(),
    once: jest.fn(),
    emit: jest.fn(),
    removeListener: jest.fn(),
    removeAllListeners: jest.fn(),
    ...overrides
  }
}

/**
 * Factory for creating mock database result objects
 */
const createMockDatabaseResult = (rows = [], overrides = {}) => {
  return {
    rows,
    rowCount: rows.length,
    command: 'SELECT',
    fields: [],
    ...overrides
  }
}

/**
 * Factory for creating mock email message objects
 */
const createMockEmailMessage = (from = 'sender@example.com', to = 'recipient@example.com', overrides = {}) => {
  return {
    messageId: `<${Date.now()}.${Math.random()}@example.com>`,
    from: {address: from, name: ''},
    to: [{address: to, name: ''}],
    subject: 'Test Email',
    date: new Date(),
    headers: new Map([
      ['from', from],
      ['to', to],
      ['subject', 'Test Email'],
      ['date', new Date().toISOString()]
    ]),
    text: 'This is a test email message.',
    html: '<p>This is a test email message.</p>',
    attachments: [],
    ...overrides
  }
}

module.exports = {
  createMockWebsiteConfig,
  createMockServiceConfig,
  createMockMailAccount,
  createMockDNSRecord,
  createMockSSLCertificate,
  createMockRequest,
  createMockResponse,
  createMockSocket,
  createMockChildProcess,
  createMockDatabaseResult,
  createMockEmailMessage
}
