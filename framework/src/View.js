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
      function: '${Candy.Var(await ',
      close: '}}',
      end: ').html().replace(/\\n/g, "<br>")}'
    },
    break: {
      function: 'break;',
      arguments: {}
    },
    component: {
      // TODO: Implement component
      //   <candy:component name="navbar" title="Dashboard"/>
    },
    continue: {
      function: 'continue;',
      arguments: {}
    },
    mysql: {
      // TODO: Implement mysql
    },
    else: {
      function: '} else {'
    },
    elseif: {
      function: '} else if($condition){',
      arguments: {
        condition: true
      }
    },
    fetch: {
      // TODO: Implement fetch
      //  <candy:fetch fetch="/get/products" as="data" method="GET" headers="{}" body="null" refresh="false">
    },
    for: {
      function: 'for(let $key in $constructor){ let $value = $constructor[$key];',
      arguments: {
        var: null,
        get: null,
        key: 'key',
        value: 'value'
      }
    },
    if: {
      function: 'if($condition){',
      arguments: {
        condition: true
      }
    },
    '<candy:js>': {
      end: ' html += `',
      function: '`; ',
      close: '</candy:js>'
    },
    lazy: {
      // TODO: Implement lazy
      //  <candy:lazy>
      //    <candy:component name="profile-card" data="user"/>
      //  </candy:lazy>
    },
    list: {
      arguments: {
        var: null,
        get: null,
        key: 'key',
        value: 'value'
      },
      end: '}',
      function: 'for(let $key in $constructor){ let $value = $constructor[$key];',
      replace: 'ul'
    },
    while: {
      function: 'while($condition){',
      arguments: {
        condition: true
      }
    }
  }
  #part = {}
  #candy = null

  constructor(candy) {
    this.#candy = candy

    if (!global.Candy?.View?.EarlyHints) {
      const config = candy.Config?.earlyHints
      this.#earlyHints = new EarlyHints(config)
      this.#earlyHints.init()

      if (!global.Candy) global.Candy = {}
      if (!global.Candy.View) global.Candy.View = {}
      global.Candy.View.EarlyHints = this.#earlyHints
    } else {
      this.#earlyHints = global.Candy.View.EarlyHints
    }
  }

  all(name) {
    this.#part.all = name
    return this
  }

  // - PRINT VIEW
  async print() {
    if (this.#candy.Request.res.finished) return

    const routePath = this.#candy.Request.req.url.split('?')[0]

    // Handle AJAX load requests
    if (this.#candy.Request.isAjaxLoad === true && this.#candy.Request.ajaxLoad && this.#candy.Request.ajaxLoad.length > 0) {
      let output = {}
      let variables = {}

      // Collect variables marked for AJAX
      for (let key in this.#candy.Request.variables) {
        if (this.#candy.Request.variables[key].ajax) {
          variables[key] = this.#candy.Request.variables[key].value
        }
      }

      // Render requested elements
      for (let element of this.#candy.Request.ajaxLoad) {
        if (this.#part[element]) {
          let viewPath = this.#part[element]
          if (viewPath.includes('.')) viewPath = viewPath.replace(/\./g, '/')
          if (fs.existsSync(`./view/${element}/${viewPath}.html`)) {
            output[element] = await this.#render(`./view/${element}/${viewPath}.html`)
          }
        }
      }

      this.#candy.Request.header('Content-Type', 'application/json')
      this.#candy.Request.header('X-Candy-Page', this.#candy.Request.page || '')
      this.#candy.Request.end({
        output: output,
        variables: variables
      })
      return
    }

    // Normal page rendering
    let result = ''
    if (this.#part.skeleton && fs.existsSync(`./skeleton/${this.#part.skeleton}.html`)) {
      result = fs.readFileSync(`./skeleton/${this.#part.skeleton}.html`, 'utf8')
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
      const hasEarlyHints = this.#candy.Request.hasEarlyHints()

      if (!hasEarlyHints) {
        const detectedResources = this.#earlyHints.extractFromHtml(result)

        if (detectedResources && detectedResources.length > 0) {
          this.#earlyHints.cacheHints(routePath, detectedResources)
        }
      }
    }

    this.#candy.Request.header('Content-Type', 'text/html')
    this.#candy.Request.end(result)
  }

  #parseCandyTag(content) {
    // Parse backend comments
    // Multi-line: <!--candy ... candy-->
    // Single-line: <!--candy ... -->
    content = content.replace(/<!--candy([\s\S]*?)(?:candy-->|-->)/g, () => {
      return ''
    })

    // Parse <script:candy> tags (IDE-friendly JavaScript with backend execution)
    content = content.replace(/<script:candy([^>]*)>([\s\S]*?)<\/script:candy>/g, (fullMatch, attributes, jsContent) => {
      return `<candy:js>${jsContent}</candy:js>`
    })

    content = content.replace(/<candy([^>]*?)\/>/g, (fullMatch, attributes) => {
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
    while (depth < maxDepth && content.includes('<candy')) {
      const before = content
      content = content.replace(/<candy([^>]*)>((?:(?!<candy)[\s\S])*?)<\/candy>/g, (fullMatch, attributes, innerContent) => {
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

        if (attrs.t || attrs.translate) {
          const placeholders = []
          let processedContent = innerContent
          let placeholderIndex = 1

          processedContent = processedContent.replace(/\{\{([^}]+)\}\}/g, (match, variable) => {
            variable = variable.trim()
            if (variable.startsWith("'") && variable.endsWith("'")) {
              placeholders.push(variable)
            } else {
              placeholders.push(`Candy.Var(await ${variable}).html().replace(/\\n/g, "<br>")`)
            }
            return `%s${placeholderIndex++}`
          })

          processedContent = processedContent.replace(/\{!!([^}]+)!!}/g, (match, variable) => {
            placeholders.push(`await ${variable.trim()}`)
            return `%s${placeholderIndex++}`
          })

          const translationCall =
            placeholders.length > 0 ? `__('${processedContent}', ${placeholders.join(', ')})` : `__('${processedContent}')`

          // Check if raw attribute is present
          if (attrs.raw) {
            return `{!! ${translationCall} !!}`
          } else {
            return `{{ ${translationCall} }}`
          }
        } else {
          // <candy> without attributes = string literal
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
      content = Form.parse(content, this.#candy)

      const jsBlocks = []
      content = content.replace(/<script:candy([^>]*)>([\s\S]*?)<\/script:candy>/g, (match, attrs, jsContent) => {
        const placeholder = `___CANDY_JS_BLOCK_${jsBlocks.length}___`
        jsBlocks.push(jsContent)
        return `<script:candy${attrs}>${placeholder}</script:candy>`
      })

      content = this.#parseCandyTag(content)
      content = content.replace(/`/g, '\\`').replace(/\$\{/g, '\\${')

      jsBlocks.forEach((jsContent, index) => {
        content = content.replace(`___CANDY_JS_BLOCK_${index}___`, jsContent)
      })

      let result = 'html += `\n' + content + '\n`'
      content = content.split('\n')
      for (let key in this.#functions) {
        let att = ''
        let func = this.#functions[key]
        let matches = func.close
          ? result.match(new RegExp(`${key}[\\s\\S]*?${func.close}`, 'g'))
          : result.match(new RegExp(`<candy:${key}.*?>`, 'g'))
        if (!matches) continue
        for (let match of matches) {
          const attrRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/g
          let attrMatch
          const args = []
          while ((attrMatch = attrRegex.exec(match))) {
            args.push(attrMatch[0])
          }
          if (!func.close) match = match.replace(/<candy:|>/g, '')
          let vars = {}
          if (func.arguments)
            for (let arg of args) {
              const argRegex = /(\w+)(?:=(["'])((?:(?!\2).)*)\2|=([^\s>]+))?/
              const argMatch = argRegex.exec(arg)
              if (!argMatch) continue
              const key = argMatch[1]
              const value = argMatch[3] !== undefined ? argMatch[3] : argMatch[4] !== undefined ? argMatch[4] : true
              if (func.arguments[key] === undefined) {
                att += `${key}="${value}"`
                continue
              }
              vars[key] = value
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
            fun = fun.replace('$constructor', constructor)
          }

          for (let key in func.arguments) {
            if (vars[key] === undefined) {
              if (func.arguments[key] === null) console.error(`"${key}" is required for "${match}"\n  in "${file}"`)
              vars[key] = func.arguments[key]
            }
            fun = fun.replace(new RegExp(`\\$${key}`, 'g'), vars[key])
          }
          if (func.close) {
            result = result.replace(match, fun + match.substring(key.length, match.length - func.close.length) + func.end)
          } else {
            result = result.replace(
              `<candy:${match}>`,
              (func.replace ? `<${[func.replace, att].join(' ')}>` : '') + '`; ' + fun + ' html += `'
            )
            result = result.replace(
              `</candy:${key}>`,
              '`; ' + (func.end ?? '}') + ' html += `' + (func.replace ? `</${func.replace}>` : '')
            )
          }
        }
      }
      let cache = `${nodeCrypto.createHash('md5').update(file).digest('hex')}`
      if (!fs.existsSync(CACHE_DIR)) fs.mkdirSync(CACHE_DIR, {recursive: true})
      fs.writeFileSync(
        `${CACHE_DIR}/${cache}`,
        `module.exports = async (Candy, get, __) => {\nlet html = '';\n${result}\nreturn html.trim()\n}`
      )
      delete require.cache[require.resolve(`${__dir}/${CACHE_DIR}/${cache}`)]
      if (!Candy.View) Candy.View = {}
      if (!Candy.View.cache) Candy.View.cache = {}
      Candy.View.cache[file] = {
        mtime: mtime,
        cache: cache
      }
    }
    try {
      return await require(`${__dir}/${CACHE_DIR}/${Candy.View.cache[file].cache}`)(
        this.#candy,
        key => this.#candy.Request.get(key),
        (...args) => this.#candy.Lang.get(...args)
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

    this.#sendEarlyHintsIfAvailable()
    return this
  }

  skeleton(name) {
    this.#part.skeleton = name
    this.#sendEarlyHintsIfAvailable()
    return this
  }

  #sendEarlyHintsIfAvailable() {
    if (this.#candy.Request.res.headersSent) return

    const routePath = this.#candy.Request.req.url.split('?')[0]
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
      this.#candy.Request.setEarlyHints(hints)
    }
  }
}

module.exports = View
