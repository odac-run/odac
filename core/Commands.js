require('./Odac.js')

const path = require('path')

module.exports = {
  auth: {
    args: ['key', '-k', '--key'],
    description: 'Define your server to your Odac account',
    action: async args => {
      const cli = Odac.cli('Cli')
      let key = cli.parseArg(args, ['-k', '--key']) || args[0]
      if (!key) key = await cli.question(__('Enter your authentication key: '))

      await Odac.cli('Connector').call({
        action: 'auth',
        data: [key]
      })
    }
  },
  debug: {
    description: 'Debug Odac Server',
    action: async () => Odac.cli('Monitor').debug()
  },
  help: {
    description: 'List all available commands',
    action: async () => Odac.cli('Cli').help()
  },
  monit: {
    description: 'Monitor Website or Service',
    action: async () => Odac.cli('Monitor').monit()
  },
  restart: {
    description: 'Restart Odac Server',
    action: async () => Odac.cli('Cli').boot()
  },
  run: {
    args: ['file'],
    description: 'Run a script or file as a service',
    action: async args => {
      let filePath = args[0]
      if (!filePath) return console.log(__('Please specify a file to run.'))

      // Check for Windows path manually to support cross-platform tests
      const isWindowsAbsolute = /^[a-zA-Z]:\\|^\\\\/.test(filePath)

      if (!path.isAbsolute(filePath) && !isWindowsAbsolute) {
        filePath = path.resolve(filePath)
      }
      await Odac.cli('Connector').call({action: 'app.start', data: [filePath]})
    }
  },

  app: {
    title: 'APP',
    sub: {
      create: {
        description: 'Create a new application',
        args: ['-t', '--type'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let type = cli.parseArg(args, ['-t', '--type']) || args[0]

          if (!type) {
            type = await cli.question(__('Enter the app type or repo: '))
          }

          await Odac.cli('Connector').call({
            action: 'app.create',
            data: [type]
          })
        }
      },
      delete: {
        description: 'Delete an App',
        args: ['-i', '--id'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let app = cli.parseArg(args, ['-i', '--id']) || args[0]
          if (!app) app = await cli.question(__('Enter the App ID or Name: '))
          await Odac.cli('Connector').call({action: 'app.delete', data: [app]})
        }
      },
      list: {
        description: 'List all apps',
        action: async () => Odac.cli('Connector').call({action: 'app.list'})
      }
    }
  },
  mail: {
    title: 'MAIL',
    sub: {
      create: {
        description: 'Create a new mail account',
        args: ['-e', '--email', '-p', '--password'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let email = cli.parseArg(args, ['-e', '--email'])
          let password = cli.parseArg(args, ['-p', '--password'])

          if (!email) email = await cli.question(__('Enter the e-mail address: '))
          if (!password) password = await cli.question(__('Enter the password: '))

          let confirmPassword = password
          if (!cli.parseArg(args, ['-p', '--password'])) {
            confirmPassword = await cli.question(__('Re-enter the password: '))
          }

          await Odac.cli('Connector').call({
            action: 'mail.create',
            data: [email, password, confirmPassword]
          })
        }
      },
      delete: {
        description: 'Delete a mail account',
        args: ['-e', '--email'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let email = cli.parseArg(args, ['-e', '--email'])
          if (!email) email = await cli.question(__('Enter the e-mail address: '))

          await Odac.cli('Connector').call({
            action: 'mail.delete',
            data: [email]
          })
        }
      },
      list: {
        description: 'List all domain mail accounts',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain'])
          if (!domain) domain = await cli.question(__('Enter the domain name: '))

          await Odac.cli('Connector').call({action: 'mail.list', data: [domain]})
        }
      },
      password: {
        description: 'Change mail account password',
        args: ['-e', '--email', '-p', '--password'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let email = cli.parseArg(args, ['-e', '--email'])
          let password = cli.parseArg(args, ['-p', '--password'])

          if (!email) email = await cli.question(__('Enter the e-mail address: '))
          if (!password) password = await cli.question(__('Enter the new password: '))

          let confirmPassword = password
          if (!cli.parseArg(args, ['-p', '--password'])) {
            confirmPassword = await cli.question(__('Re-enter the new password: '))
          }

          await Odac.cli('Connector').call({
            action: 'mail.password',
            data: [email, password, confirmPassword]
          })
        }
      }
    }
  },
  ssl: {
    title: 'SSL',
    sub: {
      renew: {
        description: 'Renew SSL certificate for a domain',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain'])
          if (!domain) domain = await cli.question(__('Enter the domain name: '))

          await Odac.cli('Connector').call({action: 'ssl.renew', data: [domain]})
        }
      }
    }
  },
  subdomain: {
    title: 'SUBDOMAIN',
    sub: {
      create: {
        description: 'Create a new subdomain',
        args: ['-s', '--subdomain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let subdomain = cli.parseArg(args, ['-s', '--subdomain'])
          if (!subdomain) subdomain = await cli.question(__('Enter the subdomain name (subdomain.example.com): '))

          await Odac.cli('Connector').call({
            action: 'subdomain.create',
            data: [subdomain]
          })
        }
      },
      delete: {
        description: 'Delete a subdomain',
        args: ['-s', '--subdomain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let subdomain = cli.parseArg(args, ['-s', '--subdomain'])
          if (!subdomain) subdomain = await cli.question(__('Enter the subdomain name (subdomain.example.com): '))

          await Odac.cli('Connector').call({
            action: 'subdomain.delete',
            data: [subdomain]
          })
        }
      },
      list: {
        description: 'List all domain subdomains',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain'])
          if (!domain) domain = await cli.question(__('Enter the domain name: '))

          await Odac.cli('Connector').call({
            action: 'subdomain.list',
            data: [domain]
          })
        }
      }
    }
  },
  web: {
    title: 'WEBSITE',
    sub: {
      create: {
        description: 'Create a new website',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain'])
          if (!domain) {
            domain = await cli.question(__('Enter the domain name: '))
          }
          await Odac.cli('Connector').call({
            action: 'web.create',
            data: [domain]
          })
        }
      },
      delete: {
        description: 'Delete a website',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain'])
          if (!domain) {
            domain = await cli.question(__('Enter the domain name: '))
          }
          await Odac.cli('Connector').call({
            action: 'web.delete',
            data: [domain]
          })
        }
      },
      list: {
        description: 'List all websites',
        action: async () => await Odac.cli('Connector').call({action: 'web.list'})
      }
    }
  }
}
