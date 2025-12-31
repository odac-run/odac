const {log, error} = Odac.core('Log', false).init('Service')

const bcrypt = require('bcrypt')
const SMTPServer = require('smtp-server').SMTPServer
const parser = require('mailparser').simpleParser
const sqlite3 = require('sqlite3').verbose()
const forge = require('node-forge')
const fs = require('fs')
const os = require('os')
const server = require('./mail/server')
const smtp = require('./mail/smtp')
const tls = require('tls')

class Mail {
  #checking = false
  #clients = {}
  #counts = {}
  #db
  #server_smtp
  #started = false
  #sslCache = new Map()
  #blocked = new Map()

  clearSSLCache(domain) {
    if (domain) {
      for (const key of this.#sslCache.keys()) {
        if (key === domain || key.endsWith('.' + domain)) {
          this.#sslCache.delete(key)
        }
      }
    } else {
      this.#sslCache.clear()
    }
  }

  #handleFailedAuth(ip) {
    if (!this.#clients[ip]) this.#clients[ip] = {attempts: 0, last: 0}
    // Reset counter if last attempt was more than 1 hour ago
    if (Date.now() - this.#clients[ip].last > 1000 * 60 * 60) {
      this.#clients[ip] = {attempts: 0, last: 0}
    }

    this.#clients[ip].attempts++
    this.#clients[ip].last = Date.now()

    if (this.#clients[ip].attempts > 5) {
      this.#block(ip, 'Too many failed login attempts')
      delete this.#clients[ip]
    }
  }

  #block(ip, reason = 'Suspicious activity') {
    if (this.#blocked.has(ip)) return
    log(`Blocking IP ${ip}: ${reason}`)
    // Block for 24 hours
    this.#blocked.set(ip, Date.now() + 1000 * 60 * 60 * 24)
  }

  #isBlocked(ip) {
    if (!this.#blocked.has(ip)) return false
    if (this.#blocked.get(ip) < Date.now()) {
      this.#blocked.delete(ip)
      return false
    }
    return true
  }

  check() {
    if (this.#checking) return
    if (!this.#started) this.init()
    if (!this.#started) return
    this.#checking = true
    for (const domain of Object.keys(Odac.core('Config').config.websites)) {
      if (!Odac.core('Config').config.websites[domain].DNS || !Odac.core('Config').config.websites[domain].DNS.MX) continue
      if (Odac.core('Config').config.websites[domain].cert !== false && !Odac.core('Config').config.websites[domain].cert?.dkim)
        this.#dkim(domain)
    }
    this.#checking = false
  }

  async create(email, password, retype) {
    if (!email || !password || !retype) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (password != retype) return Odac.server('Api').result(false, await __('Passwords do not match.'))
    password = await new Promise((resolve, reject) => {
      bcrypt.hash(password, 10, (err, hash) => {
        if (err) reject(err)
        resolve(hash)
      })
    })
    if (!email.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/))
      return Odac.server('Api').result(false, await __('Invalid email address.'))
    if (await this.exists(email)) return Odac.server('Api').result(false, await __('Mail account %s already exists.', email))
    let domain = email.split('@')[1]
    if (!Odac.core('Config').config.websites[domain]) {
      for (let d in Odac.core('Config').config.websites) {
        if (domain.substr(-d.length) != d) continue
        if (Odac.core('Config').config.websites[d].subdomain.includes(domain.substr(-d.length))) {
          domain = d
          break
        }
      }
      return Odac.server('Api').result(false, await __('Domain %s not found.', domain))
    }
    this.#db.serialize(() => {
      let stmt = this.#db.prepare("INSERT INTO mail_account ('email', 'password', 'domain') VALUES (?, ?, ?)")
      stmt.run(email, password, domain)
      stmt.finalize()
    })
    return Odac.server('Api').result(true, await __('Mail account %s created successfully.', email))
  }

