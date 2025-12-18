const axios = require('axios')
const fs = require('fs')

class Mail {
  #header = {}
  #from
  #subject = ''
  #template
  #to

  constructor(template) {
    this.#template = template
  }

  header(header) {
    this.#header = header
    return this
  }

  from(email, name) {
    this.#from = {email: email, name: name}
    return this
  }

  subject(subject) {
    this.#subject = subject
    return this
  }

  to(email) {
    email = {value: [{address: email}]}
    this.#to = email
    return this
  }

  send(data) {
    return new Promise(resolve => {
      if (!fs.existsSync(__dir + '/view/mail/' + this.#template + '.html')) return console.log('Template not found') && false
      if (!this.#from || !this.#subject || !this.#to) return console.log('From, Subject and To fields are required') && false
      if (!Odac.Var(this.#from.email).is('email')) return console.log('From field is not a valid e-mail address') && false
      if (!Odac.Var(this.#to.value[0].address).is('email')) return console.log('To field is not a valid e-mail address') && false
      if (!this.#header['From']) this.#header['From'] = `${this.#from.name} <${this.#from.email}>`
      if (!this.#header['To']) this.#header['To'] = this.#to
      if (!this.#header['Subject']) this.#header['Subject'] = this.#subject
      if (!this.#header['Message-ID']) this.#header['Message-ID'] = `<${crypto.randomBytes(16).toString('hex')}-${Date.now()}@odac>`
      if (!this.#header['Content-Transfer-Encoding']) this.#header['Content-Transfer-Encoding'] = 'quoted-printable'
      if (!this.#header['Date']) this.#header['Date'] = new Date().toUTCString()
      if (!this.#header['Content-Type'])
        this.#header['Content-Type'] = 'multipart/alternative; boundary="----=' + crypto.randomBytes(32).toString('hex') + '"'
      if (!this.#header['X-Mailer']) this.#header['X-Mailer'] = 'Odac'
      if (!this.#header['MIME-Version']) this.#header['MIME-Version'] = '1.0'
      let content = fs.readFileSync(__dir + '/view/mail/' + this.#template + '.html').toString()
      for (const iterator of Object.keys(data)) content = content.replace(new RegExp(`{${iterator}}`, 'g'), data[iterator])
      axios
        .post(
          'http://127.0.0.1:1453',
          {
            action: 'mail.send',
            data: [
              {
                subject: this.#subject,
                from: {value: [{address: this.#from.email, name: this.#from.name}]},
                to: this.#to,
                header: this.#header,
                html: content,
                text: content.replace(/<[^>]*>?/gm, '')
              }
            ]
          },
          {headers: {Authorization: Odac.Config.system.api.auth}}
        )
        .then(response => {
          resolve(response.data)
        })
        .catch(error => {
          console.log(error)
          resolve(false)
        })
    })
  }
}

module.exports = Mail
