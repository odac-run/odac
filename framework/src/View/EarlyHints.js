const fs = require('fs')
const path = require('path')

class EarlyHints {
  #manifest = {}
  #cache = new Map()
  #config = null
  #initialized = false

  constructor(config) {
    this.#config = config || {
      enabled: true,
      auto: true,
      maxResources: 5
    }
  }

  init() {
    if (this.#initialized) return
    this.#initialized = true

    if (!this.#config.enabled) return

    this.#buildManifest()
  }

  #buildManifest() {
    const viewDir = path.join(process.cwd(), 'view')
    const skeletonDir = path.join(process.cwd(), 'skeleton')

    try {
      if (fs.existsSync(viewDir)) {
        const files = this.#getAllViewFiles(viewDir)
        for (const file of files) {
          const html = fs.readFileSync(file, 'utf8')
          const resources = this.#extractResources(html)

          const relativePath = path.relative(viewDir, file)
          const viewName = 'view/' + relativePath.replace(/\.html$/, '').replace(/\\/g, '/')

          if (resources.length > 0) {
            this.#manifest[viewName] = resources
          }
        }
      }

      if (fs.existsSync(skeletonDir)) {
        const files = this.#getAllViewFiles(skeletonDir)
        for (const file of files) {
          const html = fs.readFileSync(file, 'utf8')
          const resources = this.#extractResources(html)

          const relativePath = path.relative(skeletonDir, file)
          const viewName = 'skeleton/' + relativePath.replace(/\.html$/, '').replace(/\\/g, '/')

          if (resources.length > 0) {
            this.#manifest[viewName] = resources
          }
        }
      }
    } catch {
      // Silently fail, manifest building is optional
    }
  }

  #getAllViewFiles(dir, files = []) {
    const entries = fs.readdirSync(dir, {withFileTypes: true})

    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name)
      if (entry.isDirectory()) {
        this.#getAllViewFiles(fullPath, files)
      } else if (entry.isFile() && entry.name.endsWith('.html')) {
        files.push(fullPath)
      }
    }

    return files
  }

  #extractResources(html) {
    const resources = []

    const headMatch = html.match(/<head[^>]*>([\s\S]*?)<\/head>/i)
    if (!headMatch) return resources

    const head = headMatch[1]

    const cssMatches = head.matchAll(/<link[^>]+href=["']([^"']+\.css)["'][^>]*>/gi)
    for (const match of cssMatches) {
      const fullTag = match[0].toLowerCase()
      if (fullTag.includes('defer')) continue
      if (fullTag.includes('rel="stylesheet"') || fullTag.includes("rel='stylesheet'")) {
        resources.push({href: match[1], as: 'style'})
      }
    }

    const jsMatches = head.matchAll(/<script[^>]+src=["']([^"']+\.js)["'][^>]*>/gi)
    for (const match of jsMatches) {
      const fullTag = match[0]
      if (!fullTag.includes('defer') && !fullTag.includes('async')) {
        resources.push({href: match[1], as: 'script'})
      }
    }

    const fontMatches = head.matchAll(/<link[^>]+href=["']([^"']+\.(woff2?|ttf|otf|eot))["'][^>]*>/gi)
    for (const match of fontMatches) {
      const fullTag = match[0]
      if (fullTag.includes('defer')) continue
      resources.push({
        href: match[1],
        as: 'font',
        crossorigin: 'anonymous'
      })
    }

    return resources.slice(0, this.#config.maxResources)
  }

  getHints(viewPath, routePath) {
    if (!this.#config.enabled || !this.#config.auto) return null

    let hints = this.#manifest[viewPath]

    if (!hints && routePath) {
      hints = this.#cache.get(routePath)
    }

    return hints || null
  }

  getHintsForViewFiles(viewPaths) {
    if (!this.#config.enabled || !this.#config.auto) return null

    for (const viewPath of viewPaths) {
      const hints = this.#manifest[viewPath]
      if (hints && hints.length > 0) {
        return hints
      }
    }

    return null
  }

  cacheHints(routePath, resources) {
    if (!this.#config.enabled || !this.#config.auto) return

    if (resources && resources.length > 0) {
      this.#cache.set(routePath, resources)
    }
  }

  extractFromHtml(html) {
    if (!this.#config.enabled || !this.#config.auto) return []

    return this.#extractResources(html)
  }

  formatLinkHeader(resource) {
    let header = `<${resource.href}>; rel=preload; as=${resource.as}`
    if (resource.crossorigin) {
      header += `; crossorigin`
    }
    if (resource.type) {
      header += `; type=${resource.type}`
    }
    return header
  }

  send(res, resources) {
    if (!this.#config.enabled || !resources || resources.length === 0) return false
    if (res.headersSent || res.writableEnded) return false

    try {
      const links = resources.map(r => this.formatLinkHeader(r))

      if (typeof res.writeEarlyHints === 'function') {
        res.writeEarlyHints({link: links})
      }

      res.setHeader('X-Odac-Early-Hints', JSON.stringify(links))

      return true
    } catch {
      return false
    }
  }
}

module.exports = EarlyHints
