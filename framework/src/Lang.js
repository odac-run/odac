const fs = require('fs')

class Lang {
  #odac
  #data = {}
  #lang

  constructor(Odac) {
    this.#odac = Odac
    this.set()
  }

  get(...args) {
    if (typeof args[0] !== 'string') return args[0]
    if (!this.#data[args[0]]) {
      this.#data[args[0]] = args[0]
      this.#save()
    }
    let str = this.#data[args[0]]

    // Support both %s (sequential) and %s1, %s2 (numbered) placeholders
    const hasNumberedPlaceholders = /%s\d+/.test(str)

    if (hasNumberedPlaceholders) {
      for (let i = 1; i < args.length; i++) {
        const numberedPattern = new RegExp(`%s${i}`, 'g')
        str = str.replace(numberedPattern, args[i])
      }
    } else {
      for (let i = 1; i < args.length; i++) {
        str = str.replace('%s', args[i])
      }
    }
    return str
  }

  #save() {
    if (!this.#lang) return
    if (!fs.existsSync(__dir + '/storage/')) fs.mkdirSync(__dir + '/storage/')
    if (!fs.existsSync(__dir + '/storage/language/')) fs.mkdirSync(__dir + '/storage/language/')
    fs.writeFileSync(__dir + '/storage/language/' + this.#lang + '.json', JSON.stringify(this.#data, null, 4))
  }

  set(lang) {
    if (!lang || lang.length !== 2 || !this.#odac.Var(lang).is('alpha')) {
      if (this.#odac.Request.header('ACCEPT-LANGUAGE') && this.#odac.Request.header('ACCEPT-LANGUAGE').length > 1)
        lang = this.#odac.Request.header('ACCEPT-LANGUAGE').substr(0, 2)
      else lang = this.#odac.Config.lang?.default || 'en'
    }
    this.#lang = lang
    if (fs.existsSync(__dir + '/storage/language/' + lang + '.json'))
      this.#data = JSON.parse(fs.readFileSync(__dir + '/storage/language/' + lang + '.json'))
    else this.#data = {}
  }
}

module.exports = Lang
