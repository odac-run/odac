class Stream {
  #heartbeat
  #req
  #res
  #closed = false
  #candy

  constructor(req, res, input, candy) {
    this.#req = req
    this.#res = res
    this.#candy = candy
    this.#init()
    this.#handleInput(input)
  }

  #init() {
    if (this.#res.headersSent || this.#res.writableEnded) {
      this.#closed = true
      return
    }

    this.#req.setTimeout(0)
    this.#res.setTimeout(0)

    this.#res.setHeader('Content-Type', 'text/event-stream')
    this.#res.setHeader('Cache-Control', 'no-cache')
    this.#res.setHeader('Connection', 'keep-alive')

    this.#heartbeat = setInterval(() => {
      if (!this.#closed) {
        this.#res.write(': heartbeat\n\n')
      }
    }, 30000)

    const handleClose = () => {
      this.close()
    }

    this.#req.on('close', handleClose)
    this.#res.on('close', handleClose)
  }

  #handleInput(input) {
    if (input === undefined) {
      return
    }

    if (typeof input === 'function') {
      const result = input(
        data => this.send(data),
        () => this.close(),
        this
      )

      if (result && typeof result[Symbol.asyncIterator] === 'function') {
        this.#pipeAsyncIterator(result)
      } else if (result && typeof result.next === 'function') {
        this.#pipeIterator(result)
      } else if (typeof result === 'function') {
        this.on('close', result)
      }

      return
    }

    if (input && typeof input[Symbol.asyncIterator] === 'function') {
      this.#pipeAsyncIterator(input)
      return
    }

    if (input && typeof input.next === 'function') {
      this.#pipeIterator(input)
      return
    }

    if (Array.isArray(input) || (input && typeof input[Symbol.iterator] === 'function')) {
      for (const value of input) {
        this.send(value)
      }
      this.close()
      return
    }

    if (input && typeof input.then === 'function') {
      input
        .then(data => {
          this.send(data)
          this.close()
        })
        .catch(err => {
          this.error(err.message)
          this.close()
        })
      return
    }

    if (input && typeof input.pipe === 'function') {
      input.on('data', chunk => this.send(chunk.toString()))
      input.on('end', () => this.close())
      input.on('error', err => {
        this.error(err.message)
        this.close()
      })
      return
    }

    this.send(input)
    this.close()
  }

  send(data) {
    if (this.#closed) return false
    const message = typeof data === 'string' ? data : JSON.stringify(data)
    this.#res.write(`data: ${message}\n\n`)
    return true
  }

  error(message) {
    return this.send({error: message})
  }

  on(event, callback) {
    if (event === 'close') {
      this.#req.on('close', callback)
    }
  }

  close() {
    if (this.#closed) return
    this.#closed = true
    clearInterval(this.#heartbeat)
    if (!this.#res.writableEnded) {
      this.#res.end()
    }
    if (this.#candy && typeof this.#candy.cleanup === 'function') {
      this.#candy.cleanup()
    }
  }

  #pipeIterator(iterator) {
    const batchSize = 100
    const iterate = () => {
      let count = 0
      while (count < batchSize) {
        const {value, done} = iterator.next()
        if (done) {
          this.close()
          return
        }
        this.send(value)
        count++
      }
      setImmediate(iterate)
    }
    iterate()
  }

  async #pipeAsyncIterator(iterator) {
    try {
      for await (const value of iterator) {
        this.send(value)
      }
      this.close()
    } catch (err) {
      this.error(err.message)
      this.close()
    }
  }
}

module.exports = Stream
