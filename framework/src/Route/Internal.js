class Internal {
  static #validateField(validator, field, validation, value) {
    const rules = validation.rule.split('|')
    for (const rule of rules) {
      const validatorChain = validator.post(field.name).check(rule)
      if (validation.message) {
        const message = this.replacePlaceholders(validation.message, {
          value: value,
          field: field.name,
          label: field.label || field.placeholder,
          rule: rule
        })
        validatorChain.message(message)
      }
    }
  }

  static async register(Candy) {
    const token = await Candy.request('_candy_register_token')
    if (!token) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    const formData = Candy.Request.session(`_register_form_${token}`)

    if (!formData) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Candy.Request.session(`_register_form_${token}`, null)
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Candy.Request.session('_client')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Candy.Request.header('user-agent')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Candy.Request.ip) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    const config = formData.config
    const validator = Candy.validator()
    const data = {}

    const uniqueFields = []

    for (const field of config.fields) {
      const value = await Candy.request(field.name)

      for (const validation of field.validations) {
        this.#validateField(validator, field, validation, value)
      }

      if (field.unique) {
        uniqueFields.push(field.name)
      }

      if (!field.skip) {
        data[field.name] = value
      }
    }

    for (const set of config.sets) {
      if (set.value !== null) {
        if (set.ifEmpty && data[set.name]) continue
        data[set.name] = set.value
      } else if (set.compute) {
        data[set.name] = await this.computeValue(set.compute, Candy)
      } else if (set.callback) {
        if (typeof Candy.fn[set.callback] === 'function') {
          data[set.name] = await Candy.fn[set.callback](Candy)
        }
      }
    }

    if (await validator.error()) {
      return validator.result()
    }

    const registerResult = await Candy.Auth.register(data, {
      autoLogin: config.autologin !== false,
      uniqueFields: uniqueFields.length > 0 ? uniqueFields : ['email']
    })

    if (!registerResult.success) {
      if (registerResult.error === 'Database connection not configured') {
        return Candy.return({
          result: {success: false},
          errors: {_candy_form: 'Service temporarily unavailable. Please try again later.'}
        })
      }
      const errorField = registerResult.field || '_candy_form'
      const errors = {[errorField]: registerResult.error}
      return Candy.return({
        result: {success: false},
        errors: errors
      })
    }

    Candy.Request.session(`_register_form_${token}`, null)

    return Candy.return({
      result: {
        success: true,
        message: 'Registration successful',
        redirect: config.redirect
      }
    })
  }

  static replacePlaceholders(message, data) {
    if (!message) return message

    const ruleParts = data.rule ? data.rule.split(':') : []
    const ruleValue = ruleParts[1] || null

    const placeholders = {
      '{value}': data.value || '',
      '{field}': data.field || '',
      '{label}': data.label || data.field || '',
      '{min}': ruleValue,
      '{max}': ruleValue,
      '{len}': ruleValue,
      '{other}': ruleValue
    }

    let result = message
    for (const [placeholder, value] of Object.entries(placeholders)) {
      if (value !== null) {
        result = result.replace(new RegExp(placeholder.replace(/[{}]/g, '\\$&'), 'g'), value)
      }
    }

    return result
  }

  static async computeValue(type, Candy) {
    switch (type) {
      case 'now':
        return Math.floor(Date.now() / 1000)
      case 'date':
        return new Date().toISOString().split('T')[0]
      case 'datetime':
        return new Date().toISOString()
      case 'timestamp':
        return Date.now()
      case 'ip':
        return Candy.Request.ip
      case 'user_agent':
        return Candy.Request.header('user-agent')
      case 'uuid':
        return require('crypto').randomUUID()
      default:
        return null
    }
  }

  static async login(Candy) {
    const token = await Candy.request('_candy_login_token')
    if (!token) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    const formData = Candy.Request.session(`_login_form_${token}`)

    if (!formData) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Candy.Request.session(`_login_form_${token}`, null)
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Candy.Request.session('_client')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Candy.Request.header('user-agent')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Candy.Request.ip) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    const config = formData.config
    const validator = Candy.validator()
    const credentials = {}

    for (const field of config.fields) {
      const value = await Candy.request(field.name)

      for (const validation of field.validations) {
        this.#validateField(validator, field, validation, value)
      }

      credentials[field.name] = value
    }

    if (await validator.error()) {
      return validator.result()
    }

    const loginResult = await Candy.Auth.login(credentials)

    if (!loginResult.success) {
      if (loginResult.error === 'Database connection not configured') {
        return Candy.return({
          result: {success: false},
          errors: {_candy_form: 'Service temporarily unavailable. Please try again later.'}
        })
      }
      const errorField = loginResult.field || '_candy_form'
      const errors = {[errorField]: loginResult.error}
      return Candy.return({
        result: {success: false},
        errors: errors
      })
    }

    Candy.Request.session(`_login_form_${token}`, null)

    return Candy.return({
      result: {
        success: true,
        message: 'Login successful',
        redirect: config.redirect
      }
    })
  }

  static async processForm(Candy) {
    const token = await Candy.request('_candy_form_token')
    if (!token) return

    const formData = Candy.Request.session(`_custom_form_${token}`)
    if (!formData) return

    if (formData.expires < Date.now()) {
      Candy.Request.session(`_custom_form_${token}`, null)
      return
    }

    if (formData.sessionId !== Candy.Request.session('_client')) return
    if (formData.userAgent !== Candy.Request.header('user-agent')) return
    if (formData.ip !== Candy.Request.ip) return

    const config = formData.config
    const validator = Candy.validator()
    const data = {}

    const uniqueFields = []

    for (const field of config.fields) {
      const value = await Candy.request(field.name)

      for (const validation of field.validations) {
        this.#validateField(validator, field, validation, value)

        if (validation.rule.includes('unique')) {
          if (!uniqueFields.some(f => f.name === field.name)) {
            uniqueFields.push({name: field.name, message: validation.message})
          }
        }
      }

      if (!field.skip) {
        data[field.name] = value
      }
    }

    for (const set of config.sets || []) {
      if (set.value !== undefined && set.value !== null) {
        if (set.ifEmpty && data[set.name] !== undefined && data[set.name] !== null && data[set.name] !== '') continue
        data[set.name] = set.value
      } else if (set.compute) {
        data[set.name] = await this.computeValue(set.compute, Candy)
      } else if (set.callback) {
        if (typeof Candy.fn[set.callback] === 'function') {
          data[set.name] = await Candy.fn[set.callback](Candy)
        }
      }
    }

    Candy.formData = data
    Candy.formConfig = config
    Candy.formValidator = validator
    Candy.formUniqueFields = uniqueFields
  }

  static async customForm(Candy) {
    const token = await Candy.request('_candy_form_token')
    if (!token) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    const formData = Candy.Request.session(`_custom_form_${token}`)

    if (!formData) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Candy.Request.session(`_custom_form_${token}`, null)
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Candy.Request.session('_client')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Candy.Request.header('user-agent')) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Candy.Request.ip) {
      return Candy.return({
        result: {success: false},
        errors: {_candy_form: 'Invalid request'}
      })
    }

    if (await Candy.formValidator.error()) {
      return Candy.formValidator.result()
    }

    if (Candy.formConfig.table) {
      try {
        const mysql = Candy.Mysql

        for (const field of Candy.formUniqueFields) {
          if (Candy.formData[field.name] == null) continue

          const existingRecord = await mysql.query(`SELECT id FROM ?? WHERE ?? = ? LIMIT 1`, [
            Candy.formConfig.table,
            field.name,
            Candy.formData[field.name]
          ])

          if (existingRecord && existingRecord.length > 0) {
            const errorMessage = field.message || `This ${field.name} is already registered`
            return Candy.return({
              result: {success: false},
              errors: {[field.name]: errorMessage}
            })
          }
        }

        await mysql.query('INSERT INTO ?? SET ?', [Candy.formConfig.table, Candy.formData])

        Candy.Request.session(`_custom_form_${token}`, null)

        return Candy.return({
          result: {
            success: true,
            message: Candy.formConfig.successMessage || 'Form submitted successfully!',
            redirect: Candy.formConfig.redirect
          }
        })
      } catch (error) {
        if (error.message === 'Database connection not configured') {
          return Candy.return({
            result: {success: false},
            errors: {_candy_form: 'Database not configured. Please check your config.json'}
          })
        }

        return Candy.return({
          result: {success: false},
          errors: {_candy_form: error.message || 'Database error occurred'}
        })
      }
    }

    Candy.Request.session(`_custom_form_${token}`, null)

    return null
  }
}

module.exports = Internal
