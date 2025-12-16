const http = require(`http`)

module.exports = {
  init: function () {
    let args = process.argv.slice(2)
    if (args[0] == 'framework' && args[1] == 'run') args = args.slice(2)
    let port = parseInt(args[0] ?? '1071')
    console.log(`CandyPack Server running on \x1b]8;;http://127.0.0.1:${port}\x1b\\\x1b[4mhttp://127.0.0.1:${port}\x1b[0m\x1b]8;;\x1b\\.`)

    const server = http.createServer((req, res) => {
      return Candy.Route.request(req, res)
    })

    server.on('upgrade', (req, socket, head) => {
      const id = `${Date.now()}${Math.random().toString(36).substr(2, 9)}`
      const param = Candy.instance(id, req, null)
      Candy.Route.handleWebSocketUpgrade(req, socket, head, param)
    })

    server.listen(port)
  }
}
