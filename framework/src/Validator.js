class Validator {
  #checklist = {}
  #completed = false
  #message = {}
  #method = 'POST'
  #name = ''
  #request

  constructor(Request) {
    this.#request = Request
  }

  check(rules) {
    if (!this.#checklist[this.#method]) this.#checklist[this.#method] = {}
    if (!this.#checklist[this.#method][this.#name]) this.#checklist[this.#method][this.#name] = []
    this.#checklist[this.#method][this.#name].push({rules: rules, message: null})
    return this
  }

  async error() {
    if (!this.#completed) await this.#validate()
    return Object.keys(this.#message).length > 0
  }

  get(key) {
    if (this.#completed) this.#completed = false
    this.#method = 'GET'
    this.#name = key
    return this
  }

  message(value) {
    const checks = this.#checklist[this.#method][this.#name]
    if (checks && checks.length > 0) {
      checks[checks.length - 1].message = value
    }
    return this
  }

  post(key) {
    if (this.#completed) this.#completed = false
    this.#method = 'POST'
    this.#name = key
    return this
  }

  async result(message, data) {
    if (!this.#completed) await this.#validate()
    let result = {}
    result.result = {}
    result.result.success = Object.keys(this.#message).length === 0
    if (result.result.success) {
      result.result.message = message ?? ''
      result.data = data ?? null
    } else {
      result.errors = this.#message['_odac_form'] ? {_odac_form: this.#message['_odac_form']} : this.#message
    }
    return result
  }

  success(callback) {
    if (typeof callback === 'string') return this.result(callback)
    else return this.result(null, callback)
  }

  async #validate() {
    for (const method of Object.keys(this.#checklist)) {
      for (const key of Object.keys(this.#checklist[method])) {
        const checks = this.#checklist[method][key]
        let value

        if (method === 'VAR') {
          value = checks.customValue
        } else if (method === 'FILES') {
          value = this.#request.file ? await this.#request.file(key) : null
        } else {
          value = await this.#request.request(key, method)
        }

        for (const checkItem of checks) {
          if (this.#message[key]) break

          let error = false
          let rules = checkItem.rules

          if (typeof rules === 'boolean') {
            error = rules === false
          } else {
            for (const rule of rules.includes('|') ? rules.split('|') : [rules]) {
              let vars = rule.split(':')
              let inverse = vars[0].startsWith('!')
              if (!error) {
                switch (inverse ? vars[0].substr(1) : vars[0]) {
                  case 'required':
                    error = value === undefined || value === '' || value === null
                    break
                  case 'accepted':
                    error = !value || (value !== 1 && value !== '1' && value !== 'on' && value !== 'yes' && value !== true)
                    break
                  case 'numeric':
                    error = value && value !== '' && !Odac.Var(value).is('numeric')
                    break
                  case 'alpha':
                    error = value && value !== '' && !Odac.Var(value).is('alpha')
                    break
                  case 'alphaspace':
                    error = value && value !== '' && !Odac.Var(value).is('alphaspace')
                    break
                  case 'alphanumeric':
                    error = value && value !== '' && !Odac.Var(value).is('alphanumeric')
                    break
                  case 'alphanumericspace':
                    error = value && value !== '' && !Odac.Var(value).is('alphanumericspace')
                    break
                  case 'email':
                    error = value && value !== '' && !Odac.Var(value).is('email')
                    break
                  case 'ip':
                    error = value && value !== '' && !Odac.Var(value).is('ip')
                    break
                  case 'float':
                    error = value && value !== '' && !Odac.Var(value).is('float')
                    break
                  case 'mac':
                    error = value && value !== '' && !Odac.Var(value).is('mac')
                    break
                  case 'domain':
                    error = value && value !== '' && !Odac.Var(value).is('domain')
                    break
                  case 'url':
                    error = value && value !== '' && !Odac.Var(value).is('url')
                    break
                  case 'username':
                    error = value && value !== '' && !/^[a-zA-Z0-9]+$/.test(value)
                    break
                  case 'xss':
                    error = value && value !== '' && /<[^>]*>/g.test(value)
                    break
                  case 'usercheck':
                    error = !(await Odac.Auth.check())
                    break
                  case 'array':
                    error = value && !Array.isArray(value)
                    break
                  case 'date':
                    error = value && value !== '' && isNaN(Date.parse(value))
                    break
                  case 'min':
                    error = value && value !== '' && vars[1] && value < vars[1]
                    break
                  case 'max':
                    error = value && value !== '' && vars[1] && value > vars[1]
                    break
                  case 'len':
                    error = value && value !== '' && vars[1] && String(value).length !== parseInt(vars[1])
                    break
                  case 'minlen':
                    error = value && value !== '' && vars[1] && String(value).length < parseInt(vars[1])
                    break
                  case 'maxlen':
                    error = value && value !== '' && vars[1] && String(value).length > parseInt(vars[1])
                    break
                  case 'mindate':
                    error = value && value !== '' && vars[1] && new Date(value).getTime() < new Date(vars[1]).getTime()
                    break
                  case 'maxdate':
                    error = value && value !== '' && vars[1] && new Date(value).getTime() > new Date(vars[1]).getTime()
                    break
                  case 'same': {
                    const otherValue = await this.#request.request(vars[1], method)
                    error = value !== otherValue
                    break
                  }
                  case 'different': {
                    const otherValue = await this.#request.request(vars[1], method)
                    error = value === otherValue
                    break
                  }
                  case 'equal':
                    error = value && vars[1] && value !== vars[1]
                    break
                  case 'notin':
                    error = value && value !== '' && vars[1] && String(value).includes(vars[1])
                    break
                  case 'in':
                    error = value && value !== '' && vars[1] && !String(value).includes(vars[1])
                    break
                  case 'not':
                    error = value && vars[1] && value === vars[1]
                    break
                  case 'regex':
                    error = value && value !== '' && vars[1] && !new RegExp(vars[1]).test(value)
                    break
                  case 'user': {
                    if (!(await Odac.Auth.check())) {
                      error = true
                    } else {
                      const userData = Odac.Auth.user(vars[1])
                      if (Odac.Var(userData).is('bcrypt')) {
                        error = !Odac.Var(userData).hashCheck(value)
                      } else {
                        error = value !== userData
                      }
                    }
                    break
                  }
                }
                if (inverse) error = !error
              }
            }
          }

          if (error) {
            this.#message[key] = checkItem.message
            break
          }
        }
      }
    }
    this.#completed = true
  }

  var(name, value = null) {
    if (this.#completed) this.#completed = false
    this.#method = 'VAR'
    this.#name = name
    if (!this.#checklist[this.#method]) this.#checklist[this.#method] = {}
    if (!this.#checklist[this.#method][name]) {
      this.#checklist[this.#method][name] = []
      this.#checklist[this.#method][name].customValue = value === null ? name : value
    }
    return this
  }

  file(name) {
    if (this.#completed) this.#completed = false
    this.#method = 'FILES'
    this.#name = name
    return this
  }

  async brute(maxAttempts = 5) {
    const ip = this.#request.ip()
    const now = new Date().toISOString().slice(0, 13).replace(/[-:T]/g, '')
    const page = this.#request.path()
    const storage = Odac.storage('sys')
    const validation = storage.get('validation') || {}

    this.#name = '_odac_form'

    if (Object.keys(this.#message).length > 0) {
      if (!validation.brute) validation.brute = {}
      if (!validation.brute[now]) validation.brute[now] = {}
      if (!validation.brute[now][page]) validation.brute[now][page] = {}
      if (!validation.brute[now][page][ip]) validation.brute[now][page][ip] = 0

      validation.brute[now][page][ip]++

      if (validation.brute[now][page][ip] >= maxAttempts) {
        this.#message['_odac_form'] = Odac.Lang
          ? Odac.Lang.get('Too many failed attempts. Please try again later.')
          : 'Too many failed attempts. Please try again later.'
      }
    }

    storage.set('validation', validation)
    return this
  }
}

module.exports = Validator
