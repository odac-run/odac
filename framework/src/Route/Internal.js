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

  static async register(Odac) {
    const token = await Odac.request('_odac_register_token')
    if (!token) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    const formData = Odac.Request.session(`_register_form_${token}`)

    if (!formData) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Odac.Request.session(`_register_form_${token}`, null)
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Odac.Request.session('_client')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Odac.Request.header('user-agent')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Odac.Request.ip) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    const config = formData.config
    const validator = Odac.validator()
    const data = {}

    const uniqueFields = []

    for (const field of config.fields) {
      const value = await Odac.request(field.name)

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
        data[set.name] = await this.computeValue(set.compute, Odac)
      } else if (set.callback) {
        if (typeof Odac.fn[set.callback] === 'function') {
          data[set.name] = await Odac.fn[set.callback](Odac)
        }
      }
    }

    if (await validator.error()) {
      return validator.result()
    }

    const registerResult = await Odac.Auth.register(data, {
      autoLogin: config.autologin !== false,
      uniqueFields: uniqueFields.length > 0 ? uniqueFields : ['email']
    })

    if (!registerResult.success) {
      if (registerResult.error === 'Database connection not configured') {
        return Odac.return({
          result: {success: false},
          errors: {_odac_form: 'Service temporarily unavailable. Please try again later.'}
        })
      }
      const errorField = registerResult.field || '_odac_form'
      const errors = {[errorField]: registerResult.error}
      return Odac.return({
        result: {success: false},
        errors: errors
      })
    }

    Odac.Request.session(`_register_form_${token}`, null)

    return Odac.return({
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

  static async computeValue(type, Odac) {
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
        return Odac.Request.ip
      case 'user_agent':
        return Odac.Request.header('user-agent')
      case 'uuid':
        return require('crypto').randomUUID()
      default:
        return null
    }
  }

  static async login(Odac) {
    const token = await Odac.request('_odac_login_token')
    if (!token) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    const formData = Odac.Request.session(`_login_form_${token}`)

    if (!formData) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Odac.Request.session(`_login_form_${token}`, null)
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Odac.Request.session('_client')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Odac.Request.header('user-agent')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Odac.Request.ip) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    const config = formData.config
    const validator = Odac.validator()
    const credentials = {}

    for (const field of config.fields) {
      const value = await Odac.request(field.name)

      for (const validation of field.validations) {
        this.#validateField(validator, field, validation, value)
      }

      credentials[field.name] = value
    }

    if (await validator.error()) {
      return validator.result()
    }

    const loginResult = await Odac.Auth.login(credentials)

    if (!loginResult.success) {
      if (loginResult.error === 'Database connection not configured') {
        return Odac.return({
          result: {success: false},
          errors: {_odac_form: 'Service temporarily unavailable. Please try again later.'}
        })
      }
      const errorField = loginResult.field || '_odac_form'
      const errors = {[errorField]: loginResult.error}
      return Odac.return({
        result: {success: false},
        errors: errors
      })
    }

    Odac.Request.session(`_login_form_${token}`, null)

    return Odac.return({
      result: {
        success: true,
        message: 'Login successful',
        redirect: config.redirect
      }
    })
  }

  static async processForm(Odac) {
    const token = await Odac.request('_odac_form_token')
    if (!token) return

    const formData = Odac.Request.session(`_custom_form_${token}`)
    if (!formData) return

    if (formData.expires < Date.now()) {
      Odac.Request.session(`_custom_form_${token}`, null)
      return
    }

    if (formData.sessionId !== Odac.Request.session('_client')) return
    if (formData.userAgent !== Odac.Request.header('user-agent')) return
    if (formData.ip !== Odac.Request.ip) return

    const config = formData.config
    const validator = Odac.validator()
    const data = {}

    const uniqueFields = []

    for (const field of config.fields) {
      const value = await Odac.request(field.name)

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
        data[set.name] = await this.computeValue(set.compute, Odac)
      } else if (set.callback) {
        if (typeof Odac.fn[set.callback] === 'function') {
          data[set.name] = await Odac.fn[set.callback](Odac)
        }
      }
    }

    Odac.formData = data
    Odac.formConfig = config
    Odac.formValidator = validator
    Odac.formUniqueFields = uniqueFields
  }

  static async customForm(Odac) {
    const token = await Odac.request('_odac_form_token')
    if (!token) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    const formData = Odac.Request.session(`_custom_form_${token}`)

    if (!formData) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.expires < Date.now()) {
      Odac.Request.session(`_custom_form_${token}`, null)
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Form session expired. Please refresh the page.'}
      })
    }

    if (formData.sessionId !== Odac.Request.session('_client')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid session'}
      })
    }

    if (formData.userAgent !== Odac.Request.header('user-agent')) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    if (formData.ip !== Odac.Request.ip) {
      return Odac.return({
        result: {success: false},
        errors: {_odac_form: 'Invalid request'}
      })
    }

    if (await Odac.formValidator.error()) {
      return Odac.formValidator.result()
    }

    if (Odac.formConfig.table) {
      try {
        const mysql = Odac.Mysql

        for (const field of Odac.formUniqueFields) {
          if (Odac.formData[field.name] == null) continue

          const existingRecord = await mysql.query(`SELECT id FROM ?? WHERE ?? = ? LIMIT 1`, [
            Odac.formConfig.table,
            field.name,
            Odac.formData[field.name]
          ])

          if (existingRecord && existingRecord.length > 0) {
            const errorMessage = field.message || `This ${field.name} is already registered`
            return Odac.return({
              result: {success: false},
              errors: {[field.name]: errorMessage}
            })
          }
        }

        await mysql.query('INSERT INTO ?? SET ?', [Odac.formConfig.table, Odac.formData])

        Odac.Request.session(`_custom_form_${token}`, null)

        return Odac.return({
          result: {
            success: true,
            message: Odac.formConfig.successMessage || 'Form submitted successfully!',
            redirect: Odac.formConfig.redirect
          }
        })
      } catch (error) {
        if (error.message === 'Database connection not configured') {
          return Odac.return({
            result: {success: false},
            errors: {_odac_form: 'Database not configured. Please check your config.json'}
          })
        }

        return Odac.return({
          result: {success: false},
          errors: {_odac_form: error.message || 'Database error occurred'}
        })
      }
    }

    Odac.Request.session(`_custom_form_${token}`, null)

    return null
  }
}

module.exports = Internal
