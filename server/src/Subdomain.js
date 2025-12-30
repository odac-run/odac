class Subdomain {
  async create(subdomain) {
    let domain = subdomain.split('.')
    subdomain = subdomain.trim().split('.')
    if (subdomain.length < 3) return Odac.server('Api').result(false, await __('Invalid subdomain name.'))
    if (Odac.core('Config').config.websites[domain.join('.')])
      return Odac.server('Api').result(false, await __('Domain %s already exists.', domain.join('.')))
    while (domain.length > 2) {
      domain.shift()
      if (Odac.core('Config').config.websites[domain.join('.')]) {
        domain = domain.join('.')
        break
      }
    }
    if (typeof domain == 'object') return Odac.server('Api').result(false, await __('Domain %s not found.', domain.join('.')))
    subdomain = subdomain.join('.').substr(0, subdomain.join('.').length - domain.length - 1)
    let fulldomain = [subdomain, domain].join('.')
    if (Odac.core('Config').config.websites[domain].subdomain.includes(subdomain))
      return Odac.server('Api').result(false, await __('Subdomain %s already exists.', fulldomain))
    Odac.server('DNS').record({name: fulldomain, type: 'A'}, {name: fulldomain, type: 'MX'})
    let websites = Odac.core('Config').config.websites
    websites[domain].subdomain.push(subdomain)
    websites[domain].subdomain.sort()
    Odac.core('Config').config.websites = websites
    Odac.server('SSL').renew(domain)
    return Odac.server('Api').result(true, await __('Subdomain %s1 created successfully for domain %s2.', fulldomain, domain))
  }

  async delete(subdomain) {
    let domain = subdomain.split('.')
    subdomain = subdomain.trim().split('.')
    if (subdomain.length < 3) return Odac.server('Api').result(false, await __('Invalid subdomain name.'))
    if (Odac.core('Config').config.websites[domain.join('.')])
      return Odac.server('Api').result(false, await __('%s is a domain.', domain.join('.')))
    while (domain.length > 2) {
      domain.shift()
      if (Odac.core('Config').config.websites[domain.join('.')]) {
        domain = domain.join('.')
        break
      }
    }
    if (typeof domain == 'object') return Odac.server('Api').result(false, await __('Domain %s not found.', domain.join('.')))
    subdomain = subdomain.join('.').substr(0, subdomain.join('.').length - domain.length - 1)
    let fulldomain = [subdomain, domain].join('.')
    if (!Odac.core('Config').config.websites[domain].subdomain.includes(subdomain))
      return Odac.server('Api').result(false, await __('Subdomain %s not found.', fulldomain))
    Odac.server('DNS').delete({name: fulldomain, type: 'A'}, {name: fulldomain, type: 'MX'})
    let websites = Odac.core('Config').config.websites
    websites[domain].subdomain = websites[domain].subdomain.filter(s => s != subdomain)
    Odac.core('Config').config.websites = websites
    return Odac.server('Api').result(true, await __('Subdomain %s1 deleted successfully from domain %s2.', fulldomain, domain))
  }

  async list(domain) {
    if (!Odac.core('Config').config.websites[domain]) return Odac.server('Api').result(false, await __('Domain %s not found.', domain))
    let subdomains = Odac.core('Config').config.websites[domain].subdomain.map(subdomain => {
      return subdomain + '.' + domain
    })
    return Odac.server('Api').result(true, (await __('Subdomains of %s:', domain)) + '\n  ' + subdomains.join('\n  '))
  }
}

module.exports = new Subdomain()
