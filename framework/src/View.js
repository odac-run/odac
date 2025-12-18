const nodeCrypto = require('crypto')
const fs = require('fs')
const Form = require('./View/Form')
const EarlyHints = require('./View/EarlyHints')

const CACHE_DIR = './storage/.cache'

class View {
  #cache = {}
  #earlyHints = null
  #functions = {
    '{!!': {
      function: '${await ',
      close: '!!}',
      end: '}'
    },
    '{{--': {
      function: '`; /*',
      close: '--}}',
      end: '*/ html += `'
    },
    '{{': {
      function: '${Odac.Var(await ',
      close: '}}',
      end: ').html().replace(/\\n/g, "<br>")}'
    },
    break: {
      function: 'break;',
      arguments: {}
    },
    component: {
      // TODO: Implement component
      //   <odac:component name="navbar" title="Dashboard"/>
    },
    continue: {
      function: 'continue;',
      arguments: {}
    },
    mysql: {
      // TODO: Implement mysql
    },
    elseif: {
      function: '} else if(await ($condition)){',
      arguments: {
        condition: true
      }
    },
    else: {
      function: '} else {'
    },
    fetch: {
      // TODO: Implement fetch
      //  <odac:fetch fetch="/get/products" as="data" method="GET" headers="{}" body="null" refresh="false">
    },
    for: {
      function: '{ let _arr = $constructor; for(let $key in _arr){ let $value = _arr[$key];',
      end: '}}',
      arguments: {
        var: null,
        get: null,
        key: 'key',
        value: 'value'
      }
    },
    if: {
      function: 'if(await ($condition)){',
      arguments: {
        condition: true
      }
    },
    '<odac:js>': {
      end: ' html += `',
      function: '`; ',
      close: '</odac:js>'
    },
    lazy: {
      // TODO: Implement lazy
      //  <odac:lazy>
      //    <odac:component name="profile-card" data="user"/>
      //  </odac:lazy>
    },
    list: {
      arguments: {
        var: null,
        get: null,
        key: 'key',
        value: 'value'
      },
      end: '}}',
      function: '{ let _arr = $constructor; for(let $key in _arr){ let $value = _arr[$key];',
      replace: 'ul'
    },
    while: {
      function: 'while(await ($condition)){',
      arguments: {
        condition: true
      }
    }
  }
  #part = {}
  #odac = null

  constructor(odac) {
    this.#odac = odac

    if (!global.Odac?.View?.EarlyHints) {
      const config = odac.Config?.earlyHints
      this.#earlyHints = new EarlyHints(config)
      this.#earlyHints.init()

      if (!global.Odac) global.Odac = {}
      if (!global.Odac.View) global.Odac.View = {}
      global.Odac.View.EarlyHints = this.#earlyHints
    } else {
      this.#earlyHints = global.Odac.View.EarlyHints
    }
  }

  all(name) {
    this.#part.all = name
    return this
  }

  // - PRINT VIEW
  async print() {
    if (this.#odac.Request.res.finished) return

    const routePath = this.#odac.Request.req.url.split('?')[0]

    // Handle AJAX load requests
    if (this.#odac.Request.isAjaxLoad === true && this.#odac.Request.ajaxLoad && this.#odac.Request.ajaxLoad.length > 0) {
      let output = {}
      let variables = {}

      // Collect variables marked for AJAX
      for (let key in this.#odac.Request.variables) {
        if (this.#odac.Request.variables[key].ajax) {
          variables[key] = this.#odac.Request.variables[key].value
        }
      }

      // Render requested elements
      for (let element of this.#odac.Request.ajaxLoad) {
        if (this.#part[element]) {
          let viewPath = this.#part[element]
          if (viewPath.includes('.')) viewPath = viewPath.replace(/\./g, '/')
          if (fs.existsSync(`./view/${element}/${viewPath}.html`)) {
            output[element] = await this.#render(`./view/${element}/${viewPath}.html`)
          }
        }
      }

      const currentSkeleton = this.#part.skeleton || 'main'
      const clientSkeleton = this.#odac.Request.clientSkeleton
      const skeletonChanged = clientSkeleton && clientSkeleton !== currentSkeleton

      this.#odac.Request.header('Content-Type', 'application/json')
      this.#odac.Request.header('X-Odac-Page', this.#odac.Request.page || '')
      this.#odac.Request.header('Vary', 'X-Odac')

      this.#odac.Request.end({
        output: output,
        variables: variables,
        skeletonChanged: skeletonChanged
      })
      return
    }

    // Normal page rendering
    let result = ''
    if (this.#part.skeleton && fs.existsSync(`./skeleton/${this.#part.skeleton}.html`)) {
      result = fs.readFileSync(`./skeleton/${this.#part.skeleton}.html`, 'utf8')

      // Add data-odac-navigate to content wrapper for auto-navigation
      result = this.#addNavigateAttribute(result)

      for (let key in this.#part) {
        if (['all', 'skeleton'].includes(key)) continue
        if (!this.#part[key]) continue
        if (this.#part[key].includes('.')) this.#part[key] = this.#part[key].replace(/\./g, '/')
        if (fs.existsSync(`./view/${key}/${this.#part[key]}.html`)) {
          result = result.replace(`{{ ${key.toUpperCase()} }}`, await this.#render(`./view/${key}/${this.#part[key]}.html`))
        }
      }
      if (this.#part.all) {
        let parts = result.match(/{{.*?}}/g).map(part => part.replace(/{{|}}/g, '').trim())
        if (parts)
          for (let part of parts) {
            part = part.trim()
            let file = this.#part.all.split('.')
            file.splice(-1, 0, part.toLowerCase())
            file = file.join('/')
            if (fs.existsSync(`./view/${file}.html`)) {
              result = result.replace(`{{ ${part.toUpperCase()} }}`, await this.#render(`./view/${file}.html`))
            }
          }
      }
    }

    if (result) {
      const hasEarlyHints = this.#odac.Request.hasEarlyHints()

      if (!hasEarlyHints) {
        const detectedResources = this.#earlyHints.extractFromHtml(result)

        if (detectedResources && detectedResources.length > 0) {
          this.#earlyHints.cacheHints(routePath, detectedResources)
        }
      }
    }

    this.#odac.Request.header('Content-Type', 'text/html')
    this.#odac.Request.end(result)
  }

  #parseOdacTag(content) {
    // Parse backend comments
    // Multi-line: <!--odac ... odac-->
    // Single-line: <!--odac ... -->
    content = content.replace(/<!--odac([\s\S]*?)(?:odac-->|-->)/g, () => {
      return ''
    })

    // Parse <script:odac> tags (IDE-friendly JavaScript with backend execution)
    content = content.replace(/<script:odac([^>]*)>([\s\S]*?)<\/script:odac>/g, (fullMatch, attributes, jsContent) => {
      return `<odac:js>${jsContent}</odac:js>`
    })

    content = content.replace(/<odac:else\s*\/>/g, '<odac:else>')
    content = content.replace(/<odac:elseif\s+([^>]*?)\/>/g, '<odac:elseif $1>')

    content = content.replace(/<odac([^>]*?)\/>/g, (fullMatch, attributes) => {
      attributes = attributes.trim()

      const attrs = {}
      const attrRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/g
      let match
      while ((match = attrRegex.exec(attributes))) {
        const key = match[1]
        const value = match[3] !== undefined ? match[3] : match[4] !== undefined ? match[4] : true
        attrs[key] = value
      }

      if (attrs.get) {
        return `{{ get('${attrs.get}') || '' }}`
      } else if (attrs.var) {
        if (attrs.raw) {
          return `{!! ${attrs.var} !!}`
        } else {
          return `{{ ${attrs.var} }}`
        }
      }
      return fullMatch
    })

    let depth = 0
    let maxDepth = 10
    while (depth < maxDepth && content.includes('<odac')) {
      const before = content
      content = content.replace(/<odac([^>]*)>((?:(?!<odac)[\s\S])*?)<\/odac>/g, (fullMatch, attributes, innerContent) => {
        attributes = attributes.trim()
        innerContent = innerContent.trim()

        const attrs = {}
        const attrRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/g
        let match
        while ((match = attrRegex.exec(attributes))) {
          const key = match[1]
          const value = match[3] !== undefined ? match[3] : match[4] !== undefined ? match[4] : true
          attrs[key] = value
        }

        if (attrs.get) {
          return `{{ get('${attrs.get}') || '' }}`
        } else if (attrs.var) {
          if (attrs.raw) {
            return `{!! ${attrs.var} !!}`
          } else {
            return `{{ ${attrs.var} }}`
          }
        } else if (attrs.t || attrs.translate) {
          const placeholders = []
          let processedContent = innerContent
          let placeholderIndex = 1

          processedContent = processedContent.replace(/\{\{([^}]+)\}\}/g, (match, variable) => {
            variable = variable.trim()
            if (variable.startsWith("'") && variable.endsWith("'")) {
              placeholders.push(variable)
            } else {
              placeholders.push(`Odac.Var(await ${variable}).html().replace(/\\n/g, "<br>")`)
            }
            return `%s${placeholderIndex++}`
          })

          processedContent = processedContent.replace(/\{!!([^}]+)!!}/g, (match, variable) => {
            placeholders.push(`await ${variable.trim()}`)
            return `%s${placeholderIndex++}`
          })

          const translationCall =
            placeholders.length > 0 ? `__('${processedContent}', ${placeholders.join(', ')})` : `__('${processedContent}')`

          if (attrs.raw) {
            return `{!! ${translationCall} !!}`
          } else {
            return `{{ ${translationCall} }}`
          }
        } else {
          return `{{ '${innerContent}' }}`
        }
      })
      if (before === content) break
      depth++
    }

    return content
  }

  async #render(file) {
    let mtime = fs.statSync(file).mtimeMs
    let content = fs.readFileSync(file, 'utf8')

    if (this.#cache[file]?.mtime !== mtime) {
      content = Form.parse(content, this.#odac)

      const jsBlocks = []
      content = content.replace(/<script:odac([^>]*)>([\s\S]*?)<\/script:odac>/g, (match, attrs, jsContent) => {
        const placeholder = `___ODAC_JS_BLOCK_${jsBlocks.length}___`
        jsBlocks.push(jsContent)
        return `<script:odac${attrs}>${placeholder}</script:odac>`
      })

      content = this.#parseOdacTag(content)
      content = content.replace(/`/g, '\\`').replace(/\$\{/g, '\\${')

      jsBlocks.forEach((jsContent, index) => {
        content = content.replace(`___ODAC_JS_BLOCK_${index}___`, jsContent)
      })

      let result = 'html += `\n' + content + '\n`'
      content = content.split('\n')
      for (let key in this.#functions) {
        let att = ''
        let func = this.#functions[key]
        let matches = func.close
          ? result.match(new RegExp(`${key}[\\s\\S]*?${func.close}`, 'g'))
          : result.match(new RegExp(`<odac:${key}(?:\\s+[^>]*?(?:"[^"]*"|'[^']*'|[^"'>])*)?>`, 'g'))
        if (!matches) continue
        for (let match of matches) {
          let matchForParsing = match
          if (!func.close) matchForParsing = matchForParsing.replace(/^<odac:/, '').replace(/>$/, '')
          const attrRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/g
          let attrMatch
          const args = []
          while ((attrMatch = attrRegex.exec(matchForParsing))) {
            args.push(attrMatch[0])
          }
          let vars = {}
          if (func.arguments)
            for (let arg of args) {
              const argRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/
              const argMatch = argRegex.exec(arg)
              if (!argMatch) continue
              const argKey = argMatch[1]
              const value = argMatch[3] !== undefined ? argMatch[3] : argMatch[4] !== undefined ? argMatch[4] : true
              if (func.arguments[argKey] === undefined) {
                att += `${argKey}="${value}"`
                continue
              }
              vars[argKey] = value
            }
          if (!func.function) continue
          let fun = func.function

          if (key === 'for' || key === 'list') {
            if (!vars.var && !vars.get) {
              console.error(`"var" or "get" is required for "${match}"\n  in "${file}"`)
              continue
            }
            let constructor
            if (vars.var) {
              constructor = `await ${vars.var}`
              delete vars.var
            } else if (vars.get) {
              constructor = `get('${vars.get}')`
              delete vars.get
            }
            fun = fun.replace(/\$constructor/g, constructor)
          }

          for (let argKey in func.arguments) {
            if (argKey === 'var' || argKey === 'get') continue
            if (vars[argKey] === undefined) {
              if (func.arguments[argKey] === null) console.error(`"${argKey}" is required for "${match}"\n  in "${file}"`)
              vars[argKey] = func.arguments[argKey]
            }
            fun = fun.replace(new RegExp(`\\$${argKey}`, 'g'), vars[argKey])
          }
          if (func.close) {
            result = result.replace(match, fun + match.substring(key.length, match.length - func.close.length) + func.end)
          } else {
            result = result.replace(match, (func.replace ? `<${[func.replace, att].join(' ')}>` : '') + '`; ' + fun + ' html += `')
            result = result.replace(`</odac:${key}>`, '`; ' + (func.end ?? '}') + ' html += `' + (func.replace ? `</${func.replace}>` : ''))
          }
        }
      }
      let cache = `${nodeCrypto.createHash('md5').update(file).digest('hex')}`
      if (!fs.existsSync(CACHE_DIR)) fs.mkdirSync(CACHE_DIR, {recursive: true})
      fs.writeFileSync(
        `${CACHE_DIR}/${cache}`,
        `module.exports = async (Odac, get, __) => {\nlet html = '';\n${result}\nreturn html.trim()\n}`
      )
      delete require.cache[require.resolve(`${__dir}/${CACHE_DIR}/${cache}`)]
      if (!Odac.View) Odac.View = {}
      if (!Odac.View.cache) Odac.View.cache = {}
      Odac.View.cache[file] = {
        mtime: mtime,
        cache: cache
      }
    }
    try {
      return await require(`${__dir}/${CACHE_DIR}/${Odac.View.cache[file].cache}`)(
        this.#odac,
        key => this.#odac.Request.get(key),
        (...args) => this.#odac.Lang.get(...args)
      )
    } catch (e) {
      let stackLine = e.stack.split('\n')[1].match(/:(\d+):\d+/)
      let line = stackLine ? parseInt(stackLine[1]) - 3 : e.lineNumber ? e.lineNumber - 3 : 'unknown'
      console.error(e.toString().split('\n')[0] + `\n  in line ${line}\n  of "${file}"`)
    }
    return ''
  }

  // - SET PARTS
  set(...args) {
    if (args.length === 1 && typeof args[0] === 'object') for (let key in args[0]) this.#part[key] = args[0][key]
    else if (args.length === 2) this.#part[args[0]] = args[1]

    if (!this.#odac.Request.page) {
      this.#odac.Request.page = this.#part.content || this.#part.all || ''
    }

    this.#sendEarlyHintsIfAvailable()
    return this
  }

  skeleton(name) {
    this.#part.skeleton = name
    this.#sendEarlyHintsIfAvailable()
    return this
  }

  #addNavigateAttribute(skeleton) {
    skeleton = skeleton.replace(/(<[^>]+>)(\s*\{\{\s*CONTENT\s*\}\})/, (match, openTag, content) => {
      if (openTag.includes('data-odac-navigate')) return match
      const tagWithAttr = openTag.slice(0, -1) + ' data-odac-navigate="content">'
      return tagWithAttr + content
    })

    const skeletonName = this.#part.skeleton || 'main'
    const pageName = this.#odac.Request.page || ''

    skeleton = skeleton.replace(/<html([^>]*)>/, (match, attrs) => {
      const updates = []
      if (!attrs.includes('data-odac-skeleton')) {
        updates.push(`data-odac-skeleton="${skeletonName}"`)
      }
      if (!attrs.includes('data-odac-page')) {
        updates.push(`data-odac-page="${pageName}"`)
      }
      if (updates.length === 0) return match
      return `<html${attrs} ${updates.join(' ')}>`
    })

    return skeleton
  }

  #sendEarlyHintsIfAvailable() {
    if (this.#odac.Request.res.headersSent) return

    const routePath = this.#odac.Request.req.url.split('?')[0]
    const viewPaths = []

    if (this.#part.skeleton) {
      viewPaths.push(`skeleton/${this.#part.skeleton}`)
    }

    for (let key in this.#part) {
      if (['skeleton'].includes(key)) continue
      if (this.#part[key]) {
        const viewPath = this.#part[key].replace(/\./g, '/')
        viewPaths.push(`view/${key}/${viewPath}`)
      }
    }

    let hints = this.#earlyHints.getHints(null, routePath)

    if (!hints && viewPaths.length > 0) {
      hints = this.#earlyHints.getHintsForViewFiles(viewPaths)
    }

    if (hints && hints.length > 0) {
      this.#odac.Request.setEarlyHints(hints)
    }
  }
}

module.exports = View
