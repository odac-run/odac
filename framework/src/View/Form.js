const nodeCrypto = require('crypto')

class Form {
  static FORM_TYPES = ['register', 'login', 'form']

  static parse(content, Candy) {
    for (const type of this.FORM_TYPES) {
      content = this.parseFormType(content, Candy, type)
    }
    return content
  }

  static parseFormType(content, Candy, type) {
    const regex = new RegExp(`<candy:${type}[\\s\\S]*?<\\/candy:${type}>`, 'g')
    const matches = content.match(regex)
    if (!matches) return content

    for (const match of matches) {
      const formToken = nodeCrypto.randomBytes(32).toString('hex')
      const formConfig = this.extractConfig(match, formToken, type)

      this.storeConfig(formToken, formConfig, Candy, type)

      const generatedForm = this.generateForm(match, formConfig, formToken, type)
      content = content.replace(match, generatedForm)
    }

    return content
  }

  static extractConfig(html, formToken, type) {
    if (type === 'register') {
      return this.extractRegisterConfig(html, formToken)
    } else if (type === 'login') {
      return this.extractLoginConfig(html, formToken)
    } else if (type === 'form') {
      return this.extractFormConfig(html, formToken)
    }
  }

  static storeConfig(token, config, Candy, type) {
    if (type === 'register') {
      this.storeRegisterConfig(token, config, Candy)
    } else if (type === 'login') {
      this.storeLoginConfig(token, config, Candy)
    } else if (type === 'form') {
      this.storeFormConfig(token, config, Candy)
    }
  }

  static generateForm(originalHtml, config, formToken, type) {
    if (type === 'register') {
      return this.generateRegisterForm(originalHtml, config, formToken)
    } else if (type === 'login') {
      return this.generateLoginForm(originalHtml, config, formToken)
    } else if (type === 'form') {
      return this.generateCustomForm(originalHtml, config, formToken)
    }
  }

  static extractRegisterConfig(html, formToken) {
    const config = {
      token: formToken,
      redirect: null,
      autologin: true,
      submitText: 'Register',
      submitLoading: 'Processing...',
      fields: [],
      sets: []
    }

    const registerMatch = html.match(/<candy:register([^>]*)>/)
    if (!registerMatch) return config

    const registerTag = registerMatch[0]
    const redirectMatch = registerTag.match(/redirect=["']([^"']+)["']/)
    const autologinMatch = registerTag.match(/autologin=["']([^"']+)["']/)

    if (redirectMatch) config.redirect = redirectMatch[1]
    if (autologinMatch) config.autologin = autologinMatch[1] !== 'false'

    const submitMatch = html.match(/<candy:submit([^>/]*)(?:\/?>|>(.*?)<\/candy:submit>)/)
    if (submitMatch) {
      const submitTag = submitMatch[1]
      const textMatch = submitTag.match(/text=["']([^"']+)["']/)
      const loadingMatch = submitTag.match(/loading=["']([^"']+)["']/)
      const classMatch = submitTag.match(/class=["']([^"']+)["']/)
      const styleMatch = submitTag.match(/style=["']([^"']+)["']/)
      const idMatch = submitTag.match(/id=["']([^"']+)["']/)

      if (textMatch) config.submitText = textMatch[1]
      else if (submitMatch[2]) config.submitText = submitMatch[2].trim()

      if (loadingMatch) config.submitLoading = loadingMatch[1]
      if (classMatch) config.submitClass = classMatch[1]
      if (styleMatch) config.submitStyle = styleMatch[1]
      if (idMatch) config.submitId = idMatch[1]
    }

    const fieldMatches = html.match(/<candy:field[\s\S]*?<\/candy:field>/g)
    if (fieldMatches) {
      for (const fieldHtml of fieldMatches) {
        const field = this.parseField(fieldHtml)
        if (field) config.fields.push(field)
      }
    }

    const setMatches = html.match(/<candy:set[^>]*\/?>/g)
    if (setMatches) {
      for (const setTag of setMatches) {
        const set = this.parseSet(setTag)
        if (set) config.sets.push(set)
      }
    }

    return config
  }

