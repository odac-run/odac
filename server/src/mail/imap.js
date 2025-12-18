const {log, error} = Odac.core('Log', false).init('Mail', 'IMAP')

// IMAP Constants
const CONSTANTS = {
  UIDVALIDITY: 123456789,
  MAX_AUTH_DATA_SIZE: 1024,
  MAX_COMMAND_SIZE: 8192,
  TIMEOUT_INTERVAL: 30000,
  DEFAULT_BOXES: ['INBOX', 'Drafts', 'Sent', 'Spam', 'Trash'],
  PERMANENT_FLAGS: ['\\Answered', '\\Flagged', '\\Deleted', '\\Seen', '\\Draft', '\\*'],
  CAPABILITIES: ['IMAP4rev1', 'AUTH=PLAIN', 'STARTTLS', 'IDLE'],
  MAX_CONNECTIONS_PER_IP: 10
}

// Rate limiting store
const rateLimitStore = new Map()

class Connection {
  #auth
  #actions = {
    APPEND: () => this.#append(),
    AUTHENTICATE: () => this.#authenticate(),
    CAPABILITY: () => this.#capability(),
    CLOSE: () => this.#close(),
    COPY: () => this.#copy(),
    CREATE: () => this.#create(),
    DELETE: () => this.#delete(),
    EXAMINE: () => this.#examine(),
    EXPUNGE: () => this.#expunge(),
    FETCH: () => this.#fetch(),
    IDLE: () => this.#idle(),
    LIST: () => this.#list(),
    LSUB: () => this.#lsub(),
    LOGIN: () => this.#login(),
    LOGOUT: () => this.#logout(),
    NOOP: () => this.#noop(),
    RENAME: () => this.#rename(),
    SEARCH: () => this.#search(),
    SELECT: () => this.#select(),
    STARTTLS: () => this.#starttls(),
    STATUS: () => this.#status(),
    STORE: () => this.#store()
  }
  #box = 'INBOX'
  #commands
  #end = false
  #idleInterval
  #options
  #request
  #socket
  #timeout
  #wait = false

  constructor(socket, self) {
    this.#socket = socket
    this.#options = self.options
    this.#setupRateLimit()
    this.#setupTimeout()
  }

  #setupRateLimit() {
    const clientIP = this.#getClientIP()
    const now = Date.now()

    if (!rateLimitStore.has(clientIP)) {
      rateLimitStore.set(clientIP, {count: 1, firstRequest: now})
    } else {
      const clientData = rateLimitStore.get(clientIP)
      if (now - clientData.firstRequest > 60000) {
        // Reset after 1 minute
        rateLimitStore.set(clientIP, {count: 1, firstRequest: now})
      } else {
        clientData.count++
        if (clientData.count > CONSTANTS.MAX_CONNECTIONS_PER_IP) {
          this.#socket.end()
          return
        }
      }
    }
  }

  #setupTimeout() {
    this.#timeout = setTimeout(() => {
      if (!this.#end && this.#socket && !this.#socket.destroyed) {
        try {
          this.#socket.write('* BYE Server timeout\r\n')
          this.#end = true
          this.#socket.end()
        } catch (timeoutError) {
          // Socket might already be closed, just cleanup
          error('Timeout cleanup error:', timeoutError.message)
          this.#cleanup()
        }
      }
    }, CONSTANTS.TIMEOUT_INTERVAL)
  }

  #getClientIP() {
    return this.#socket.remoteAddress?.replace('::ffff:', '') || '0.0.0.0'
  }

  #authenticate() {
    try {
      const authMechanism = this.#commands[2]?.toUpperCase()
      log('Authenticate request from: ' + this.#getClientIP() + ' using mechanism: ' + authMechanism)

      // Send appropriate challenge based on auth mechanism
      if (authMechanism === 'PLAIN') {
        this.#write('+ \r\n') // Empty challenge for PLAIN
      } else if (authMechanism === 'LOGIN') {
        this.#write('+ VXNlcm5hbWU6\r\n') // Base64 encoded "Username:"
      } else {
        this.#write('+ Ready for authentication\r\n')
      }

      this.#wait = true

      // Set a timeout for authentication data
      const authTimeout = setTimeout(() => {
        if (this.#wait) {
          this.#wait = false
          this.#write(`${this.#request.id} NO Authentication timeout\r\n`)
          log('Authentication timeout from: ' + this.#getClientIP())
        }
      }, 10000) // 10 second timeout

      this.#socket.once('data', data => {
        clearTimeout(authTimeout)
        this.#wait = false

        try {
          const dataStr = data.toString().trim()
          log('Authentication data received from ' + this.#getClientIP() + ': ' + dataStr.substring(0, 50) + '...')

          if (!dataStr || dataStr.length > CONSTANTS.MAX_AUTH_DATA_SIZE) {
            this.#write(`${this.#request.id} NO Authentication data too large\r\n`)
            log('Authentication data too large from: ' + this.#getClientIP())
            return
          }

          // Handle AUTHENTICATE CANCEL
          if (dataStr === '*') {
            this.#write(`${this.#request.id} BAD Authentication cancelled\r\n`)
            log('Authentication cancelled by client: ' + this.#getClientIP())
            return
          }

          if (authMechanism === 'PLAIN') {
            let auth
            try {
              auth = Buffer.from(dataStr, 'base64').toString('utf8').split('\0')
            } catch {
              this.#write(`${this.#request.id} NO Authentication data invalid\r\n`)
              log('Base64 decode failed from: ' + this.#getClientIP())
              return
            }

            if (auth.length !== 3 || !auth[1] || !auth[2]) {
              this.#write(`${this.#request.id} NO Authentication failed\r\n`)
              this.#auth = false
              log('Authentication failed - invalid format from: ' + this.#getClientIP())
              return
            }

            if (!this.#options.onAuth || typeof this.#options.onAuth !== 'function') {
              this.#write(`${this.#request.id} NO Authentication not available\r\n`)
              log('onAuth handler not available')
              return
            }

            this.#options.onAuth(
              {
                username: auth[1],
                password: auth[2]
              },
              {
                remoteAddress: this.#getClientIP()
              },
              err => {
                if (err) {
                  this.#write(`${this.#request.id} NO Authentication failed\r\n`)
                  log('Authentication failed for: ' + auth[1])
                  this.#auth = false
                } else {
                  this.#write(`${this.#request.id} OK Authentication successful\r\n`)
                  log('Authentication successful for: ' + auth[1])
                  this.#auth = auth[1]
                }
              }
            )
          } else if (authMechanism === 'LOGIN') {
            // LOGIN mechanism expects username first, then password in separate responses
            const username = Buffer.from(dataStr, 'base64').toString('utf8')
            this.#write('+ UGFzc3dvcmQ6\r\n') // Base64 encoded "Password:"

            this.#socket.once('data', passwordData => {
              const password = Buffer.from(passwordData.toString().trim(), 'base64').toString('utf8')

              if (!this.#options.onAuth || typeof this.#options.onAuth !== 'function') {
                this.#write(`${this.#request.id} NO Authentication not available\r\n`)
                return
              }

              this.#options.onAuth(
                {
                  username: username,
                  password: password
                },
                {
                  remoteAddress: this.#getClientIP()
                },
                err => {
                  if (err) {
                    this.#write(`${this.#request.id} NO Authentication failed\r\n`)
                    log('Authentication failed for: ' + username)
                    this.#auth = false
                  } else {
                    this.#write(`${this.#request.id} OK Authentication successful\r\n`)
                    log('Authentication successful for: ' + username)
                    this.#auth = username
                  }
                }
              )
            })
          } else {
            this.#write(`${this.#request.id} NO Unsupported authentication mechanism\r\n`)
            log('Unsupported auth mechanism: ' + authMechanism)
            this.#auth = false
          }
        } catch (authError) {
          error('Authentication error:', authError.message)
          this.#write(`${this.#request.id} NO Authentication failed\r\n`)
          this.#auth = false
        }
      })

      // Also listen for socket errors during authentication
      const errorHandler = err => {
        clearTimeout(authTimeout)
        this.#wait = false
        error('Socket error during authentication:', err.message)
        this.#write(`${this.#request.id} NO Authentication failed\r\n`)
        this.#auth = false
      }

      this.#socket.once('error', errorHandler)
      this.#socket.once('close', () => {
        clearTimeout(authTimeout)
        this.#wait = false
        log('Socket closed during authentication from: ' + this.#getClientIP())
      })
    } catch (err) {
      error('Authenticate method error:', err.message)
      this.#write(`${this.#request.id} NO Authentication failed\r\n`)
      this.#wait = false
    }
  }

  #safeParse(jsonString, defaultValue = null) {
    try {
      return JSON.parse(jsonString)
    } catch (parseError) {
      error('JSON parse error:', parseError.message)
      return defaultValue
    }
  }

  #append() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onAppend || typeof this.#options.onAppend != 'function')
      return this.#write(`${this.#request.id} NO APPEND failed\r\n`)
    let mailbox = this.#commands[2]
    let flags = this.#commands[3]
    let size = this.#commands[4]
    if (size.startsWith('{') && size.endsWith('}')) size = size.substr(1, size.length - 2)
    this.#write('+ Ready for literal data\r\n')
    this.#wait = true
    this.#socket.once('data', data => {
      this.#options.onAppend({address: this.#auth, mailbox: mailbox, flags: flags, message: data.toString()}, err => {
        if (err) return this.#write(`${this.#request.id} NO APPEND failed\r\n`)
        this.#write(`${this.#request.id} OK APPEND completed\r
