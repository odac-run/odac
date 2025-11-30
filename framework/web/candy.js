class candy {
  actions = {}
  #data = null
  fn = {}
  #page = null
  #token = {hash: [], data: false}
  #formSubmitHandlers = new Map()
  #loader = {elements: {}, callback: null}
  #isNavigating = false

  constructor() {
    this.#data = this.data()
  }

  #ajax(options) {
    const {
      url,
      type = 'GET',
      headers = {},
      data = null,
      dataType = 'text',
      success = () => {},
      error = () => {},
      complete = () => {},
      contentType = 'application/x-www-form-urlencoded; charset=UTF-8',
      xhr: xhrFactory
    } = options

    const xhr = xhrFactory ? xhrFactory() : new XMLHttpRequest()

    xhr.open(type, url, true)

    Object.keys(headers).forEach(key => {
      xhr.setRequestHeader(key, headers[key])
    })

    if (contentType && !(data instanceof FormData)) {
      xhr.setRequestHeader('Content-Type', contentType)
    }

    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        let responseData = xhr.responseText
        if (dataType === 'json') {
          try {
            responseData = JSON.parse(responseData)
          } catch (e) {
            console.error('JSON parse error:', e)
            error(xhr, 'parseerror', e)
            return
          }
        }

        document.dispatchEvent(
          new CustomEvent('candy:ajaxSuccess', {
            detail: {response: responseData, status: xhr.statusText, xhr, requestUrl: url}
          })
        )

        success(responseData, xhr.statusText, xhr)
      } else {
        error(xhr, xhr.statusText)
      }
    }

    xhr.onerror = () => error(xhr, 'error')
    xhr.onloadend = () => complete()
    xhr.send(data)
  }

  #fade(element, type, duration = 400, callback) {
    const isIn = type === 'in'
    const startOpacity = isIn ? 0 : 1
    const endOpacity = isIn ? 1 : 0

    element.style.opacity = startOpacity
    if (isIn) {
      element.style.display = 'block'
    }

    let startTime = null

    const animate = currentTime => {
      if (!startTime) startTime = currentTime
      const progress = currentTime - startTime
      const opacity = startOpacity + (endOpacity - startOpacity) * Math.min(progress / duration, 1)
      element.style.opacity = opacity

      if (progress < duration) {
        requestAnimationFrame(animate)
      } else {
        if (!isIn) {
          element.style.display = 'none'
        }
        if (callback) callback()
      }
    }
    requestAnimationFrame(animate)
  }

  #fadeIn(element, duration, callback) {
    this.#fade(element, 'in', duration, callback)
  }

  #fadeOut(element, duration, callback) {
    this.#fade(element, 'out', duration, callback)
  }

  #on(element, event, selector, handler) {
    element.addEventListener(event, e => {
      let target = e.target.closest(selector)
      if (target) {
        handler.call(target, e)
      }
    })
  }

  #serialize(form) {
    const params = []
    form.querySelectorAll('input, select, textarea').forEach(el => {
      if (el.name && !el.disabled) {
        if (el.type === 'checkbox' || el.type === 'radio') {
          if (el.checked) {
            params.push(`${encodeURIComponent(el.name)}=${encodeURIComponent(el.value)}`)
          }
        } else if (el.tagName.toLowerCase() === 'select' && el.multiple) {
          Array.from(el.options).forEach(option => {
            if (option.selected) {
              params.push(`${encodeURIComponent(el.name)}=${encodeURIComponent(option.value)}`)
            }
          })
        } else {
          params.push(`${encodeURIComponent(el.name)}=${encodeURIComponent(el.value)}`)
        }
      }
    })
    return params.join('&')
  }

  action(obj) {
    if (obj.function) for (let func in obj.function) this.fn[func] = obj.function[func]

    // Handle navigate configuration
    if (obj.navigate !== undefined && obj.navigate !== false) {
      let selector, elements, callback

      // Minimal: navigate: 'main'
      if (typeof obj.navigate === 'string') {
        selector = 'a[href^="/"]:not([data-navigate="false"]):not(.no-navigate)'
        elements = {content: obj.navigate}
        callback = null
      }
      // Medium/Advanced: navigate: {...}
      else if (typeof obj.navigate === 'object') {
        // Determine base selector
        let baseSelector
        if (obj.navigate.links) {
          baseSelector = obj.navigate.links
        } else if (obj.navigate.selector) {
          baseSelector = obj.navigate.selector
        } else {
          baseSelector = 'a[href^="/"]' // Default: all internal links
        }

        // Add exclusions to selector
        selector = `${baseSelector}:not([data-navigate="false"]):not(.no-navigate)`

        // Determine elements to update
        if (obj.navigate.update) {
          if (typeof obj.navigate.update === 'string') {
            elements = {content: obj.navigate.update}
          } else {
            elements = obj.navigate.update
          }
        } else if (obj.navigate.elements) {
          elements = obj.navigate.elements
        } else {
          elements = {content: 'main'} // Default
        }

        // Determine callback
        callback = obj.navigate.on || obj.navigate.callback || null
      }
      // Boolean: navigate: true
      else if (obj.navigate === true) {
        selector = 'a[href^="/"]:not([data-navigate="false"]):not(.no-navigate)'
        elements = {content: 'main'}
        callback = null
      }

      // Initialize loader after DOM is ready
      if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', () => {
          this.loader(selector, elements, callback)
        })
      } else {
        this.loader(selector, elements, callback)
      }
    }

    if (obj.start) document.addEventListener('DOMContentLoaded', () => obj.start())
    if (obj.load) {
      if (!this.actions.load) this.actions.load = []
      this.actions.load.push(obj.load)
      document.addEventListener('DOMContentLoaded', () => obj.load())
    }
    if (obj.page) {
      if (!this.actions.page) this.actions.page = {}
      for (let page in obj.page) {
        if (!this.actions.page[page]) this.actions.page[page] = []
        this.actions.page[page].push(obj.page[page])
        if (this.page() == page) document.addEventListener('DOMContentLoaded', () => obj.page[page]())
      }
    }
    if (obj.interval) {
      if (!this.actions.interval) this.actions.interval = {}
      for (let interval in obj.interval) {
        this.actions.interval[interval] = obj.interval[interval]
        if (obj.interval[interval].page && obj.interval[interval].page != this.page()) continue
        this.actions.interval[interval]._ = setInterval(obj.interval[interval].function, obj.interval[interval].interval ?? 1000)
      }
    }
    for (let key in obj) {
      if (['function', 'start', 'load', 'page', 'interval', 'navigate'].includes(key)) continue
      for (let key2 in obj[key]) {
        if (typeof obj[key][key2] == 'function') {
          this.#on(document, key, key2, obj[key][key2])
        } else {
          let func = ''
          let split = ''
          if (obj[key][key2].includes('.')) split = '.'
          else if (obj[key][key2].includes('#')) split = '#'
          else if (obj[key][key2].includes(' ')) split = ' '
          func = split != '' ? obj[key][key2].split(split) : [obj[key][key2]]
          if (func != '') {
            let getfunc = obj
            func.forEach(function (item) {
              getfunc = getfunc[item] !== undefined ? getfunc[item] : getfunc[split + item]
            })
            this.#on(document, key, key2, getfunc)
          }
        }
      }
    }
  }

  client() {
    if (!document.cookie.includes('candy_client=')) return null
    return document.cookie.split('candy_client=')[1].split(';')[0]
  }

  data() {
    if (this.#data) return this.#data
    if (!document.cookie.includes('candy_data=')) return null
    return JSON.parse(unescape(document.cookie.split('candy_data=')[1].split(';')[0]))
  }

  form(obj, callback) {
    if (typeof obj != 'object') obj = {form: obj}
    const formSelector = obj.form

    if (this.#formSubmitHandlers.has(formSelector)) {
      const oldHandler = this.#formSubmitHandlers.get(formSelector)
      document.removeEventListener('submit', oldHandler)
    }

    const handler = e => {
      const formElement = e.target.closest(formSelector)
      if (!formElement) return

      e.preventDefault()

      const inputs = formElement.querySelectorAll('input:not([type="hidden"]), textarea, select')
      let isValid = true
      let firstInvalidInput = null

      const showError = (input, errorType) => {
        isValid = false
        firstInvalidInput = input

        if (input.type !== 'checkbox' && input.type !== 'radio') {
          input.style.borderColor = '#dc3545'
        }

        const customMessage = input.getAttribute(`data-error-${errorType}`)
        if (customMessage) {
          let errorSpan = formElement.querySelector(`[candy-form-error="${input.name}"]`)

          if (!errorSpan) {
            errorSpan = document.createElement('span')
            errorSpan.setAttribute('candy-form-error', input.name)

            if ((input.type === 'checkbox' || input.type === 'radio') && input.id) {
              const label = formElement.querySelector(`label[for="${input.id}"]`)
              if (label) {
                label.parentNode.insertBefore(errorSpan, label.nextSibling)
              } else {
                input.parentNode.insertBefore(errorSpan, input.nextSibling)
              }
            } else {
              input.parentNode.insertBefore(errorSpan, input.nextSibling)
            }
          }

          errorSpan.textContent = customMessage
          errorSpan.style.cssText = 'display:block;color:#dc3545;font-size:0.875rem;margin-top:0.25rem'
        }
      }

      for (const input of inputs) {
        input.style.borderColor = ''
        const errorSpan = formElement.querySelector(`[candy-form-error="${input.name}"]`)
        if (errorSpan) {
          errorSpan.style.display = 'none'
          errorSpan.textContent = ''
        }

        if (input.hasAttribute('required')) {
          const isEmpty = input.type === 'checkbox' || input.type === 'radio' ? !input.checked : !input.value.trim()
          if (isEmpty) {
            showError(input, 'required')
            break
          }
        }

        if (input.hasAttribute('minlength') && input.value && input.value.trim().length < parseInt(input.getAttribute('minlength'))) {
          showError(input, 'minlength')
          break
        }

        if (input.hasAttribute('maxlength') && input.value && input.value.trim().length > parseInt(input.getAttribute('maxlength'))) {
          showError(input, 'maxlength')
          break
        }

        if (input.hasAttribute('pattern') && input.value) {
          const trimmedValue = input.value.trim()
          const pattern = input.getAttribute('pattern')
          if (!new RegExp(pattern).test(trimmedValue)) {
            showError(input, 'pattern')
            break
          }
        }

        if (input.type === 'email' && input.value && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(input.value.trim())) {
          showError(input, 'email')
          break
        }
      }

      if (!isValid) {
        if (firstInvalidInput) firstInvalidInput.focus()
        return
      }

      let actions = this.actions
      if (
        actions.candy &&
        actions.candy.form &&
        actions.candy.form.input &&
        actions.candy.form.input.class &&
        actions.candy.form.input.class.invalid
      ) {
        const invalidClass = actions.candy.form.input.class.invalid
        formElement
          .querySelectorAll(`select.${invalidClass},input.${invalidClass},textarea.${invalidClass}`)
          .forEach(el => el.classList.remove(invalidClass))
      }

      if (obj.messages !== false) {
        if (obj.messages == undefined || obj.messages == true || obj.messages.includes('error')) {
          formElement.querySelectorAll('*[candy-form-error]').forEach(el => (el.style.display = 'none'))
        }
        if (obj.messages == undefined || obj.messages == true || obj.messages.includes('success')) {
          formElement.querySelectorAll('*[candy-form-success]').forEach(el => (el.style.display = 'none'))
        }
      }

      let datastring, cache, contentType, processData
      if (formElement.querySelector('input[type=file]')) {
        datastring = new FormData(formElement)
        datastring.append('token', this.token())
        cache = false
        contentType = false
        processData = false
      } else {
        datastring = this.#serialize(formElement) + '&_token=' + this.token()
        cache = true
        contentType = 'application/x-www-form-urlencoded; charset=UTF-8'
        processData = true
      }

      const submitButtons = formElement.querySelectorAll('button[type="submit"], input[type="submit"]')
      submitButtons.forEach(btn => {
        btn.disabled = true
        const loadingText = btn.getAttribute('data-loading-text')
        if (loadingText) {
          btn.setAttribute('data-original-text', btn.textContent)
          btn.textContent = loadingText
        }
      })

      formElement.querySelectorAll('input:not([type="hidden"]), textarea, select').forEach(el => (el.disabled = true))

      this.#ajax({
        type: formElement.getAttribute('method'),
        url: formElement.getAttribute('action'),
        data: datastring,
        dataType: 'json',
        contentType: contentType,
        processData: processData,
        cache: cache,
        success: data => {
          if (!data.result) return false
          if (obj.messages == undefined || obj.messages) {
            if (data.result.success && (obj.messages == undefined || obj.messages.includes('success') || obj.messages == true)) {
              const successEl = formElement.querySelector('*[candy-form-success]')
              if (successEl) {
                successEl.innerHTML = data.result.message
                this.#fadeIn(successEl)
              } else {
                formElement.insertAdjacentHTML('beforeend', `<span candy-form-success="${obj.form}">${data.result.message}</span>`)
              }
            } else if (!data.result.success && data.errors) {
              Object.entries(data.errors).forEach(([name, message]) => {
                if (message) {
                  let errorEl = formElement.querySelector(`[candy-form-error="${name}"]`)
                  if (errorEl) {
                    errorEl.textContent = message
                    errorEl.style.cssText = 'display:block;color:#dc3545;font-size:0.875rem;margin-top:0.25rem'
                  } else {
                    const inputEl = formElement.querySelector(`*[name="${name}"]`)
                    if (inputEl) {
                      errorEl = document.createElement('span')
                      errorEl.setAttribute('candy-form-error', name)
                      errorEl.textContent = message
                      errorEl.style.cssText = 'display:block;color:#dc3545;font-size:0.875rem;margin-top:0.25rem'

                      if ((inputEl.type === 'checkbox' || inputEl.type === 'radio') && inputEl.id) {
                        const label = formElement.querySelector(`label[for="${inputEl.id}"]`)
                        if (label) {
                          label.parentNode.insertBefore(errorEl, label.nextSibling)
                        } else {
                          inputEl.parentNode.insertBefore(errorEl, inputEl.nextSibling)
                        }
                      } else {
                        inputEl.parentNode.insertBefore(errorEl, inputEl.nextSibling)
                      }
                    } else if (name === '_candy_form') {
                      errorEl = document.createElement('div')
                      errorEl.setAttribute('candy-form-error', name)
                      errorEl.textContent = message
                      errorEl.style.cssText =
                        'display:block;color:#dc3545;background-color:#f8d7da;border:1px solid #f5c2c7;border-radius:0.375rem;padding:0.75rem 1rem;margin-bottom:1rem;font-size:0.875rem'
                      formElement.insertBefore(errorEl, formElement.firstChild)
                    }
                  }
                }
                const inputEl = formElement.querySelector(`*[name="${name}"]`)
                if (inputEl) {
                  if (inputEl.type !== 'checkbox' && inputEl.type !== 'radio') {
                    inputEl.style.borderColor = '#dc3545'
                  }
                  inputEl.addEventListener(
                    'focus',
                    function handler() {
                      inputEl.style.borderColor = ''
                      const errorEl = formElement.querySelector(`[candy-form-error="${name}"]`)
                      if (errorEl) {
                        errorEl.style.display = 'none'
                        errorEl.textContent = ''
                      }
                      inputEl.removeEventListener('focus', handler)
                    }.bind(this),
                    {once: true}
                  )
                }
              })
            }
          }
          if (data.result.success && data.result.redirect) {
            window.location.href = data.result.redirect
          } else if (callback !== undefined) {
            if (typeof callback === 'function') callback(data)
            else if (data.result.success) window.location.replace(callback)
          }
        },
        xhr: () => {
          var xhr = new window.XMLHttpRequest()
          xhr.upload.addEventListener(
            'progress',
            function (evt) {
              if (evt.lengthComputable) {
                var percent = parseInt((100 / evt.total) * evt.loaded)
                if (obj.loading) obj.loading(percent)
              }
            },
            false
          )
          return xhr
        },
        error: () => {
          console.error('CandyJS:', 'Somethings went wrong...', '\nForm: ' + obj.form + '\nRequest: ' + formElement.getAttribute('action'))
        },
        complete: () => {
          const submitButtons = formElement.querySelectorAll('button[type="submit"], input[type="submit"]')
          submitButtons.forEach(btn => {
            btn.disabled = false
            const originalText = btn.getAttribute('data-original-text')
            if (originalText) {
              btn.textContent = originalText
              btn.removeAttribute('data-original-text')
            }
          })
          formElement.querySelectorAll('input:not([type="hidden"]), textarea, select').forEach(el => (el.disabled = false))
        }
      })
    }

    document.addEventListener('submit', handler)
    this.#formSubmitHandlers.set(formSelector, handler)
  }

  get(url, callback) {
    url = url + '?_token=' + this.token()
    this.#ajax({url: url, success: callback})
  }

  page() {
    if (!this.#page) {
      let data = this.data()
      if (data !== null) this.#page = data.page
      else this.token(true)
    }
    return this.#page
  }

  storage(key, value) {
    if (value === undefined) return localStorage.getItem(key)
    else if (value === null) return localStorage.removeItem(key)
    else localStorage.setItem(key, value)
  }

  token() {
    let data = this.data()
    if (!this.#token.listener) {
      document.addEventListener('candy:ajaxSuccess', event => {
        const {detail} = event
        const {xhr, requestUrl} = detail
        if (requestUrl.includes('://')) return false
        try {
          const token = xhr.getResponseHeader('X-Candy-Token')
          if (token) this.#token.hash.push(token)
          if (this.#token.hash.length > 2) this.#token.hash.shift()
        } catch (e) {
          console.error('Error in ajaxSuccess token handler:', e)
        }
      })
      this.#token.listener = true // Mark as listener attached
    }
    if (!this.#token.hash.length) {
      if (!this.#token.data && data) {
        this.#page = data.page
        this.#token.hash.push(data.token)
        this.#token.data = true
      } else {
        var req = new XMLHttpRequest()
        req.open('GET', '/', false)
        req.setRequestHeader('X-Candy', 'token')
        req.setRequestHeader('X-Candy-Client', this.client())
        req.send(null)
        var req_data = JSON.parse(req.response)
        if (req_data.token) this.#token.hash.push(req_data.token)
      }
    }
    this.#token.hash.filter(n => n)
    var return_token = this.#token.hash.shift()
    if (!this.#token.hash.length)
      this.#ajax({
        url: '/',
        type: 'GET',
        headers: {'X-Candy': 'token', 'X-Candy-Client': this.client()},
        success: data => {
          var result = JSON.parse(JSON.stringify(data))
          if (result.token) this.#token.hash.push(result.token)
        }
      })
    return return_token
  }

  load(url, callback, push = true) {
    if (this.#isNavigating) return false

    const currentUrl = window.location.href

    // Normalize URL to be absolute
    url = new URL(url, currentUrl).href

    if (url === '' || url.startsWith('javascript:') || url.includes('#')) return false

    this.#isNavigating = true

    const currentSkeleton = document.documentElement.dataset.candySkeleton

    this.#ajax({
      url: url,
      type: 'GET',
      headers: {
        'X-Candy': 'ajaxload',
        'X-Candy-Load': Object.keys(this.#loader.elements).join(','),
        'X-Candy-Skeleton': currentSkeleton || ''
      },
      dataType: 'json',
      success: (data, status, xhr) => {
        if (data.skeletonChanged) {
          window.location.href = url
          return
        }

        if (url !== currentUrl && push) {
          window.history.pushState(null, document.title, url)
        }

        const newPage = xhr.getResponseHeader('X-Candy-Page')
        if (newPage) this.#page = newPage

        // Update elements with fade effect
        const elements = Object.entries(this.#loader.elements)
        let completed = 0

        if (elements.length === 0) {
          this.#handleLoadComplete(data, callback)
          return
        }

        elements.forEach(([key, selector]) => {
          const element = document.querySelector(selector)
          if (!element) {
            completed++
            if (completed === elements.length) {
              this.#handleLoadComplete(data, callback)
            }
            return
          }

          this.#fadeOut(element, 400, () => {
            if (data.output && data.output[key]) {
              element.innerHTML = data.output[key]
            }
            this.#fadeIn(element, 400, () => {
              completed++
              if (completed === elements.length) {
                this.#handleLoadComplete(data, callback)
              }
            })
          })
        })
      },
      error: () => {
        this.#isNavigating = false
        window.location.replace(url)
      }
    })
  }

  #handleLoadComplete(data, callback) {
    // Call load actions
    if (this.actions.load) {
      if (Array.isArray(this.actions.load)) {
        this.actions.load.forEach(fn => fn(this.page(), data.variables))
      } else if (typeof this.actions.load === 'function') {
        this.actions.load(this.page(), data.variables)
      }
    }

    // Call page-specific actions
    if (this.actions.page && this.actions.page[this.page()]) {
      const pageActions = this.actions.page[this.page()]
      if (Array.isArray(pageActions)) {
        pageActions.forEach(fn => fn(data.variables))
      } else if (typeof pageActions === 'function') {
        pageActions(data.variables)
      }
    }

    // Call custom callback
    if (callback && typeof callback === 'function') {
      callback(this.page(), data.variables)
    }

    // Scroll to top
    window.scrollTo({top: 0, behavior: 'smooth'})

    this.#isNavigating = false
  }

  loader(selector, elements, callback) {
    this.#loader.elements = elements
    this.#loader.callback = callback

    const candyInstance = this

    // Handle link clicks
    this.#on(document, 'click', selector, function (e) {
      if (e.ctrlKey || e.metaKey) return

      const anchor = this
      if (!anchor) return

      const url = anchor.getAttribute('href')
      const target = anchor.getAttribute('target')

      if (!url || url === '' || url.startsWith('javascript:') || url.startsWith('#')) return

      const currentHost = window.location.host
      const isExternal = url.includes('://') && !url.includes(currentHost)

      if ((target === null || target === '_self') && !isExternal) {
        e.preventDefault()
        candyInstance.load(url, callback)
      }
    })

    // Handle browser back/forward
    window.addEventListener('popstate', () => {
      this.load(window.location.href, callback, false)
    })
  }

  listen(url, onMessage, options = {}) {
    const {onError = null, onOpen = null, autoReconnect = true, reconnectDelay = 3000} = options

    let eventSource = null
    let reconnectTimer = null
    let isClosed = false

    const connect = () => {
      if (isClosed) return

      const urlWithToken = url + (url.includes('?') ? '&' : '?') + '_token=' + encodeURIComponent(this.token())
      eventSource = new EventSource(urlWithToken)

      eventSource.onopen = e => {
        if (onOpen) onOpen(e)
      }

      eventSource.onmessage = e => {
        try {
          const data = JSON.parse(e.data)
          onMessage(data)
        } catch {
          onMessage(e.data)
        }
      }

      eventSource.onerror = e => {
        if (onError) onError(e)

        if (autoReconnect && !isClosed) {
          eventSource.close()
          reconnectTimer = setTimeout(connect, reconnectDelay)
        }
      }
    }

    connect()

    return {
      close: () => {
        isClosed = true
        if (reconnectTimer) clearTimeout(reconnectTimer)
        if (eventSource) eventSource.close()
      },
      send: () => {
        throw new Error('SSE is one-way. Use POST requests to send data.')
      }
    }
  }
}

window.Candy = new candy()

// Auto-initialize navigation from data-candy-navigate attribute
;(function initAutoNavigate() {
  const init = () => {
    const contentEl = document.querySelector('[data-candy-navigate="content"]')
    if (contentEl) {
      window.Candy.loader('a[href^="/"]:not([data-navigate="false"]):not(.no-navigate)', {content: '[data-candy-navigate="content"]'}, null)
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init)
  } else {
    init()
  }
})()

document.addEventListener('DOMContentLoaded', () => {
  const formTypes = ['register', 'login']

  formTypes.forEach(type => {
    const forms = document.querySelectorAll(`form.candy-${type}-form[data-candy-${type}]`)
    forms.forEach(form => {
      const token = form.getAttribute(`data-candy-${type}`)
      window.Candy.form({form: `form[data-candy-${type}="${token}"]`})
    })
  })

  const customForms = document.querySelectorAll('form.candy-custom-form[data-candy-form]')
  customForms.forEach(form => {
    const token = form.getAttribute('data-candy-form')
    window.Candy.form({form: `form[data-candy-form="${token}"]`})
  })
})