  async delete(email) {
    if (!email) return Odac.server('Api').result(false, await __('Email address is required.'))
    if (!email.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/))
      return Odac.server('Api').result(false, await __('Invalid email address.'))
    if (!(await this.exists(email))) return Odac.server('Api').result(false, await __('Mail account %s not found.', email))
    this.#db.serialize(() => {
      let stmt = this.#db.prepare('DELETE FROM mail_account WHERE email = ?')
      stmt.run(email)
      stmt.finalize()
    })
    return Odac.server('Api').result(true, await __('Mail account %s deleted successfully.', email))
  }

  #dkim(domain) {
    let keys = forge.pki.rsa.generateKeyPair(1024)
    const privateKeyPem = forge.pki.privateKeyToPem(keys.privateKey)
    let publicKeyPem = forge.pki.publicKeyToPem(keys.publicKey)
    if (!fs.existsSync(os.homedir() + '/.odac/cert/dkim')) fs.mkdirSync(os.homedir() + '/.odac/cert/dkim', {recursive: true})
    fs.writeFileSync(os.homedir() + '/.odac/cert/dkim/' + domain + '.key', privateKeyPem)
    fs.chmodSync(os.homedir() + '/.odac/cert/dkim/' + domain + '.key', 0o600)
    fs.writeFileSync(os.homedir() + '/.odac/cert/dkim/' + domain + '.pub', publicKeyPem)
    publicKeyPem = publicKeyPem
      .replace('-----BEGIN PUBLIC KEY-----', '')
      .replace('-----END PUBLIC KEY-----', '')
      .replace(/\r\n/g, '')
      .replace(/\n/g, '')
    if (!Odac.core('Config').config.websites[domain].cert) Odac.core('Config').config.websites[domain].cert = {}
    Odac.core('Config').config.websites[domain].cert.dkim = {
      private: os.homedir() + '/.odac/cert/dkim/' + domain + '.key',
      public: os.homedir() + '/.odac/cert/dkim/' + domain + '.pub'
    }
    Odac.server('DNS').record({
      type: 'TXT',
      name: `default._domainkey.${domain}`,
      value: `v=DKIM1; k=rsa; p=${publicKeyPem}`
    })
  }

  exists(email) {
    return new Promise(resolve => {
      this.#db.get('SELECT * FROM mail_account WHERE email = ?', [email], (err, row) => {
        if (row) resolve(row)
        else resolve(false)
      })
    })
  }

  init() {
    let start = false
    for (let domain in Odac.core('Config').config.websites) {
      let web = Odac.core('Config').config.websites[domain]
      if (web && web.DNS && web.DNS.MX) start = true
    }
    if (!start || this.#started) return
    this.#started = true
    if (!fs.existsSync(os.homedir() + '/.odac/db')) fs.mkdirSync(os.homedir() + '/.odac/db', {recursive: true})
    this.#db = new sqlite3.Database(os.homedir() + '/.odac/db/mail', err => {
      if (err) error(err.message)
    })
    this.#db.serialize(() => {
      this.#db.run(`CREATE TABLE IF NOT EXISTS mail_received ('id'          INTEGER PRIMARY KEY AUTOINCREMENT,
                                                                    'uid'         INTEGER NOT NULL,
                                                                    'email'       VARCHAR(255) NOT NULL,
                                                                    'mailbox'     VARCHAR(255),
                                                                    'flags'       JSON DEFAULT '[]',
                                                                    'attachments' JSON,
                                                                    'headers'     JSON,
                                                                    'headerLines' JSON,
                                                                    'html'        TEXT,
                                                                    'text'        TEXT,
                                                                    'textAsHtml'  TEXT,
                                                                    'subject'     TEXT,
                                                                    'date'        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                                                                    'to'          JSON,
                                                                    'from'        JSON,
                                                                    'messageId'   TEXT,
                                                                    UNIQUE(email, uid))`)
      this.#db.run(`CREATE TABLE IF NOT EXISTS mail_account ('id'       INTEGER PRIMARY KEY AUTOINCREMENT,
                                                                   'email'    VARCHAR(255) UNIQUE,
                                                                   'password' VARCHAR(255),
                                                                   'domain'   VARCHAR(255),
                                                                   'created'  TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`)
      this.#db.run(`CREATE TABLE IF NOT EXISTS mail_box ('id'       INTEGER PRIMARY KEY AUTOINCREMENT,
                                                               'email'    VARCHAR(255),
                                                               'title'    VARCHAR(255),
                                                               'parent'   INTEGER DEFAULT 0,
                                                               'deleted'  BOOLEAN DEFAULT 0,
                                                               'date'     TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                                                                UNIQUE(email, title))`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_email  ON mail_account  (email);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_domain ON mail_account  (domain);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_uid    ON mail_received (uid);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_email  ON mail_received (email);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_flags  ON mail_received (flags);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_date   ON mail_received (date);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_email  ON mail_box      (email);`)
      this.#db.run(`CREATE INDEX IF NOT EXISTS idx_title  ON mail_box      (title);`)
    })
    const self = this
    let options = {
      logger: true,
      tls: {minVersion: 'TLSv1.2'},
      secure: false,
      banner: 'ODAC',
      size: 1024 * 1024 * 10,
      authOptional: true,
      onConnect(session, callback) {
        if (self.#isBlocked(session.remoteAddress)) {
          return callback(new Error('Your IP is blocked due to suspicious activity.'))
        }
        return callback()
      },
      onAuth(auth, session, callback) {
        let ip = session.remoteAddress
        // Basic format check
        if (!auth.username.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/)) {
          self.#handleFailedAuth(ip)
          return callback(new Error('Invalid username or password'))
        }

        self.exists(auth.username).then(async result => {
          if (result && (await bcrypt.compare(auth.password, result.password))) {
            // Successful login, clear attempts
            if (self.#clients[ip]) delete self.#clients[ip]
            return callback(null, {user: auth.username})
          }

          self.#handleFailedAuth(ip)
          return callback(new Error('Invalid username or password'))
        })
      },
      onAppend(data, callback) {
        parser(data.message, {}, async (err, parsed) => {
          if (err) {
            error(err)
            return callback(err)
          }
          await self.#store(data.address, parsed, data.mailbox, data.flags)
          callback()
        })
      },
      onExpunge(data, callback) {
        self.#db.all(
          "SELECT uid FROM mail_received WHERE email = ? AND mailbox = ? AND flags LIKE '%deleted%'",
          [data.address, data.mailbox],
          (err, rows) => {
            if (err) {
              error(err)
              return callback(err)
            }
            let uids = rows.map(row => row.uid)
            self.#db.run(
              "DELETE FROM mail_received WHERE email = ? AND mailbox = ? AND flags LIKE '%deleted%'",
              [data.address, data.mailbox],
              err => {
                if (err) {
                  error(err)
                  return callback(err)
                }
                callback(null, uids)
              }
            )
          }
        )
      },
      onData(stream, session, callback) {
        parser(stream, {}, async (err, parsed) => {
          if (err) return error(err)
          // log('ON DATA:', session);
          if (!parsed.to?.value?.[0]?.address) {
            error('Missing recipient address')
            return callback(new Error('Invalid recipient'))
          }
          if (!parsed.to.value[0].address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/)) {
            error('Invalid recipient:', parsed.to.value[0].address)
            return callback(new Error('Invalid recipient'))
          }
          if (!parsed.from?.value?.[0]?.address) {
            error('Missing sender address')
            return callback(new Error('Invalid sender'))
          }
          if (!parsed.from.value[0].address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/)) {
            error('Invalid sender:', parsed.from.value[0].address)
            return callback(new Error('Invalid sender'))
          }
          let sender = await self.exists(parsed.from.value[0].address)
          if (sender && (!session.user || parsed.from.value[0].address !== session.user)) {
            error('Unexpected sender:', parsed.from.value[0].address)
            return callback(new Error('Unexpected sender'))
          }
          if (
            !sender &&
            !['hostmaster', 'postmaster'].includes(parsed.to.value[0].address.split('@')[0]) &&
            !(await self.exists(parsed.to.value[0].address))
          ) {
            error('Unexpected recipient:', parsed.to.value[0].address)
            return callback(new Error('Unexpected recipient'))
          }
          await self.#store(session.user ?? parsed.to.value[0].address, parsed)
          if (session.user && parsed.from.value[0].address === session.user) smtp.send(parsed)
          callback()
        })
      },
      onCreate(data, callback) {
        self.#db.run('INSERT INTO mail_box (email, title) VALUES (?, ?)', [data.address, data.mailbox], err => {
          if (err) {
            error(err)
            return callback(err)
          }
          callback()
        })
      },
      onDelete(data, callback) {
        self.#db.run('DELETE FROM mail_box WHERE email = ? AND title = ?', [data.address, data.mailbox], err => {
          if (err) {
            error(err)
            return callback(err)
          }
          callback()
        })
      },
      onRename(data, callback) {
        self.#db.run(
          'UPDATE mail_box SET title = ? WHERE email = ? AND title = ?',
          [data.newMailbox, data.address, data.oldMailbox],
          err => {
            if (err) {
              error(err)
              return callback(err)
            }
            callback()
          }
        )
      },
      onFetch(data, session, callback) {
        let limit = ``
        if (data.limit) {
          if (data.limit[0] && !isNaN(data.limit[0])) limit += `AND uid >= ${parseInt(data.limit[0])} `
          if (data.limit[1] && !isNaN(data.limit[1])) limit += `AND uid <= ${parseInt(data.limit[1])} `
        }
        self.#db.all(
          `SELECT * FROM mail_received
                              WHERE email = ? AND mailbox = ? ${limit}
                              ORDER BY id DESC`,
          [data.email, data.mailbox],
          (err, rows) => {
            if (err) {
              error(err)
              return callback(false)
            }
            callback(rows)
          }
        )
      },
      onList(data, callback) {
        self.#db.all('SELECT title FROM mail_box WHERE email = ?', [data.address], (err, rows) => {
          if (err) {
            error(err)
            return callback(err)
          }
          let boxes = rows.map(row => row.title)
          if (!boxes.includes('INBOX')) boxes.unshift('INBOX')
          callback(null, boxes)
        })
      },
      onLsub(data, callback) {
        self.#db.all('SELECT title FROM mail_box WHERE email = ?', [data.address], (err, rows) => {
          if (err) {
            error(err)
            return callback(err)
          }
          let boxes = rows.map(row => row.title)
          if (!boxes.includes('INBOX')) boxes.unshift('INBOX')
          callback(null, boxes)
        })
      },
      onMailFrom(address, session, callback) {
        if (!address.address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/)) return callback(new Error('Invalid email address'))
        return callback()
      },
      onRcptTo(address, session, callback) {
        if (!address.address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/)) return callback(new Error('Invalid email address'))
        return callback()
      },
      onSelect(data, session, callback) {
        self.#db.get(
          "SELECT COUNT(*) AS 'exists', SUM(IIF(flags LIKE '%seen%', 0, 1)) AS 'unseen', MAX(uid) + 1 AS uidnext, MAX(uid) AS uidvalidity FROM mail_received WHERE email = ? AND mailbox = ?",
          [data.address, data.mailbox],
          (err, row) => {
            if (err) {
              error(err)
              return callback(err)
            }
            callback(row)
          }
        )
      },
      onStore(data, session, callback) {
        let uids = data.uids
        for (let flag of data.flags) {
          for (let uid of uids) {
            uid = [uid, uid]
            if (uid.includes(':')) uid = uid.split(':')
            switch (data.action) {
              case 'add':
                self.#db.run(
                  `UPDATE mail_received
                                    SET flags = JSON_INSERT(flags, '$[#]', ?)
                                    WHERE email = ? AND uid BETWEEN ? AND ? AND flags NOT LIKE ?`,
                  [flag, data.address, uid[0], uid[1], `%${flag}%`],
                  err => {
                    if (err) {
                      error(err)
                      return callback(err)
                    }
                  }
                )
                break
              case 'remove':
                self.#db.run(
                  `UPDATE mail_received
                                    SET flags = JSON_REMOVE(flags, (SELECT value FROM JSON_EACH(flags) WHERE value = ?))
                                    WHERE email = ? AND uid BETWEEN ? AND ? AND flags LIKE ?`,
                  [flag, data.address, uid[0], uid[1], `%${flag}%`],
                  err => {
                    if (err) {
                      error(err)
                      return callback(err)
                    }
                  }
                )
                break
              case 'set':
                self.#db.run(
                  `UPDATE mail_received
                                    SET flags = JSON_SET(flags, '$', ?)
                                    WHERE email = ? AND uid BETWEEN ? AND ?`,
                  [JSON.stringify(data.flags), data.address, uid[0], uid[1]],
                  err => {
                    if (err) {
                      error(err)
                      return callback(err)
                    }
                  }
                )
                break
            }
          }
        }
        callback()
      },
      onError(err) {
        error('Error:', err)
      }
    }
    let serv = new SMTPServer(options)
    serv.listen(25)
    serv.on('error', err => log('SMTP Server Error: ', err))
    // Handle socket errors to prevent crash
    if (serv.server) {
      serv.server.on('connection', socket => {
        socket.on('error', err => {
          if (err.code !== 'ECONNRESET') error('SMTP Socket Error:', err)
        })
      })
    }
    const imap = new server(options)
    imap.listen(143)
    options.SNICallback = (hostname, callback) => {
      const cached = this.#sslCache.get(hostname)
      if (cached) return callback(null, cached)

      let ssl = Odac.core('Config').config.ssl ?? {}
      let sslOptions = {}
      while (!Odac.core('Config').config.websites[hostname] && hostname.includes('.')) hostname = hostname.split('.').slice(1).join('.')
      let website = Odac.core('Config').config.websites[hostname]
      if (
        website &&
        website.cert.ssl &&
        website.cert.ssl.key &&
        website.cert.ssl.cert &&
        fs.existsSync(website.cert.ssl.key) &&
        fs.existsSync(website.cert.ssl.cert)
      ) {
        sslOptions = {
          key: fs.readFileSync(website.cert.ssl.key),
          cert: fs.readFileSync(website.cert.ssl.cert)
        }
      } else {
        sslOptions = {
          key: fs.readFileSync(ssl.key),
          cert: fs.readFileSync(ssl.cert)
        }
      }
      sslOptions.minVersion = 'TLSv1.2'
      const ctx = tls.createSecureContext(sslOptions)
      this.#sslCache.set(hostname, ctx)
      callback(null, ctx)
    }
    options.secure = true
    this.#server_smtp = new SMTPServer(options)
    this.#server_smtp.listen(465)
    this.#server_smtp.on('error', err => {
      if (err.code === 'ERR_SSL_HTTP_REQUEST' && err.meta?.remoteAddress) {
        this.#block(err.meta.remoteAddress, 'HTTP request on SMTP port')
      }
      error('SMTP Server Error: ', err)
    })
    // Handle socket errors to prevent crash
    if (this.#server_smtp.server) {
      this.#server_smtp.server.on('connection', socket => {
        socket.on('error', err => {
          if (err.code !== 'ECONNRESET') error('SMTP Secure Socket Error:', err)
        })
      })
    }
    const imap_sec = new server(options)
    imap_sec.listen(993)
  }

  async list(domain) {
    if (!domain) return Odac.server('Api').result(false, await __('Domain is required.'))
    if (!Odac.core('Config').config.websites[domain]) return Odac.server('Api').result(false, await __('Domain %s not found.', domain))
    let accounts = []
    await new Promise((resolve, reject) => {
      this.#db.each(
        'SELECT * FROM mail_account WHERE domain = ?',
        [domain],
        (err, row) => {
          if (err) reject(err)
          accounts.push(row.email)
        },
        (err, count) => {
          if (err) reject(err)
          resolve(count)
        }
      )
    })
    return Odac.server('Api').result(true, (await __('Mail accounts for domain %s.', domain)) + '\n' + accounts.join('\n'))
  }

  async password(email, password, retype) {
    if (!email || !password || !retype) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (password != retype) return Odac.server('Api').result(false, await __('Passwords do not match.'))
    password = await new Promise((resolve, reject) => {
      bcrypt.hash(password, 10, (err, hash) => {
        if (err) reject(err)
        resolve(hash)
      })
    })
    if (!email.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/))
      return Odac.server('Api').result(false, await __('Invalid email address.'))
    if (!this.exists(email)) return Odac.server('Api').result(false, await __('Mail account %s not found.', email))
    this.#db.serialize(() => {
      let stmt = this.#db.prepare('UPDATE mail_account SET password = ? WHERE email = ?')
      stmt.run(password, email)
      stmt.finalize()
    })
    return Odac.server('Api').result(true, await __('Mail account %s password updated successfully.', email))
  }

  async send(data) {
    if (!data || !data.from || !data.to || !data.header) return Odac.server('Api').result(false, await __('All fields are required.'))
    if (!data.from.value[0].address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/))
      return Odac.server('Api').result(false, await __('Invalid email address.'))
    if (!data.to.value[0].address.match(/^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/))
      return Odac.server('Api').result(false, await __('Invalid email address.'))
    let domain = data.from.value[0].address.split('@')[1].split('.')
    while (domain.length > 2 && !Odac.core('Config').config.websites[domain.join('.')]) domain.shift()
    domain = domain.join('.')
    if (!Odac.core('Config').config.websites[domain]) return Odac.server('Api').result(false, await __('Domain %s not found.', domain))
    let mail = {
      atttachments: [],
      headerLines: [],
      from: data.from,
      to: data.to,
      subject: data.subject ?? ''
    }
    for (let key in data.header) mail.headerLines.push({key: key.toLowerCase(), line: key + ': ' + data.header[key]})
    if (data.html) mail.html = data.html
    if (data.text) mail.text = data.text
    mail.attachments = data.attachments ?? []
    smtp.send(mail)
    return Odac.server('Api').result(true, await __('Mail sent successfully.'))
  }

  #store(email, data, mailbox = 'INBOX', flags = '[]') {
    return new Promise(resolve => {
      if (email === data.from.value[0].address) {
        flags = JSON.stringify(['seen'])
        mailbox = 'Sent'
      }
      this.#db.serialize(async () => {
        if (!this.#counts[email]) {
          await new Promise((sub_resolve, sub_reject) => {
            this.#db.get('SELECT COUNT(*) AS count FROM mail_received WHERE email = ?', [email], (err, row) => {
              if (err) return sub_reject(err)
              this.#counts[email] = row.count + 1
              return sub_resolve()
            })
          })
        } else this.#counts[email]++
        if (data.html === '0') data.html = ''
        this.#db.run(
          "INSERT INTO mail_received ('uid', 'email', 'mailbox', 'attachments', 'headers', 'headerLines', 'html', 'text', 'textAsHtml', 'subject', 'to', 'from', 'messageId', 'flags') VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
          [
            this.#counts[email],
            email,
            mailbox,
            JSON.stringify(data.attachments),
            JSON.stringify(data.headers),
            JSON.stringify(data.headerLines),
            data.html,
            data.text,
            data.textAsHtml,
            data.subject,
            JSON.stringify(data.to),
            JSON.stringify(data.from),
            data.messageId,
            flags
          ],
          async err => {
            if (!err) return resolve(true)
            error(err)
            return resolve(await this.#store(email, data))
          }
        )
      })
    })
  }
}

module.exports = new Mail()
