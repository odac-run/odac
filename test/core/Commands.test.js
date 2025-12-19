// Mock the Odac global before requiring Commands
global.Odac = {
  cli: jest.fn(),
  core: jest.fn()
}

// Mock the global __ function
global.__ = jest.fn(key => key)

const Commands = require('../../core/Commands')

describe('Commands', () => {
  let mockCli, mockConnector, mockMonitor

  beforeEach(() => {
    mockCli = {
      parseArg: jest.fn(),
      question: jest.fn(),
      help: jest.fn(),
      boot: jest.fn()
    }

    mockConnector = {
      call: jest.fn()
    }

    mockMonitor = {
      debug: jest.fn(),
      monit: jest.fn()
    }

    global.Odac.cli.mockImplementation(name => {
      switch (name) {
        case 'Cli':
          return mockCli
        case 'Connector':
          return mockConnector
        case 'Monitor':
          return mockMonitor
        default:
          return {}
      }
    })

    global.Odac.core.mockImplementation(name => {
      if (name === 'Lang') {
        return {get: key => key}
      }
      return {}
    })

    // Clear all mocks
    jest.clearAllMocks()
  })

  describe('command structure', () => {
    it('should have all expected top-level commands', () => {
      expect(Commands.auth).toBeDefined()
      expect(Commands.debug).toBeDefined()
      expect(Commands.help).toBeDefined()
      expect(Commands.monit).toBeDefined()
      expect(Commands.restart).toBeDefined()
      expect(Commands.run).toBeDefined()
      expect(Commands.mail).toBeDefined()
      expect(Commands.ssl).toBeDefined()
      expect(Commands.subdomain).toBeDefined()
      expect(Commands.web).toBeDefined()
    })

    it('should have correct command descriptions', () => {
      expect(Commands.auth.description).toBe('Define your server to your Odac account')
      expect(Commands.debug.description).toBe('Debug Odac Server')
      expect(Commands.help.description).toBe('List all available commands')
      expect(Commands.monit.description).toBe('Monitor Website or Service')
      expect(Commands.restart.description).toBe('Restart Odac Server')
      expect(Commands.run.description).toBe('Add a new Service')
    })
  })

  describe('auth command', () => {
    it('should use provided key argument', async () => {
      const args = ['test-key']
      mockCli.parseArg.mockReturnValue('test-key')

      await Commands.auth.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'auth',
        data: ['test-key']
      })
    })

    it('should prompt for key if not provided', async () => {
      const args = []
      mockCli.parseArg.mockReturnValue(null)
      mockCli.question.mockResolvedValue('prompted-key')

      await Commands.auth.action(args)

      expect(mockCli.question).toHaveBeenCalledWith('Enter your authentication key: ')
      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'auth',
        data: ['prompted-key']
      })
    })
  })

  describe('debug command', () => {
    it('should call monitor debug', async () => {
      await Commands.debug.action()
      expect(mockMonitor.debug).toHaveBeenCalled()
    })
  })

  describe('help command', () => {
    it('should call cli help', async () => {
      await Commands.help.action()
      expect(mockCli.help).toHaveBeenCalled()
    })
  })

  describe('monit command', () => {
    it('should call monitor monit', async () => {
      await Commands.monit.action()
      expect(mockMonitor.monit).toHaveBeenCalled()
    })
  })

  describe('restart command', () => {
    it('should call cli boot', async () => {
      await Commands.restart.action()
      expect(mockCli.boot).toHaveBeenCalled()
    })
  })

  describe('run command', () => {
    it('should handle absolute path', async () => {
      const args = ['/absolute/path/service.js']

      await Commands.run.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'service.start',
        data: ['/absolute/path/service.js']
      })
    })

    it('should convert relative path to absolute', async () => {
      const args = ['relative/service.js']

      await Commands.run.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'service.start',
        data: [expect.stringContaining('relative/service.js')]
      })
    })
  })

  describe('mail commands', () => {
    describe('create', () => {
      it('should create mail account with provided credentials', async () => {
        const args = []
        mockCli.parseArg.mockImplementation((args, flags) => {
          if (flags.includes('--email')) return 'test@example.com'
          if (flags.includes('--password')) return 'password123'
          return null
        })

        await Commands.mail.sub.create.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'mail.create',
          data: ['test@example.com', 'password123', 'password123']
        })
      })

      it('should prompt for missing credentials', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockImplementation(prompt => {
          if (prompt.includes('e-mail')) return Promise.resolve('prompted@example.com')
          if (prompt.includes('password')) return Promise.resolve('prompted-pass')
          return Promise.resolve('')
        })

        await Commands.mail.sub.create.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the e-mail address: ')
        expect(mockCli.question).toHaveBeenCalledWith('Enter the password: ')
        expect(mockCli.question).toHaveBeenCalledWith('Re-enter the password: ')
      })

      it('should prompt for email when not provided via flag', async () => {
        const args = []
        mockCli.parseArg.mockImplementation((args, flags) => {
          if (flags.includes('--password')) return 'password123'
          return null
        })
        mockCli.question.mockResolvedValue('prompted@example.com')

        await Commands.mail.sub.create.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the e-mail address: ')
      })

      it('should prompt for password when not provided via flag', async () => {
        const args = []
        mockCli.parseArg.mockImplementation((args, flags) => {
          if (flags.includes('--email')) return 'test@example.com'
          return null
        })
        mockCli.question.mockImplementation(prompt => {
          if (prompt.includes('Enter the password:')) return Promise.resolve('prompted-pass')
          if (prompt.includes('Re-enter the password:')) return Promise.resolve('prompted-pass')
          return Promise.resolve('')
        })

        await Commands.mail.sub.create.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the password: ')
        expect(mockCli.question).toHaveBeenCalledWith('Re-enter the password: ')
      })
    })

    describe('delete', () => {
      it('should delete mail account', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('delete@example.com')

        await Commands.mail.sub.delete.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'mail.delete',
          data: ['delete@example.com']
        })
      })

      it('should prompt for email when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('prompted@example.com')

        await Commands.mail.sub.delete.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the e-mail address: ')
      })
    })

    describe('list', () => {
      it('should list mail accounts for domain', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('example.com')

        await Commands.mail.sub.list.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'mail.list',
          data: ['example.com']
        })
      })

      it('should prompt for domain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('example.com')

        await Commands.mail.sub.list.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the domain name: ')
      })
    })

    describe('password', () => {
      it('should change mail account password with provided credentials', async () => {
        const args = []
        mockCli.parseArg.mockImplementation((args, flags) => {
          if (flags.includes('--email')) return 'test@example.com'
          if (flags.includes('--password')) return 'newpass123'
          return null
        })

        await Commands.mail.sub.password.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'mail.password',
          data: ['test@example.com', 'newpass123', 'newpass123']
        })
      })

      it('should prompt for missing email and password', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockImplementation(prompt => {
          if (prompt.includes('e-mail')) return Promise.resolve('test@example.com')
          if (prompt.includes('new password:')) return Promise.resolve('newpass')
          if (prompt.includes('Re-enter')) return Promise.resolve('newpass')
          return Promise.resolve('')
        })

        await Commands.mail.sub.password.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the e-mail address: ')
        expect(mockCli.question).toHaveBeenCalledWith('Enter the new password: ')
        expect(mockCli.question).toHaveBeenCalledWith('Re-enter the new password: ')
      })
    })
  })

  describe('ssl commands', () => {
    describe('renew', () => {
      it('should renew SSL certificate', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('example.com')

        await Commands.ssl.sub.renew.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'ssl.renew',
          data: ['example.com']
        })
      })

      it('should prompt for domain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('example.com')

        await Commands.ssl.sub.renew.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the domain name: ')
      })
    })
  })

  describe('subdomain commands', () => {
    describe('create', () => {
      it('should create subdomain', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('sub.example.com')

        await Commands.subdomain.sub.create.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'subdomain.create',
          data: ['sub.example.com']
        })
      })

      it('should prompt for subdomain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('sub.example.com')

        await Commands.subdomain.sub.create.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the subdomain name (subdomain.example.com): ')
      })
    })

    describe('delete', () => {
      it('should delete subdomain', async () => {
        mockCli.question.mockResolvedValue('sub.example.com')

        await Commands.subdomain.sub.delete.action()

        expect(mockCli.question).toHaveBeenCalledWith('Enter the subdomain name (subdomain.example.com): ')
        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'subdomain.delete',
          data: ['sub.example.com']
        })
      })

      it('should prompt for subdomain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('sub.example.com')

        await Commands.subdomain.sub.delete.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the subdomain name (subdomain.example.com): ')
      })
    })

    describe('list', () => {
      it('should list subdomains for domain', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('example.com')

        await Commands.subdomain.sub.list.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'subdomain.list',
          data: ['example.com']
        })
      })

      it('should prompt for domain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('example.com')

        await Commands.subdomain.sub.list.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the domain name: ')
      })
    })
  })

  describe('web commands', () => {
    describe('create', () => {
      it('should create website', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('newsite.com')

        await Commands.web.sub.create.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'web.create',
          data: ['newsite.com']
        })
      })

      it('should prompt for domain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('newsite.com')

        await Commands.web.sub.create.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the domain name: ')
      })
    })

    describe('delete', () => {
      it('should delete website', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue('oldsite.com')

        await Commands.web.sub.delete.action(args)

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'web.delete',
          data: ['oldsite.com']
        })
      })

      it('should prompt for domain when not provided', async () => {
        const args = []
        mockCli.parseArg.mockReturnValue(null)
        mockCli.question.mockResolvedValue('oldsite.com')

        await Commands.web.sub.delete.action(args)

        expect(mockCli.question).toHaveBeenCalledWith('Enter the domain name: ')
      })
    })

    describe('list', () => {
      it('should list all websites', async () => {
        await Commands.web.sub.list.action()

        expect(mockConnector.call).toHaveBeenCalledWith({
          action: 'web.list'
        })
      })
    })
  })

  describe('command structure validation', () => {
    it('should have correct mail command structure', () => {
      expect(Commands.mail.title).toBe('MAIL')
      expect(Commands.mail.sub).toBeDefined()
      expect(Commands.mail.sub.create).toBeDefined()
      expect(Commands.mail.sub.delete).toBeDefined()
      expect(Commands.mail.sub.list).toBeDefined()
      expect(Commands.mail.sub.password).toBeDefined()
    })

    it('should have correct ssl command structure', () => {
      expect(Commands.ssl.title).toBe('SSL')
      expect(Commands.ssl.sub).toBeDefined()
      expect(Commands.ssl.sub.renew).toBeDefined()
    })

    it('should have correct subdomain command structure', () => {
      expect(Commands.subdomain.title).toBe('SUBDOMAIN')
      expect(Commands.subdomain.sub).toBeDefined()
      expect(Commands.subdomain.sub.create).toBeDefined()
      expect(Commands.subdomain.sub.delete).toBeDefined()
      expect(Commands.subdomain.sub.list).toBeDefined()
    })

    it('should have correct web command structure', () => {
      expect(Commands.web.title).toBe('WEBSITE')
      expect(Commands.web.sub).toBeDefined()
      expect(Commands.web.sub.create).toBeDefined()
      expect(Commands.web.sub.delete).toBeDefined()
      expect(Commands.web.sub.list).toBeDefined()
    })
  })

  describe('auth command edge cases', () => {
    it('should use key from args[0] when parseArg returns null', async () => {
      const args = ['direct-key']
      mockCli.parseArg.mockReturnValue(null)

      await Commands.auth.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'auth',
        data: ['direct-key']
      })
    })

    it('should handle Windows path format in run command', async () => {
      const args = ['C:\\Windows\\service.js']

      await Commands.run.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'service.start',
        data: ['C:\\Windows\\service.js']
      })
    })

    it('should handle UNC path format in run command', async () => {
      const args = ['\\\\server\\share\\service.js']

      await Commands.run.action(args)

      expect(mockConnector.call).toHaveBeenCalledWith({
        action: 'service.start',
        data: ['\\\\server\\share\\service.js']
      })
    })
  })
})