`)
      })
      this.#wait = false
    })
  }

  #bad() {
    error('Unknown command', this.#request.action)
    this.#write(`${this.#request.id} BAD Unknown command\r\n`)
  }

  #capability() {
    this.#write(`* CAPABILITY ${CONSTANTS.CAPABILITIES.join(' ')}\r\n`)
    this.#write(`${this.#request.id} OK CAPABILITY completed\r\n`)
  }

  #close() {
    if (this.#box) this.#expunge()
    this.#write(`${this.#request.id} OK CLOSE completed\r\n`)
  }

  #create() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onCreate || typeof this.#options.onCreate != 'function')
      return this.#write(`${this.#request.id} NO CREATE failed\r\n`)
    let mailbox = this.#commands.slice(2).join(' ')
    this.#options.onCreate({address: this.#auth, mailbox: mailbox}, err => {
      if (err) return this.#write(`${this.#request.id} NO CREATE failed\r\n`)
      this.#write(`${this.#request.id} OK CREATE completed\r\n`)
    })
  }

  #delete() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onDelete || typeof this.#options.onDelete != 'function')
      return this.#write(`${this.#request.id} NO DELETE failed\r\n`)
    let mailbox = this.#commands.slice(2).join(' ')
    this.#options.onDelete({address: this.#auth, mailbox: mailbox}, err => {
      if (err) return this.#write(`${this.#request.id} NO DELETE failed\r\n`)
      this.#write(`${this.#request.id} OK DELETE completed\r\n`)
    })
  }

  #data(data) {
    try {
      log('Data received from: ' + this.#getClientIP(), data.toString().trim())
      if (this.#wait || !data || data.toString().trim().length === 0) {
        return
      }

      const dataStr = data.toString().trim()
      if (dataStr.length > CONSTANTS.MAX_COMMAND_SIZE) {
        this.#write(`* BAD Command too large\r\n`)
        return
      }

      this.#commands = dataStr.split(' ')
      this.#request = {}
      const dataParts = dataStr.split(' ')
      this.#request.id = dataParts.shift()
      this.#request.action = dataParts.filter(item => Object.keys(this.#actions).includes(item.toUpperCase())).join(' ')

      log('Incoming IMAP command: ' + this.#request.action)

      const index = dataParts.indexOf(this.#request.action)
      dataParts.splice(dataParts.indexOf(this.#request.action), 1)
      this.#request.action = this.#request.action.toUpperCase()

      if (dataParts.includes('UID') && dataParts.indexOf('UID') < index) {
        this.#request.uid = dataParts[dataParts.indexOf('UID') + 1]
        dataParts.splice(dataParts.indexOf('UID'), 2)
        if (!dataParts.includes('UID') && !dataParts.includes('(UID')) {
          dataParts.splice(0, 0, 'UID')
        }
      } else if (index === 0) {
        this.#request.uid = dataParts[0]
        dataParts.shift()
      }

      this.#request.requests = this.#export(dataParts)

      if (this.#actions[this.#request.action]) {
        this.#actions[this.#request.action]()
      } else {
        this.#bad()
      }
    } catch (err) {
      error('Data processing error:', err.message)
      this.#bad()
    }
  }

  #examine() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#options.onSelect || typeof this.#options.onSelect !== 'function') {
        return this.#write(`${this.#request.id} NO EXAMINE failed\r\n`)
      }

      this.#box = this.#commands[2]
      this.#options.onSelect(this.#auth, this.#options, data => {
        const flagsList = CONSTANTS.PERMANENT_FLAGS.join(' ')
        this.#write(`* FLAGS (${flagsList})\r\n`)
        this.#write(`* OK [PERMANENTFLAGS (${flagsList})] Flags permitted\r\n`)
        if (data.exists !== undefined) this.#write('* ' + data.exists + ' EXISTS\r\n')
        this.#write('* ' + (data.recent ?? data.exists ?? 0) + ' RECENT\r\n')
        if (data.unseen !== undefined) {
          this.#write('* OK [UNSEEN ' + data.unseen + '] Message ' + (data.unseen ?? 0) + ' is first unseen\r\n')
        }
        if (data.uidvalidity !== undefined) {
          this.#write('* OK [UIDVALIDITY ' + data.uidvalidity + '] UIDs valid\r\n')
        }
        if (data.uidnext !== undefined) {
          this.#write('* OK [UIDNEXT ' + data.uidnext + '] Predicted next UID\r\n')
        }
        this.#write(`${this.#request.id} OK [READ-ONLY] EXAMINE completed\r\n`)
      })
    } catch (err) {
      error('EXAMINE command failed:', err.message)
      this.#write(`${this.#request.id} NO EXAMINE failed\r\n`)
    }
  }

  #expunge() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onExpunge || typeof this.#options.onExpunge != 'function')
      return this.#write(`${this.#request.id} NO EXPUNGE failed\r\n`)
    this.#options.onExpunge({address: this.#auth, mailbox: this.#box}, (err, uids) => {
      if (err) return this.#write(`${this.#request.id} NO EXPUNGE failed\r\n`)
      for (let uid of uids) this.#write(`* ${uid} EXPUNGE\r\n`)
      this.#write(`${this.#request.id} OK EXPUNGE completed\r\n`)
    })
  }

  #export(data) {
    let result = []
    while (data.length) {
      let item = data.shift()
      let fields = []
      let index = data.indexOf(item)
      if (item.includes('[]')) item = item.split('[]')[0]
      if (item.startsWith('(') || item.startsWith('[')) item = item.substring(1)
      if (!data[index + 1]?.startsWith('(BODY')) {
        if (
          item.includes('[') ||
          item.includes('(') ||
          (data[index + 1] ?? '').startsWith('[') ||
          (data[index + 1] ?? '').startsWith('(')
        ) {
          let next = true
          if (item.includes('[') || item.includes('(')) {
            item = item.split(item.includes('[') ? '[' : '(')
            fields.push(item[1].split(']')[0])
            next = !item[1].includes(']') && !item[1].includes(')')
            item = item[0]
          }
          if (next)
            while (data.length && item[1] && !item[1].includes(']') && !item[1].includes(')')) {
              fields.push(data.shift())
            }
        }
      }
      while (item.endsWith(']') || item.endsWith(')')) item = item.substring(0, item.length - 1)
      let peek = item.includes('.')
      if (peek) {
        peek = item.split('.')[1]
        item = item.split('.')[0]
      }
      result.push({
        value: item,
        peek: peek,
        fields: this.#export(fields)
      })
    }
    return result
  }

  async #fetch() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#box) return this.#write(`${this.#request.id} NO Mailbox required\r\n`)
      if (!this.#options.onFetch || typeof this.#options.onFetch !== 'function') {
        return this.#write(`${this.#request.id} NO FETCH failed\r\n`)
      }

      const ids = this.#request.uid ? this.#request.uid.split(',') : ['ALL']
      for (const id of ids) {
        await new Promise(resolve => {
          this.#options.onFetch(
            {
              email: this.#auth,
              mailbox: this.#box,
              limit: id === 'ALL' ? null : id.includes(':') ? id.split(':') : [id, id]
            },
            this.#commands,
            data => {
              if (data === false) {
                this.#write(`${this.#request.id} NO FETCH failed\r\n`)
                return resolve()
              }
              for (const row of data) {
                this.#write('* ' + row.uid + ' FETCH (')
                this.#prepare(this.#request.requests, row)
                this.#write(')\r\n')
              }
              return resolve()
            }
          )
        })
      }
      this.#write(`${this.#request.id} OK FETCH completed\r\n`)
    } catch (err) {
      error('FETCH command failed:', err.message)
      this.#write(`${this.#request.id} NO FETCH failed\r\n`)
    }
  }

  #search() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#box) return this.#write(`${this.#request.id} NO Mailbox required\r\n`)
      if (!this.#options.onSearch || typeof this.#options.onSearch !== 'function') {
        return this.#write(`${this.#request.id} NO SEARCH not implemented\r\n`)
      }

      const criteria = this.#commands.slice(2)
      this.#options.onSearch(
        {
          address: this.#auth,
          mailbox: this.#box,
          criteria: criteria
        },
        (err, uids) => {
          if (err) return this.#write(`${this.#request.id} NO SEARCH failed\r\n`)
          this.#write(`* SEARCH ${uids.join(' ')}\r\n`)
          this.#write(`${this.#request.id} OK SEARCH completed\r\n`)
        }
      )
    } catch (err) {
      error('SEARCH command failed:', err.message)
      this.#write(`${this.#request.id} NO SEARCH failed\r\n`)
    }
  }

  #copy() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#box) return this.#write(`${this.#request.id} NO Mailbox required\r\n`)
      if (!this.#options.onCopy || typeof this.#options.onCopy !== 'function') {
        return this.#write(`${this.#request.id} NO COPY not implemented\r\n`)
      }

      const uids = this.#request.uid
      const targetMailbox = this.#commands[this.#commands.length - 1]

      this.#options.onCopy(
        {
          address: this.#auth,
          sourceMailbox: this.#box,
          targetMailbox: targetMailbox,
          uids: uids
        },
        err => {
          if (err) return this.#write(`${this.#request.id} NO COPY failed\r\n`)
          this.#write(`${this.#request.id} OK COPY completed\r\n`)
        }
      )
    } catch (err) {
      error('COPY command failed:', err.message)
      this.#write(`${this.#request.id} NO COPY failed\r\n`)
    }
  }

  #idle() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#box) return this.#write(`${this.#request.id} NO Mailbox required\r\n`)

      this.#write('+ idling\r\n')
      this.#wait = true

      // Event listener for IDLE state
      this.#idleInterval = setInterval(() => {
        if (this.#options.onIdle && typeof this.#options.onIdle === 'function') {
          this.#options.onIdle(
            {
              address: this.#auth,
              mailbox: this.#box
            },
            updates => {
              if (updates) {
                for (const update of updates) {
                  this.#write(`* ${update}\r\n`)
                }
              }
            }
          )
        }
      }, 5000)

      this.#socket.once('data', data => {
        const command = data.toString().trim().toUpperCase()
        if (command === 'DONE') {
          clearInterval(this.#idleInterval)
          this.#wait = false
          this.#write(`${this.#request.id} OK IDLE terminated\r\n`)
        }
      })
    } catch (err) {
      error('IDLE command failed:', err.message)
      this.#write(`${this.#request.id} NO IDLE failed\r\n`)
    }
  }

  #starttls() {
    try {
      if (this.#socket.encrypted) {
        return this.#write(`${this.#request.id} NO TLS already active\r\n`)
      }

      this.#write(`${this.#request.id} OK Begin TLS negotiation now\r\n`)

      // TLS upgrade implementation would go here
      // This is a placeholder for actual TLS implementation
      if (this.#options.onStartTLS && typeof this.#options.onStartTLS === 'function') {
        this.#options.onStartTLS(this.#socket, err => {
          if (err) {
            error('STARTTLS failed:', err.message)
            this.#socket.end()
          }
        })
      }
    } catch (err) {
      error('STARTTLS command failed:', err.message)
      this.#write(`${this.#request.id} NO STARTTLS failed\r\n`)
    }
  }

  #list() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onList || typeof this.#options.onList != 'function') return this.#write(`${this.#request.id} NO LIST failed\r\n`)
    this.#options.onList({address: this.#auth}, (err, boxes) => {
      if (err) return this.#write(`${this.#request.id} NO LIST failed\r\n`)
      for (let box of boxes) this.#write(`* LIST (\\HasNoChildren) "/" ${box}\r\n`)
      this.#write(`${this.#request.id} OK LIST completed\r\n`)
    })
  }

  listen() {
    try {
      this.#socket.on('data', data => {
        try {
          this.#data(data)
        } catch (err) {
          error('Data handling error:', err.message)
          this.#bad()
        }
      })

      this.#socket.on('end', () => {
        this.#end = true
        this.#cleanup()
        if (!this.#socket.destroyed) {
          this.#socket.end()
        }
      })

      this.#socket.on('error', err => {
        error('Socket error:', err.message)
        this.#end = true
        if (this.#options.onError) {
          this.#options.onError(err)
        }
        this.#cleanup()
      })

      this.#socket.on('close', () => {
        this.#end = true
        this.#cleanup()
      })
    } catch (err) {
      error('Listen setup error:', err.message)
      this.#cleanup()
    }
  }

  #cleanup() {
    try {
      // Set end flag first to prevent any further writes
      this.#end = true

      // Clear timeout
      if (this.#timeout) {
        clearTimeout(this.#timeout)
        this.#timeout = null
      }

      // Clear idle interval
      if (this.#idleInterval) {
        clearInterval(this.#idleInterval)
        this.#idleInterval = null
      }

      // Remove from rate limit store
      const clientIP = this.#getClientIP()
      if (rateLimitStore.has(clientIP)) {
        const clientData = rateLimitStore.get(clientIP)
        clientData.count--
        if (clientData.count <= 0) {
          rateLimitStore.delete(clientIP)
        }
      }

      // Remove all listeners safely
      if (this.#socket && !this.#socket.destroyed) {
        this.#socket.removeAllListeners()
      }
    } catch (cleanupError) {
      error('Cleanup error:', cleanupError.message)
    }
  }

  #lsub() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onLsub || typeof this.#options.onLsub != 'function') return this.#write(`${this.#request.id} NO LSUB failed\r\n`)
    this.#options.onLsub({address: this.#auth}, (err, boxes) => {
      if (err) return this.#write(`${this.#request.id} NO LSUB failed\r\n`)
      for (let box of boxes) this.#write(`* LSUB (\\HasNoChildren) "/" "${box}"\r\n`)
      this.#write(`${this.#request.id} OK LSUB completed\r\n`)
    })
  }

  #login() {
    if (this.#options.onAuth && typeof this.#options.onAuth == 'function') {
      if (this.#commands[2].startsWith('"') && this.#commands[2].endsWith('"'))
        this.#commands[2] = this.#commands[2].substr(1, this.#commands[2].length - 2)
      if (this.#commands[3].startsWith('"') && this.#commands[3].endsWith('"'))
        this.#commands[3] = this.#commands[3].substr(1, this.#commands[3].length - 2)
      this.#options.onAuth(
        {
          username: this.#commands[2],
          password: this.#commands[3]
        },
        this.#commands,
        err => {
          if (err) {
            this.#write(`${this.#request.id} NO Authentication failed\r\n`)
            this.#auth = false
          } else {
            this.#write(`${this.#request.id} OK Authentication successful\r\n`)
            this.#auth = this.#commands[2]
          }
        }
      )
    } else {
      this.#write(`${this.#request.id} NO Authentication failed\r\n`)
      this.#auth = false
    }
  }

  #logout() {
    try {
      if (!this.#end && this.#socket && !this.#socket.destroyed && !this.#socket.writableEnded) {
        this.#write('* BYE IMAP4rev1 Server logging out\r\n')
        this.#write(`${this.#request.id} OK LOGOUT completed\r\n`)
      }
      this.#end = true
      this.#cleanup()
      if (this.#socket && !this.#socket.destroyed) {
        this.#socket.end()
      }
    } catch (err) {
      error('Logout error:', err.message)
      this.#cleanup()
    }
  }

  #noop() {
    this.#write(`${this.#request.id} OK NOOP completed\r\n`)
  }

  #prepareBody(request, data, boundary) {
    let body = {keys: '', header: '', content: ''}
    for (let obj of request.fields.length ? request.fields : [{value: 'HEADER'}, {value: 'TEXT'}]) {
      let fields = obj.fields ? obj.fields.map(field => field.value.toLowerCase()) : []
      if (request.fields.length) body.keys += obj.value + (obj.peek ? '.' + obj.peek : '')
      if (fields.length > 0) body.keys += ' ('
      if (obj.value == 'HEADER') {
        for (let line of data.headerLines) {
          let include = true
          if (obj.peek)
            if (obj.peek == 'FIELDS') include = fields.includes(line.key)
            else if (obj.peek == 'FIELDS.NOT') include = !fields.includes(line.key)
          if (include) {
            if (fields.length > 0) body.keys += line.key + ' '
            if (line.key.toLowerCase() == 'content-type') {
              if (data.attachments.length > 0) {
                body.header += 'Content-Type: multipart/mixed; boundary="' + boundary + '"\r\n'
              } else if (data.html && data.html.length > 1 && data.text && data.text.length > 1) {
                body.header += 'Content-Type: multipart/alternative; boundary="' + boundary + '_alt"\r\n'
              } else if (!data.text || data.text.length < 1) {
                body.header += 'Content-Type: text/html; charset=utf-8\r\n'
              } else if (!data.html || data.html.length < 1) {
                body.header += 'Content-Type: text/plain; charset=utf-8\r\n'
              }
            } else body.header += line.line + '\r\n'
          }
        }
        if ((obj.peek ?? '') !== 'FIELDS.NOT') {
          for (let field of fields) {
            if (!data.headerLines.find(line => line.key == field)) {
              if (fields.length > 0) body.keys += field + ' '
              else body.header += field + ': \r\n'
            }
          }
        }
        body.header = body.header.trim()
        if (fields.length > 0) body.keys = body.keys.trim() + ')'
      } else if (obj.value == 'TEXT') {
        if (body.header.length) body.content += body.header + '\r\n\r\n'
        if (data.html.length > 1 || data.attachments.length) {
          if (data.attachments.length && data.html && data.html.length && data.text && data.text.length) {
            body.content += '--' + boundary + '\r\n'
            body.content += 'Content-Type: multipart/alternative; boundary="' + boundary + '_alt"\r\n'
          }
          if (data.text && data.text.length) {
            body.content += '\r\n--' + boundary + '_alt\r\n'
            body.content += 'Content-Type: text/plain; charset=utf-8\r\n'
            body.content += 'Content-Transfer-Encoding: quoted-printable\r\n\r\n'
            body.content += data.text
            body.content += '\r\n--' + boundary + '_alt\r\n'
          }
          if (data.html.length) {
            if (data.text && data.text.length) {
              body.content += 'Content-Type: text/html; charset=utf-8\r\n'
              body.content += 'Content-Transfer-Encoding: quoted-printable\r\n\r\n'
            }
            body.content += data.html
            if (data.text && data.text.length) body.content += '\r\n--' + boundary + '_alt--\r\n'
          }
          for (let attachment of data.attachments) {
            body.content += '\r\n--' + boundary + '\r\n'
            body.content += 'Content-Type: ' + attachment.contentType + '; name="' + attachment.filename + '"\r\n'
            body.content += 'Content-Transfer-Encoding: base64\r\n'
            body.content += 'Content-Disposition: attachment; filename="' + attachment.filename + '"\r\n\r\n'
            body.content += Buffer.from(attachment.content.data).toString('base64')
          }
          if (data.attachments.length) body.content += '--' + boundary + '--\r\n'
        } else body.content += data.text
      } else if (!isNaN(obj.value)) {
        obj.value = parseInt(obj.value)
        if (obj.value === 1 || (obj.value === 2 && !data.attachments.length)) {
          if (obj.peek === 2 || obj.value === 2) body.content += data.html
          else body.content += data.text
        } else if (obj.value > 1 && data.attachments[obj.value - 2])
          body.content += Buffer.from(data.attachments[obj.value - 2].content.data).toString('base64') + '\r\n'
      }
    }
    if (body.content == '') body.content = body.header
    body.content = body.content.replace(/\r\n/g, '\n')
    this.#write('BODY[' + body.keys + '] {' + Buffer.byteLength(body.content, 'utf8') + '}\r\n')
    this.#write(body.content)
  }

  #prepareBodyStructure(data, boundary) {
    let structure = ''
    if (data.text && data.text.length && data.html && data.html.length) structure += '('
    if (data.text && data.text.length)
      structure +=
        '("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "QUOTED-PRINTABLE" ' +
        Buffer.byteLength(data.text, 'utf8') +
        ' ' +
        data.text.split('\n').length +
        ' NIL NIL NIL NIL)'
    if (data.html && data.html.length)
      structure +=
        '("TEXT" "HTML"  ("CHARSET" "UTF-8") NIL NIL "QUOTED-PRINTABLE" ' +
        Buffer.byteLength(data.html, 'utf8') +
        ' ' +
        data.html.split('\n').length +
        ')'
    if (data.text && data.text.length && data.html && data.html.length)
      structure += ' "ALTERNATIVE" ("BOUNDARY" "' + boundary + '_alt") NIL NIL NIL'
    if (data.text && data.text.length && data.html && data.html.length) structure += ')'
    for (let attachment of data.attachments)
      structure +=
        '("APPLICATION" "' +
        attachment.contentType.split('/')[1].toUpperCase() +
        '" ("NAME" "' +
        attachment.filename +
        '") NIL NIL "BASE64" ' +
        Buffer.from(attachment.content.data).toString('base64').length +
        ' NIL ("ATTACHMENT" ("FILENAME" "' +
        attachment.filename +
        '")) NIL NIL)'
    if (data.attachments.length) structure += ' "MIXED" ("BOUNDARY" "' + boundary + '") NIL NIL NIL'
    this.#write('BODYSTRUCTURE ' + structure)
  }

  #prepareEnvelope(data) {
    try {
      let envelope = ''
      let from = this.#safeParse(data.from, {value: {address: 'unknown@unknown.com'}})
      envelope += '"' + (data.date || '') + '" '
      envelope += '"' + (data.subject || '') + '" '
      envelope += '"<' + from.value.address + '>" '
      this.#write('ENVELOPE (' + envelope + ') ')
    } catch (err) {
      error('PrepareEnvelope error:', err.message)
      this.#write('ENVELOPE ("" "" "" "") ')
    }
  }

  #prepareInternalDate(data) {
    let date = new Date(data.date)
    this.#write('INTERNALDATE "' + date.toUTCString() + '" ')
  }

  #prepareFlags(data) {
    let flags = []
    try {
      flags = data.flags ? this.#safeParse(data.flags, []) : []
    } catch (err) {
      error('Error parsing flags:', err.message)
      flags = []
    }
    flags = flags.map(flag => '\\' + flag)
    this.#write('FLAGS (' + flags.join(' ') + ') ')
  }

  #prepareRfc822(data) {
    this.#write('RFC822.SIZE ' + data.html.length + ' ')
  }

  #prepareUid(data) {
    this.#write('UID ' + data.uid + ' ')
  }

  #rename() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onRename || typeof this.#options.onRename != 'function')
      return this.#write(`${this.#request.id} NO RENAME failed\r\n`)
    let oldMailbox = this.#commands[2]
    let newMailbox = this.#commands[3]
    this.#options.onRename({address: this.#auth, oldMailbox: oldMailbox, newMailbox: newMailbox}, err => {
      if (err) return this.#write(`${this.#request.id} NO RENAME failed\r\n`)
      this.#write(`${this.#request.id} OK RENAME completed\r\n`)
    })
  }

  #prepare(requests, data) {
    try {
      data.attachments = data.attachments ? this.#safeParse(data.attachments, []) : []
      for (let request of requests) {
        if (typeof data.headerLines === 'string') {
          data.headerLines = this.#safeParse(data.headerLines, [])
        }
        let boundary = data.headerLines.find(line => line.key && line.key.toLowerCase() === 'content-type')
        if (boundary) boundary = boundary.line.replace(/"/g, '').split('boundary=')[1]
        if (!boundary) boundary = 'boundary' + (data.id || Date.now())

        switch (request.value) {
          case 'BODY':
            this.#prepareBody(request, data, boundary)
            break
          case 'BODYSTRUCTURE':
            this.#prepareBodyStructure(data, boundary)
            break
          case 'ENVELOPE':
            this.#prepareEnvelope(data)
            break
          case 'INTERNALDATE':
            this.#prepareInternalDate(data)
            break
          case 'FLAGS':
            this.#prepareFlags(data)
            break
          case 'RFC822':
            this.#prepareRfc822(data)
            break
          case 'UID':
            this.#prepareUid(data)
            break
        }
      }
    } catch (err) {
      error('Prepare error:', err.message)
    }
  }

  #select() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#options.onSelect || typeof this.#options.onSelect !== 'function') {
        return this.#write(`${this.#request.id} NO SELECT failed\r\n`)
      }

      let box = this.#commands[2]
      if (box && box.startsWith('"') && box.endsWith('"')) {
        box = box.substr(1, box.length - 2)
      }

      if (!box) {
        return this.#write(`${this.#request.id} NO Mailbox name required\r\n`)
      }

      if (!CONSTANTS.DEFAULT_BOXES.includes(box.toUpperCase())) {
        box = 'INBOX'
      }

      this.#box = box
      this.#options.onSelect({address: this.#auth, mailbox: this.#box}, this.#options, data => {
        const flagsList = CONSTANTS.PERMANENT_FLAGS.join(' ')
        this.#write(`* FLAGS (${flagsList})\r\n`)
        this.#write(`* OK [PERMANENTFLAGS (${flagsList})] Flags permitted\r\n`)
        this.#write('* ' + ((data.uidnext ?? 1) - 1) + ' EXISTS\r\n')
        this.#write('* ' + (data.recent ?? data.unseen ?? 0) + ' RECENT\r\n')
        this.#write('* OK [UNSEEN ' + (data.unseen ?? 0) + '] Message ' + (data.unseen ?? 0) + ' is first unseen\r\n')
        this.#write('* OK [UIDVALIDITY ' + (data.uidvalidity ?? CONSTANTS.UIDVALIDITY) + '] UIDs valid\r\n')
        this.#write('* OK [UIDNEXT ' + (data.uidnext ?? 1) + '] Predicted next UID\r\n')
        this.#write(`${this.#request.id} OK [READ-WRITE] SELECT completed\r\n`)
      })
    } catch (err) {
      error('SELECT command failed:', err.message)
      this.#write(`${this.#request.id} NO SELECT failed\r\n`)
    }
  }

  #status() {
    if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
    if (!this.#options.onSelect || typeof this.#options.onSelect != 'function')
      return this.#write(`${this.#request.id} NO STATUS failed\r\n`)
    let mailbox = this.#commands[2]
    let fields = this.#commands.slice(3).map(field => field.toUpperCase().replace('(', '').replace(')', ''))
    if (fields.length === 0) fields = ['MESSAGES', 'RECENT', 'UIDNEXT', 'UIDVALIDITY', 'UNSEEN']
    this.#options.onSelect({address: this.#auth, mailbox: mailbox}, this.#options, data => {
      if (data.exists && !data.messages) data.messages = data.exists
      this.#write('* STATUS ' + mailbox + ' (')
      for (let field of fields)
        if (data[field.toLowerCase()] !== undefined) this.#write(field.toUpperCase() + ' ' + (data[field.toLowerCase()] ?? 0) + ' ')
      this.#write(')\r\n')
      this.#write(`${this.#request.id} OK STATUS completed\r\n`)
    })
  }

  #store() {
    try {
      if (!this.#auth) return this.#write(`${this.#request.id} NO Authentication required\r\n`)
      if (!this.#request.uid) return this.#write(`${this.#request.id} NO UID required\r\n`)

      const uids = this.#request.uid.split(',')
      for (const field of this.#request.requests) {
        let action
        switch (field.value) {
          case '+FLAGS':
            action = 'add'
            break
          case '-FLAGS':
            action = 'remove'
            break
          case 'FLAGS':
            action = 'set'
            break
        }

        if (action && this.#options.onStore && typeof this.#options.onStore === 'function') {
          const flags = field.fields.map(flag => flag.value.replace('\\', '').toLowerCase())
          this.#options.onStore(
            {
              address: this.#auth,
              uids: uids,
              action: action,
              flags: flags
            },
            this.#options,
            data => {
              if (field.peek !== 'SILENT') {
                for (const uid of uids) {
                  this.#write('* ' + uid + ' FETCH (FLAGS (' + (data.flags || []).join(' ') + '))\r\n')
                }
              }
              this.#write(`${this.#request.id} OK STORE completed\r\n`)
            }
          )
        }
      }
    } catch (err) {
      error('STORE command failed:', err.message)
      this.#write(`${this.#request.id} NO STORE failed\r\n`)
    }
  }

  #write(data) {
    try {
      if (!this.#end && this.#socket && !this.#socket.destroyed && !this.#socket.writableEnded) {
        this.#socket.write(data)
        // Reset timeout on activity
        if (this.#timeout) {
          clearTimeout(this.#timeout)
          this.#setupTimeout()
        }
      }
    } catch (err) {
      error('Write error:', err.message)
      this.#cleanup()
    }
  }
}

module.exports = Connection
