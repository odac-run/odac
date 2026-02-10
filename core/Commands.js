require('./Odac.js')

const path = require('path')

module.exports = {
  auth: {
    args: ['key', '-k', '--key'],
    description: 'Define your server to your ODAC account',
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
    description: 'Debug ODAC Server',
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
    description: 'Restart ODAC Server',
    action: async () => Odac.cli('Cli').boot()
  },
  update: {
    description: 'Update ODAC Server',
    action: async () => Odac.cli('Connector').call({action: 'update'})
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
        args: ['-t', '--type', '-n', '--name', '-u', '--url', '-b', '--branch', '--token', '-D', '--dev'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let type = cli.parseArg(args, ['-t', '--type'])

          if (!type) {
            // Find first non-flag argument to be used as type/repo
            type = args.find(arg => !arg.startsWith('-'))
          }

          const isDev = args.includes('-D') || args.includes('--dev')

          // Interactive: Logic to handle git vs standard app
          if (type === 'git' || type === 'github') {
            const url = cli.parseArg(args, ['-u', '--url']) || (await cli.question(__('Enter Git URL: ')))
            const branch = cli.parseArg(args, ['-b', '--branch'])
            const token = cli.parseArg(args, ['--token'])

            // Auto-derive name from URL if not provided
            let name = cli.parseArg(args, ['-n', '--name'])
            if (!name) {
              const path = require('path')
              name = path.basename(url, '.git').replace(/[^a-zA-Z0-9-]/g, '-')
            }

            const config = {
              type: 'git',
              url,
              name,
              branch,
              token,
              dev: isDev
            }

            await Odac.cli('Connector').call({
              action: 'app.create',
              data: [config]
            })
            return
          }

          if (!type) {
            type = await cli.question(__('Enter the app type or repo: '))
          }

          let config = type

          // Auto-detect Git URL
          if (typeof type === 'string' && (/^(https?|git|ssh):\/\//.test(type) || /^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/.test(type))) {
            const path = require('path')
            const name = path.basename(type, '.git').replace(/[^a-zA-Z0-9-]/g, '-')

            config = {
              type: 'git',
              url: type,
              name,
              dev: isDev
            }
          } else if (isDev && typeof type === 'string') {
            // Non-git app with dev flag
            config = {type: 'app', app: type, dev: true}
          }

          await Odac.cli('Connector').call({
            action: 'app.create',
            data: [config]
          })
        }
      },
      restart: {
        description: 'Restart an App',
        args: ['-i', '--id'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let app = cli.parseArg(args, ['-i', '--id']) || args[0]
          if (!app) app = await cli.question(__('Enter the App ID or Name: '))
          await Odac.cli('Connector').call({action: 'app.restart', data: [app]})
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
  domain: {
    title: 'DOMAIN',
    sub: {
      add: {
        description: 'Add a domain to an application',
        args: ['-d', '--domain', '-a', '--app'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain']) || args[0]
          let app = cli.parseArg(args, ['-a', '--app']) || args[1]

          if (!domain) domain = await cli.question(__('Enter the domain name: '))
          if (!app) app = await cli.question(__('Enter the App ID or Name: '))

          await Odac.cli('Connector').call({
            action: 'domain.add',
            data: [domain, app]
          })
        }
      },
      delete: {
        description: 'Delete a domain',
        args: ['-d', '--domain'],
        action: async args => {
          const cli = Odac.cli('Cli')
          let domain = cli.parseArg(args, ['-d', '--domain']) || args[0]

          if (!domain) domain = await cli.question(__('Enter the domain name: '))

          await Odac.cli('Connector').call({
            action: 'domain.delete',
            data: [domain]
          })
        }
      },
      list: {
        description: 'List all domains',
        args: ['-a', '--app'],
        action: async args => {
          const cli = Odac.cli('Cli')
          const app = cli.parseArg(args, ['-a', '--app']) || (typeof args[0] === 'string' ? args[0] : undefined)

          await Odac.cli('Connector').call({
            action: 'domain.list',
            data: app ? [app] : []
          })
        }
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
