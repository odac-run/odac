const fs = require('fs')
const nodeCrypto = require('crypto')
const bcrypt = require('bcrypt')

class Var {
  #value = null
  #any = false

  constructor(value) {
    this.#value = value
  }

  clear(...args) {
    args = this.#parse(args)
    let str = this.#value
    for (const arg of args) str = str.replace(new RegExp(arg, 'g'), '')
    return str
  }

  contains(...args) {
    args = this.#parse(args)
    let any = this.#any
    this.#any = false
    let result = !any
    for (const key of args) {
      if (any) result = result || this.#value.includes(key)
      else result = result && this.#value.includes(key)
    }
    return result
  }

  containsAny(...args) {
    args = this.#parse(args)
    this.#any = true
    return this.contains(args)
  }

  date(format) {
    if (!format) format = 'Y-m-d H:i:s'
    let date = new Date(this.#value)
    let year = date.getFullYear()
    let month = date.getMonth() + 1
    let day = date.getDate()
    let hour = date.getHours()
    let minute = date.getMinutes()
    let second = date.getSeconds()
    return Odac.Var(format).replace({
      Y: year,
      m: month < 10 ? `0${month}` : month,
      d: day < 10 ? `0${day}` : day,
      H: hour < 10 ? `0${hour}` : hour,
      i: minute < 10 ? `0${minute}` : minute,
      s: second < 10 ? `0${second}` : second,
      y: year.toString().substr(2, 2)
    })
  }

  decrypt(key) {
    if (!key) key = Odac.Config.encrypt.key
    const iv = '2dea8a25e5e8f004'
    try {
      const encryptedText = Buffer.from(this.#value, 'base64')
      const decipher = nodeCrypto.createDecipheriv('aes-256-cbc', key, iv)
      let decrypted = decipher.update(encryptedText)
      decrypted = Buffer.concat([decrypted, decipher.final()])
      return decrypted.toString()
    } catch (e) {
      console.log(e)
      return null
    }
  }

  encrypt(key) {
    if (!key) key = Odac.Config.encrypt.key
    const iv = '2dea8a25e5e8f004'
    const cipher = nodeCrypto.createCipheriv('aes-256-cbc', key, iv)
    let encrypted = cipher.update(this.#value)
    encrypted = Buffer.concat([encrypted, cipher.final()])
    return encrypted.toString('base64')
  }

  hash(salt = 10) {
    return bcrypt.hashSync(this.#value, bcrypt.genSaltSync(salt))
  }

  hashCheck(check) {
    return bcrypt.compareSync(check, this.#value)
  }

  html() {
    if (this.#value === null || this.#value === undefined) return ''
    return String(this.#value).replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }

  is(...args) {
    args = this.#parse(args)
    let any = this.#any
    this.#any = false
    let result = !any
    //     if(\Odac::config('locale')->get() == 'tr') $this->str = \Odac::var($this->str)->clear('Ç','ç','Ğ','ğ','İ','ı','Ö','ö','Ş','ş','Ü','ü');
    if (args.includes('alpha')) result = (result || any) && ((any && result) || /^[A-Za-z]+$/.test(this.#value))
    if (args.includes('alphaspace')) result = (result || any) && ((any && result) || /^[A-Za-z\s]+$/.test(this.#value))
    if (args.includes('alphanumeric')) result = (result || any) && ((any && result) || /^[A-Za-z0-9]+$/.test(this.#value))
    if (args.includes('alphanumericspace')) result = (result || any) && ((any && result) || /^[A-Za-z0-9\s]+$/.test(this.#value))
    if (args.includes('bcrypt')) result = (result || any) && ((any && result) || /^\$2[ayb]\$.{56}$/.test(this.#value))
    if (args.includes('date')) result = (result || any) && ((any && result) || !isNaN(Date.parse(this.#value)))
    if (args.includes('domain')) result = (result || any) && ((any && result) || /^([a-z0-9-]+\.){1,2}[a-z]{2,6}$/i.test(this.#value))
    if (args.includes('email'))
      result = (result || any) && ((any && result) || /^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$/i.test(this.#value))
    if (args.includes('float')) result = (result || any) && ((any && result) || !isNaN(parseFloat(this.#value)))
    if (args.includes('host')) result = (result || any) && ((any && result) || /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/.test(this.#value))
    if (args.includes('ip')) result = (result || any) && ((any && result) || /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/.test(this.#value))
    if (args.includes('json'))
      result = (result || any) && ((any && result) || (JSON.parse(this.#value) && JSON.parse(this.#value).length >= 0))
    if (args.includes('mac')) result = (result || any) && ((any && result) || /^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$/.test(this.#value))
    if (args.includes('md5')) result = (result || any) && ((any && result) || /^[a-f0-9A-F]{32}$/.test(this.#value))
    if (args.includes('numeric')) result = (result || any) && ((any && result) || !isNaN(this.#value))
    if (args.includes('url')) result = (result || any) && ((any && result) || /^[a-z0-9]+:\/\/[a-z0-9]+\.[a-z]{2,6}/i.test(this.#value))
    if (args.includes('emoji'))
      result =
        (result || any) &&
        ((any && result) ||
          /([0-9#][\u20E3])|[\u00ae\u00a9\u203C\u2047\u2048\u2049\u3030\u303D\u2139\u2122\u3297\u3299][\uFE00-\uFEFF]?|[\u2190-\u21FF][\uFE00-\uFEFF]?|[\u2300-\u23FF][\uFE00-\uFEFF]?|[\u2460-\u24FF][\uFE00-\uFEFF]?|[\u25A0-\u25FF][\uFE00-\uFEFF]?|[\u2600-\u27BF][\uFE00-\uFEFF]?|[\u2900-\u297F][\uFE00-\uFEFF]?|[\u2B00-\u2BF0][\uFE00-\uFEFF]?|[\u1F000-\u1F6FF][\uFE00-\uFEFF]?/u.test(
            this.#value
          ))
    if (args.includes('xss')) result = (result || any) && ((any && result) || this.#value == this.#value.replace(/<[^>]*>/g, ''))
    return result
  }

  isAny(...args) {
    args = this.#parse(args)
    this.#any = true
    return this.is(args)
  }

  isBegin(...args) {
    args = this.#parse(args)
    for (const arg of args) if (this.#value.startsWith(arg)) return true
    return false
  }

  isEnd(...args) {
    args = this.#parse(args)
    for (const arg of args) if (this.#value.endsWith(arg)) return true
    return false
  }

  md5() {
    return nodeCrypto.createHash('md5').update(this.#value).digest('hex')
  }

  #parse(value) {
    if (!['array', 'object'].includes(typeof value)) return [value]
    if (value.length == 1 && Array.isArray(value[0])) return value[0]
    return value
  }

  replace(...args) {
    args = this.#parse(args)
    if (args.length == 1) args = args[0]
    if (['array', 'object'].includes(typeof this.#value)) {
      let new_value = {}
      for (const key of Object.keys(this.#value)) new_value[key] = Odac.Var(this.#value[key]).replace(args)
      return new_value
    }
    for (const arg of Object.keys(args)) this.#value = this.#value.replace(arg, args[arg])
    return this.#value
  }

  save(path) {
    if (this.#value.includes('/')) {
      let exp = path.split('/')
      exp.pop()
      let dir = ''
      for (const key of exp) {
        dir += (dir === '' ? '' : '/') + key
        if (!fs.existsSync(dir) || !fs.lstatSync(dir).isDirectory()) fs.mkdirSync(dir)
      }
    }
    return fs.writeFileSync(path, this.#value)
  }

  slug(separator = '-') {
    let str = this.#value
    str = str.replace(/[^a-zA-Z0-9\s]/g, separator)
    str = str.replace(/[\s]/g, separator)
    str = str.replace(/[-]+/g, separator)
    str = str.toLowerCase()
    return str
  }

  format(format) {
    let str = this.#value
    let result = ''
    let letter = 0
    for (let i = 0; i < format.length; i++) {
      if (format[i] == '?') {
        result += str[letter]
        letter++
      } else if (format[i] == '*') {
        result += str.substr(letter)
        letter += str.substr(letter).length
      } else {
        result += format[i]
      }
    }
    return result
  }
}

module.exports = Var
