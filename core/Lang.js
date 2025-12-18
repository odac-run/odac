const fs = require('fs')

class Lang {
  #locale = Intl.DateTimeFormat().resolvedOptions().locale
  #file = __dirname + '/../locale/' + this.#locale + '.json'
  #strings = {}
  #loaded = false

  constructor() {
    this.#load()
  }

  #save() {
    try {
      fs.promises.writeFile(this.#file, JSON.stringify(this.#strings, null, 4), 'utf8')
    } catch (err) {
      console.error('Error saving language file:', err)
    }
  }

  #load() {
    try {
      const data = fs.readFileSync(this.#file, 'utf8')
      this.#loaded = true
      this.#strings = JSON.parse(data)
    } catch {
      this.#strings = {}
      this.#save()
    }
    return true
  }

  get(key, ...args) {
    if (!this.#loaded) this.#load()
    if (key === 'Odac') return 'Odac'
    let text = this.#strings[key]
    if (text === undefined) {
      text = key
      this.#strings[key] = text
      this.#save()
    }
    if (args.length > 0) {
      args.forEach((arg, i) => {
        if (text.includes(`%s${i + 1}`)) text = text.replace(`%s${i + 1}`, arg)
        else text = text.replace('%s', arg)
      })
    }
    return text
  }
}

module.exports = Lang
