class Auth {
  #request = null
  #table = null
  #user = null

  constructor(request) {
    this.#request = request
  }

  #validateInput(where) {
    if (!where || typeof where !== 'object') return false
    for (const key in where) {
      const value = where[key]
      if (value instanceof Promise) continue
      if (typeof value !== 'string' && typeof value !== 'number') return false
    }
    return true
  }

  async check(where) {
    if (!Candy.Config.auth) Candy.Config.auth = {}
    this.#table = Candy.Config.auth.table || 'users'
    if (!this.#table) return false
    if (where) {
      if (!this.#validateInput(where)) return false
      let sql = Candy.Mysql.table(this.#table)
      if (!sql) {
        console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
        return false
      }
      for (let key in where) sql = sql.orWhere(key, where[key] instanceof Promise ? await where[key] : where[key])
      if (!sql.rows()) return false
      let get = await sql.get()
      let equal = false
      for (var user of get) {
        equal = Object.keys(where).length > 0
        for (let key of Object.keys(where)) {
          if (where[key] instanceof Promise) where[key] = await where[key]
          if (!user[key]) equal = false
          if (user[key] === where[key]) equal = equal && true
          else if (Candy.Var(user[key]).is('bcrypt')) equal = equal && Candy.Var(user[key]).hashCheck(where[key])
          else if (Candy.Var(user[key]).is('md5')) equal = equal && Candy.Var(where[key]).md5() === user[key]
        }
        if (equal) break
      }
      if (!equal) return false
      return user
    } else if (this.#user) {
      return true
    } else {
      let check_table = await Candy.Mysql.run('SHOW TABLES LIKE ?', [this.#table])
      if (check_table.length == 0) return false
      let candy_x = this.#request.cookie('candy_x')
      let candy_y = this.#request.cookie('candy_y')
      let browser = this.#request.header('user-agent')
      if (!candy_x || !candy_y || !browser) return false
      const tokenTable = Candy.Config.auth.token || 'candy_auth'
      const primaryKey = Candy.Config.auth.key || 'id'
      let sql_token = await Candy.Mysql.table(tokenTable).where(['token_x', candy_x], ['browser', browser]).get()
      if (sql_token.length !== 1) return false
      if (!Candy.Var(sql_token[0].token_y).hashCheck(candy_y)) return false

      const maxAge = Candy.Config.auth?.maxAge || 30 * 24 * 60 * 60 * 1000
      const updateAge = Candy.Config.auth?.updateAge || 24 * 60 * 60 * 1000
      const now = Date.now()
      const lastActive = new Date(sql_token[0].active).getTime()
      const inactiveAge = now - lastActive

      if (inactiveAge > maxAge) {
        await Candy.Mysql.table(tokenTable).where('id', sql_token[0].id).delete()
        return false
      }

      this.#user = await Candy.Mysql.table(this.#table).where(primaryKey, sql_token[0].user).first()

      if (inactiveAge > updateAge) {
        Candy.Mysql.table(tokenTable)
          .where('id', sql_token[0].id)
          .set({active: new Date()})
          .catch(() => {})
      }

      return true
    }
  }

  async login(where) {
    this.#user = null
    let user = await this.check(where)
    if (!user) return false
    if (!Candy.Config.auth) Candy.Config.auth = {}
    let key = Candy.Config.auth.key || 'id'
    let token = Candy.Config.auth.token || 'candy_auth'
    const mysql = require('mysql2')
    const safeTokenTable = mysql.escapeId(token)
    let check_table = await Candy.Mysql.run('SHOW TABLES LIKE ?', [token])
    if (check_table === false) {
      console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
      return false
    }
    if (check_table.length == 0)
      await Candy.Mysql.run(
        `CREATE TABLE ${safeTokenTable} (id INT NOT NULL AUTO_INCREMENT, user INT NOT NULL, token_x VARCHAR(255) NOT NULL, token_y VARCHAR(255) NOT NULL, browser VARCHAR(255) NOT NULL, ip VARCHAR(255) NOT NULL, \`date\` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, \`active\` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (id))`
      )

    this.#cleanupExpiredTokens(token)

    let token_y = Candy.Var(Math.random().toString() + Date.now().toString() + this.#request.id + this.#request.ip).md5()
    let cookie = {
      user: user[key],
      token_x: Candy.Var(Math.random().toString() + Date.now().toString()).md5(),
      token_y: Candy.Var(token_y).hash(),
      browser: this.#request.header('user-agent'),
      ip: this.#request.ip
    }
    this.#request.cookie('candy_x', cookie.token_x, {
      httpOnly: true,
      secure: true,
      sameSite: 'Strict'
    })
    this.#request.cookie('candy_y', token_y, {httpOnly: true, secure: true, sameSite: 'Strict'})
    let mysqlTable = Candy.Mysql.table(token)
    if (!mysqlTable) {
      console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
      return false
    }
    let sql = await mysqlTable.insert(cookie)
    return sql !== false
  }

  async #cleanupExpiredTokens(tokenTable) {
    const maxAge = Candy.Config.auth?.maxAge || 30 * 24 * 60 * 60 * 1000
    const cutoffDate = new Date(Date.now() - maxAge)

    Candy.Mysql.table(tokenTable)
      .where('active', '<', cutoffDate)
      .delete()
      .catch(() => {})
  }

  async register(data, options = {}) {
    if (!Candy.Config.auth) {
      Candy.Config.auth = {}
    }

    this.#table = Candy.Config.auth.table || 'users'
    const primaryKey = Candy.Config.auth.key || 'id'
    const passwordField = options.passwordField || 'password'
    const uniqueFields = options.uniqueFields || ['email']

    const checkTable = await Candy.Mysql.run('SHOW TABLES LIKE ?', [this.#table])
    if (checkTable === false) {
      console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
      return {success: false, error: 'Database connection not configured'}
    }
    if (checkTable.length === 0) {
      await this.#createUserTable(this.#table, primaryKey, passwordField, uniqueFields, data)
    }

    if (!data || typeof data !== 'object') {
      return {success: false, error: 'Invalid data provided'}
    }

    if (data[passwordField] && !Candy.Var(data[passwordField]).is('bcrypt')) {
      data[passwordField] = Candy.Var(data[passwordField]).hash()
    }

    for (const field of uniqueFields) {
      if (data[field]) {
        const mysqlTable = Candy.Mysql.table(this.#table)
        if (!mysqlTable) {
          console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
          return {success: false, error: 'Database connection not configured'}
        }
        const existing = await mysqlTable.where(field, data[field]).first()
        if (existing) {
          return {success: false, error: `${field} already exists`, field}
        }
      }
    }

    try {
      const mysqlTable = Candy.Mysql.table(this.#table)
      if (!mysqlTable) {
        console.error('CandyPack Auth Error: MySQL connection not configured. Please add database configuration to your config.json')
        return {success: false, error: 'Database connection not configured'}
      }
      const insertResult = await mysqlTable.insert(data)
      if (insertResult === false) {
        console.error('CandyPack Auth Error: Failed to insert user into database - query failed')
        console.error('Data attempted to insert:', {...data, [passwordField]: '[REDACTED]'})
        return {success: false, error: 'Failed to create user'}
      }
      if (!insertResult.affected || insertResult.affected === 0) {
        console.error('CandyPack Auth Error: Insert query succeeded but no rows were affected')
        console.error('Insert result:', insertResult)
        console.error('Data attempted to insert:', {...data, [passwordField]: '[REDACTED]'})
        return {success: false, error: 'Failed to create user'}
      }

      const userId = insertResult.id
      const newUser = await Candy.Mysql.table(this.#table).where(primaryKey, userId).first()

      if (!newUser) {
        return {success: false, error: 'User created but could not be retrieved'}
      }

      delete newUser[passwordField]

      if (options.autoLogin !== false) {
        const loginData = {}
        loginData[primaryKey] = userId
        const loginSuccess = await this.login(loginData)

        if (!loginSuccess) {
          return {success: true, user: newUser, autoLogin: false, message: 'User created but auto-login failed'}
        }
      }

      return {success: true, user: newUser}
    } catch (error) {
      console.error('CandyPack Auth Error: Registration failed with exception')
      console.error('Error:', error.message)
      console.error('Stack:', error.stack)
      return {success: false, error: error.message || 'Registration failed'}
    }
  }

  async logout() {
    if (!this.#user) return false

    if (!Candy.Config.auth) Candy.Config.auth = {}
    const token = Candy.Config.auth.token || 'user_tokens'
    const candyX = this.#request.cookie('candy_x')
    const browser = this.#request.header('user-agent')

    if (candyX && browser) {
      const mysqlTable = Candy.Mysql.table(token)
      if (mysqlTable) {
        await mysqlTable.where(['token_x', candyX], ['browser', browser]).delete()
      }
    }

    this.#request.cookie('candy_x', '', {maxAge: -1})
    this.#request.cookie('candy_y', '', {maxAge: -1})

    this.#user = null
    return true
  }

  async #createUserTable(tableName, primaryKey, passwordField, uniqueFields, sampleData) {
    const mysql = require('mysql2')
    const columns = []

    const safePrimaryKey = mysql.escapeId(primaryKey)
    columns.push(`${safePrimaryKey} INT NOT NULL AUTO_INCREMENT`)

    for (const field of uniqueFields) {
      if (field !== primaryKey) {
        const safeField = mysql.escapeId(field)
        columns.push(`${safeField} VARCHAR(255) NOT NULL UNIQUE`)
      }
    }

    if (!uniqueFields.includes(passwordField) && passwordField !== primaryKey) {
      const safePasswordField = mysql.escapeId(passwordField)
      columns.push(`${safePasswordField} VARCHAR(255) NOT NULL`)
    }

    for (const key in sampleData) {
      if (key === primaryKey || uniqueFields.includes(key) || key === passwordField) continue

      const value = sampleData[key]
      let columnType = 'VARCHAR(255)'

      if (typeof value === 'number') {
        if (Number.isInteger(value)) {
          columnType = value > 2147483647 ? 'BIGINT' : 'INT'
        } else {
          columnType = 'DECIMAL(10,2)'
        }
      } else if (typeof value === 'boolean') {
        columnType = 'TINYINT(1)'
      } else if (value && value.length > 255) {
        columnType = 'TEXT'
      }

      const safeKey = mysql.escapeId(key)
      columns.push(`${safeKey} ${columnType} NULL`)
    }

    columns.push(`${mysql.escapeId('created_at')} TIMESTAMP DEFAULT CURRENT_TIMESTAMP`)
    columns.push(`${mysql.escapeId('updated_at')} TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP`)
    columns.push(`PRIMARY KEY (${safePrimaryKey})`)

    const safeTableName = mysql.escapeId(tableName)
    const sql = `CREATE TABLE ${safeTableName} (${columns.join(', ')}) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

    await Candy.Mysql.run(sql)
  }

  user(col) {
    if (!this.#user) return false
    if (col === null) return this.#user
    else return this.#user[col]
  }
}

module.exports = Auth