  static parseField(html) {
    const fieldTagMatch = html.match(/<candy:field([^>]*?)(?:\/>|>)/)
    if (!fieldTagMatch) return null

    const fieldTag = fieldTagMatch[0]
    const nameMatch = fieldTag.match(/name=["']([^"']+)["']/)
    if (!nameMatch) return null

    const field = {
      name: nameMatch[1],
      type: 'text',
      placeholder: '',
      label: null,
      class: '',
      id: null,
      unique: false,
      skip: false,
      validations: []
    }

    const typeMatch = fieldTag.match(/type=["']([^"']+)["']/)
    const placeholderMatch = fieldTag.match(/placeholder=["']([^"']+)["']/)
    const labelMatch = fieldTag.match(/label=["']([^"']+)["']/)
    const classMatch = fieldTag.match(/class=["']([^"']+)["']/)
    const idMatch = fieldTag.match(/id=["']([^"']+)["']/)
    const uniqueMatch = fieldTag.match(/unique=["']([^"']+)["']/) || fieldTag.match(/\sunique[\s/>]/)
    const skipMatch = fieldTag.match(/skip=["']([^"']+)["']/) || fieldTag.match(/\sskip[\s/>]/)

    if (typeMatch) field.type = typeMatch[1]
    if (placeholderMatch) field.placeholder = placeholderMatch[1]
    if (labelMatch) field.label = labelMatch[1]
    if (classMatch) field.class = classMatch[1]
    if (idMatch) field.id = idMatch[1]
    if (uniqueMatch) field.unique = uniqueMatch[1] !== 'false'
    if (skipMatch) field.skip = skipMatch[1] !== 'false'

    const validateMatches = html.match(/<candy:validate[^>]*>/g)
    if (validateMatches) {
      for (const validateTag of validateMatches) {
        const ruleMatch = validateTag.match(/rule=["']([^"']+)["']/)
        const messageMatch = validateTag.match(/message=(["'])(.*?)\1/)

        if (ruleMatch) {
          field.validations.push({
            rule: ruleMatch[1],
            message: messageMatch ? messageMatch[2] : null
          })
        }
      }
    }

    return field
  }

  static parseSet(html) {
    const nameMatch = html.match(/name=["']([^"']+)["']/)
    if (!nameMatch) return null

    const set = {
      name: nameMatch[1],
      value: null,
      compute: null,
      callback: null,
      ifEmpty: false
    }

    const valueMatch = html.match(/value=(["'])(.*?)\1/)
    const computeMatch = html.match(/compute=["']([^"']+)["']/)
    const callbackMatch = html.match(/callback=["']([^"']+)["']/)
    const ifEmptyMatch = html.match(/if-empty=["']([^"']+)["']/) || html.match(/\sif-empty[\s/>]/)

    if (valueMatch) set.value = valueMatch[2]
    if (computeMatch) set.compute = computeMatch[1]
    if (callbackMatch) set.callback = callbackMatch[1]
    if (ifEmptyMatch) set.ifEmpty = ifEmptyMatch[1] !== 'false'

    return set
  }

  static storeRegisterConfig(token, config, Candy) {
    if (!Candy.View) Candy.View = {}
    if (!Candy.View.registerForms) Candy.View.registerForms = {}

    const formData = {
      config: config,
      created: Date.now(),
      expires: Date.now() + 30 * 60 * 1000,
      sessionId: Candy.Request.session('_client'),
      userAgent: Candy.Request.header('user-agent'),
      ip: Candy.Request.ip
    }

    Candy.View.registerForms[token] = formData
    Candy.Request.session(`_register_form_${token}`, formData)
  }

  static generateRegisterForm(originalHtml, config, formToken) {
    const submitText = config.submitText || 'Register'
    const submitLoading = config.submitLoading || 'Processing...'

    let innerContent = originalHtml.replace(/<candy:register[^>]*>/, '').replace(/<\/candy:register>/, '')

    innerContent = innerContent.replace(/<candy:field[\s\S]*?<\/candy:field>/g, fieldMatch => {
      const field = this.parseField(fieldMatch)
      if (!field) return fieldMatch
      return this.generateFieldHtml(field)
    })

    const submitMatch = innerContent.match(/<candy:submit[\s\S]*?(?:<\/candy:submit>|\/?>)/)
    if (submitMatch) {
      let submitAttrs = `type="submit" data-submit-text="${submitText}" data-loading-text="${submitLoading}"`
      if (config.submitClass) submitAttrs += ` class="${config.submitClass}"`
      if (config.submitStyle) submitAttrs += ` style="${config.submitStyle}"`
      if (config.submitId) submitAttrs += ` id="${config.submitId}"`
      const submitButton = `<button ${submitAttrs}>${submitText}</button>`
      innerContent = innerContent.replace(submitMatch[0], submitButton)
    }

    innerContent = innerContent.replace(/<candy:set[^>]*\/?>/g, '')

    let html = `<form class="candy-register-form" data-candy-register="${formToken}" method="POST" action="/_candy/register" novalidate>\n`
    html += `  <input type="hidden" name="_candy_register_token" value="${formToken}">\n`
    html += innerContent
    html += `\n  <span class="candy-form-success" style="display:none;"></span>\n`
    html += `</form>`

    return html
  }

  static generateFieldHtml(field) {
    let html = ''

    if (field.label && field.type !== 'checkbox') {
      const fieldId = field.id || `candy-${field.name}`
      html += `<label for="${fieldId}">${field.label}</label>\n`
    }

    const classAttr = field.class ? ` class="${field.class}"` : ''
    const idAttr = field.id ? ` id="${field.id}"` : ` id="candy-${field.name}"`

    if (field.type === 'checkbox') {
      const attrs = this.buildHtml5Attributes(field)
      if (field.label) {
        html += `<label>\n`
        html += `  <input type="checkbox"${idAttr} name="${field.name}" value="1"${classAttr}${attrs}>\n`
        html += `  ${field.label}\n`
        html += `</label>\n`
      } else {
        html += `<input type="checkbox"${idAttr} name="${field.name}" value="1"${classAttr}${attrs}>\n`
      }
    } else if (field.type === 'textarea') {
      const attrs = this.buildHtml5Attributes(field)
      html += `<textarea${idAttr} name="${field.name}" placeholder="${field.placeholder}"${classAttr}${attrs}></textarea>\n`
    } else {
      const attrs = this.buildHtml5Attributes(field)
      html += `<input type="${field.type}"${idAttr} name="${field.name}" placeholder="${field.placeholder}"${classAttr}${attrs}>\n`
    }

    return html
  }

  static buildHtml5Attributes(field) {
    let attrs = ''
    const html5Rules = {
      required: false,
      minlength: null,
      maxlength: null,
      min: null,
      max: null,
      pattern: null
    }
    const errorMessages = {}

    for (const validation of field.validations) {
      const rules = validation.rule.split('|')
      for (const rule of rules) {
        const [ruleName, ruleValue] = rule.split(':')

        switch (ruleName) {
          case 'required':
            html5Rules.required = true
            if (validation.message) errorMessages.required = validation.message
            break
          case 'minlen':
            if (field.type !== 'number') {
              html5Rules.minlength = ruleValue
              if (validation.message) errorMessages.minlength = validation.message
            }
            break
          case 'maxlen':
            if (field.type !== 'number') {
              html5Rules.maxlength = ruleValue
              if (validation.message) errorMessages.maxlength = validation.message
            }
            break
          case 'min':
            if (field.type === 'number') html5Rules.min = ruleValue
            break
          case 'max':
            if (field.type === 'number') html5Rules.max = ruleValue
            break
          case 'email':
            if (validation.message) errorMessages.email = validation.message
            break
          case 'url':
            break
          case 'numeric':
            if (field.type === 'text') {
              html5Rules.pattern = '[0-9]+'
              if (validation.message) errorMessages.pattern = validation.message
            }
            break
          case 'alpha':
            if (field.type === 'text') {
              html5Rules.pattern = '[a-zA-Z]+'
              if (validation.message) errorMessages.pattern = validation.message
            }
            break
          case 'alphanumeric':
            if (field.type === 'text') {
              html5Rules.pattern = '[a-zA-Z0-9]+'
              if (validation.message) errorMessages.pattern = validation.message
            }
            break
          case 'accepted':
            if (field.type === 'checkbox') {
              html5Rules.required = true
              if (validation.message) errorMessages.required = validation.message
            }
            break
        }
      }
    }

    if (html5Rules.required) attrs += ' required'
    if (html5Rules.minlength) attrs += ` minlength="${html5Rules.minlength}"`
    if (html5Rules.maxlength) attrs += ` maxlength="${html5Rules.maxlength}"`
    if (html5Rules.min) attrs += ` min="${html5Rules.min}"`
    if (html5Rules.max) attrs += ` max="${html5Rules.max}"`
    if (html5Rules.pattern) attrs += ` pattern="${html5Rules.pattern}"`

    if (errorMessages.required) attrs += ` data-error-required="${errorMessages.required.replace(/"/g, '&quot;')}"`
    if (errorMessages.minlength) attrs += ` data-error-minlength="${errorMessages.minlength.replace(/"/g, '&quot;')}"`
    if (errorMessages.maxlength) attrs += ` data-error-maxlength="${errorMessages.maxlength.replace(/"/g, '&quot;')}"`
    if (errorMessages.pattern) attrs += ` data-error-pattern="${errorMessages.pattern.replace(/"/g, '&quot;')}"`
    if (errorMessages.email) attrs += ` data-error-email="${errorMessages.email.replace(/"/g, '&quot;')}"`

    return attrs
  }

  static extractLoginConfig(html, formToken) {
    const config = {
      token: formToken,
      redirect: null,
      submitText: 'Login',
      submitLoading: 'Logging in...',
      fields: []
    }

    const loginMatch = html.match(/<candy:login([^>]*)>/)
    if (!loginMatch) return config

    const loginTag = loginMatch[0]
    const redirectMatch = loginTag.match(/redirect=["']([^"']+)["']/)

    if (redirectMatch) config.redirect = redirectMatch[1]

    const submitMatch = html.match(/<candy:submit([^>/]*)(?:\/?>|>(.*?)<\/candy:submit>)/)
    if (submitMatch) {
      const submitTag = submitMatch[1]
      const textMatch = submitTag.match(/text=["']([^"']+)["']/)
      const loadingMatch = submitTag.match(/loading=["']([^"']+)["']/)
      const classMatch = submitTag.match(/class=["']([^"']+)["']/)
      const styleMatch = submitTag.match(/style=["']([^"']+)["']/)
      const idMatch = submitTag.match(/id=["']([^"']+)["']/)

      if (textMatch) config.submitText = textMatch[1]
      else if (submitMatch[2]) config.submitText = submitMatch[2].trim()

      if (loadingMatch) config.submitLoading = loadingMatch[1]
      if (classMatch) config.submitClass = classMatch[1]
      if (styleMatch) config.submitStyle = styleMatch[1]
      if (idMatch) config.submitId = idMatch[1]
    }

    const fieldMatches = html.match(/<candy:field[\s\S]*?<\/candy:field>/g)
    if (fieldMatches) {
      for (const fieldHtml of fieldMatches) {
        const field = this.parseField(fieldHtml)
        if (field) config.fields.push(field)
      }
    }

    return config
  }

  static storeLoginConfig(token, config, Candy) {
    if (!Candy.View) Candy.View = {}
    if (!Candy.View.loginForms) Candy.View.loginForms = {}

    const formData = {
      config: config,
      created: Date.now(),
      expires: Date.now() + 30 * 60 * 1000,
      sessionId: Candy.Request.session('_client'),
      userAgent: Candy.Request.header('user-agent'),
      ip: Candy.Request.ip
    }

    Candy.View.loginForms[token] = formData
    Candy.Request.session(`_login_form_${token}`, formData)
  }

  static generateLoginForm(originalHtml, config, formToken) {
    const submitText = config.submitText || 'Login'
    const submitLoading = config.submitLoading || 'Logging in...'

    let innerContent = originalHtml.replace(/<candy:login[^>]*>/, '').replace(/<\/candy:login>/, '')

    innerContent = innerContent.replace(/<candy:field[\s\S]*?<\/candy:field>/g, fieldMatch => {
      const field = this.parseField(fieldMatch)
      if (!field) return fieldMatch
      return this.generateFieldHtml(field)
    })

    const submitMatch = innerContent.match(/<candy:submit[\s\S]*?(?:<\/candy:submit>|\/?>)/)
    if (submitMatch) {
      let submitAttrs = `type="submit" data-submit-text="${submitText}" data-loading-text="${submitLoading}"`
      if (config.submitClass) submitAttrs += ` class="${config.submitClass}"`
      if (config.submitStyle) submitAttrs += ` style="${config.submitStyle}"`
      if (config.submitId) submitAttrs += ` id="${config.submitId}"`
      const submitButton = `<button ${submitAttrs}>${submitText}</button>`
      innerContent = innerContent.replace(submitMatch[0], submitButton)
    }

    let html = `<form class="candy-login-form" data-candy-login="${formToken}" method="POST" action="/_candy/login" novalidate>\n`
    html += `  <input type="hidden" name="_candy_login_token" value="${formToken}">\n`
    html += innerContent
    html += `\n  <span class="candy-form-success" style="display:none;"></span>\n`
    html += `</form>`

    return html
  }

  static extractFormConfig(html, formToken) {
    const config = {
      token: formToken,
      action: null,
      method: 'POST',
      submitText: 'Submit',
      submitLoading: 'Processing...',
      fields: [],
      sets: [],
      class: '',
      id: null,
      table: null,
      redirect: null,
      successMessage: null
    }

    const formMatch = html.match(/<candy:form([^>]*)>/)
    if (!formMatch) return config

    const formTag = formMatch[0]
    const extractAttr = name => {
      const match = formTag.match(new RegExp(`${name}=(['"])((?:(?!\\1).)*)\\1`))
      return match ? match[2] : null
    }

    const actionMatch = extractAttr('action')
    const methodMatch = extractAttr('method')
    const classMatch = extractAttr('class')
    const idMatch = extractAttr('id')
    const tableMatch = extractAttr('table')
    const redirectMatch = extractAttr('redirect')
    const successMatch = extractAttr('success')

    if (actionMatch) config.action = actionMatch
    if (methodMatch) config.method = methodMatch.toUpperCase()
    if (classMatch) config.class = classMatch
    if (idMatch) config.id = idMatch
    if (tableMatch) config.table = tableMatch
    if (redirectMatch) config.redirect = redirectMatch
    if (successMatch) config.successMessage = successMatch

    const submitMatch = html.match(/<candy:submit([^>/]*)(?:\/?>|>(.*?)<\/candy:submit>)/)
    if (submitMatch) {
      const submitTag = submitMatch[1]
      const textMatch = submitTag.match(/text=["']([^"']+)["']/)
      const loadingMatch = submitTag.match(/loading=["']([^"']+)["']/)
      const classMatch = submitTag.match(/class=["']([^"']+)["']/)
      const styleMatch = submitTag.match(/style=["']([^"']+)["']/)
      const idMatch = submitTag.match(/id=["']([^"']+)["']/)

      if (textMatch) config.submitText = textMatch[1]
      else if (submitMatch[2]) config.submitText = submitMatch[2].trim()

      if (loadingMatch) config.submitLoading = loadingMatch[1]
      if (classMatch) config.submitClass = classMatch[1]
      if (styleMatch) config.submitStyle = styleMatch[1]
      if (idMatch) config.submitId = idMatch[1]
    }

    const fieldMatches = html.match(/<candy:field[\s\S]*?<\/candy:field>/g)
    if (fieldMatches) {
      for (const fieldHtml of fieldMatches) {
        const field = this.parseField(fieldHtml)
        if (field) config.fields.push(field)
      }
    }

    const setMatches = html.match(/<candy:set[^>]*\/?>/g)
    if (setMatches) {
      for (const setTag of setMatches) {
        const set = this.parseSet(setTag)
        if (set) config.sets.push(set)
      }
    }

    return config
  }

  static storeFormConfig(token, config, Candy) {
    if (!Candy.View) Candy.View = {}
    if (!Candy.View.customForms) Candy.View.customForms = {}

    const formData = {
      config: config,
      created: Date.now(),
      expires: Date.now() + 30 * 60 * 1000,
      sessionId: Candy.Request.session('_client'),
      userAgent: Candy.Request.header('user-agent'),
      ip: Candy.Request.ip
    }

    Candy.View.customForms[token] = formData
    Candy.Request.session(`_custom_form_${token}`, formData)
  }

  static generateCustomForm(originalHtml, config, formToken) {
    const submitText = config.submitText || 'Submit'
    const submitLoading = config.submitLoading || 'Processing...'
    const action = config.action || '/_candy/form'
    const method = config.method || 'POST'

    let innerContent = originalHtml.replace(/<candy:form[^>]*>/, '').replace(/<\/candy:form>/, '')

    innerContent = innerContent.replace(/<candy:field[\s\S]*?<\/candy:field>/g, fieldMatch => {
      const field = this.parseField(fieldMatch)
      if (!field) return fieldMatch
      return this.generateFieldHtml(field)
    })

    const escapeHtml = str =>
      String(str).replace(/[&<>"']/g, m => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'})[m])

    const submitMatch = innerContent.match(/<candy:submit[\s\S]*?(?:<\/candy:submit>|\/?>)/)
    if (submitMatch) {
      let submitAttrs = `type="submit" data-submit-text="${escapeHtml(submitText)}" data-loading-text="${escapeHtml(submitLoading)}"`
      if (config.submitClass) submitAttrs += ` class="${escapeHtml(config.submitClass)}"`
      if (config.submitStyle) submitAttrs += ` style="${escapeHtml(config.submitStyle)}"`
      if (config.submitId) submitAttrs += ` id="${escapeHtml(config.submitId)}"`
      const submitButton = `<button ${submitAttrs}>${escapeHtml(submitText)}</button>`
      innerContent = innerContent.replace(submitMatch[0], submitButton)
    }

    innerContent = innerContent.replace(/<candy:set[^>]*\/?>/g, '')

    let formAttrs = `class="candy-custom-form${config.class ? ' ' + escapeHtml(config.class) : ''}" data-candy-form="${escapeHtml(formToken)}" method="${escapeHtml(method)}" action="${escapeHtml(action)}" novalidate`
    if (config.id) formAttrs += ` id="${escapeHtml(config.id)}"`

    let html = `<form ${formAttrs}>\n`
    html += `  <input type="hidden" name="_candy_form_token" value="${escapeHtml(formToken)}">\n`
    html += innerContent
    html += `\n  <span class="candy-form-success" style="display:none;"></span>\n`
    html += `</form>`

    return html
  }
}

module.exports = Form
