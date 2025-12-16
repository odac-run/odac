# WebSocket Quick Reference

Quick reference for CandyPack WebSocket API.

## Backend API

### Route Definition
```javascript
Candy.Route.ws('/path', (ws, Candy) => {
  // Handler
})
```

### WebSocket Client Methods

| Method | Description |
|--------|-------------|
| `ws.send(data)` | Send JSON data to client |
| `ws.sendBinary(buffer)` | Send binary data |
| `ws.close(code, reason)` | Close connection |
| `ws.ping()` | Send ping frame |
| `ws.join(room)` | Join a room |
| `ws.leave(room)` | Leave a room |
| `ws.to(room).send(data)` | Send to room |
| `ws.broadcast(data)` | Send to all clients |
| `ws.on(event, handler)` | Add event listener |
| `ws.off(event, handler)` | Remove event listener |

### WebSocket Client Properties

| Property | Description |
|----------|-------------|
| `ws.id` | Unique client ID |
| `ws.rooms` | Array of joined rooms |
| `ws.data` | Custom data storage |

### Events

| Event | Description |
|-------|-------------|
| `message` | Incoming message |
| `close` | Connection closed |
| `error` | Error occurred |
| `pong` | Pong received |

### Server Methods

| Method | Description |
|--------|-------------|
| `Candy.Route.wsServer.clients` | Map of all clients |
| `Candy.Route.wsServer.clientCount` | Number of clients |
| `Candy.Route.wsServer.toRoom(room, data)` | Send to room |
| `Candy.Route.wsServer.broadcast(data)` | Broadcast to all |

## Frontend API

### Connection
```javascript
const ws = Candy.ws('/path', options)
```

### Backend Options

| Option | Default | Description |
|--------|---------|-------------|
| `token` | `true` | Require CSRF token |

### Client Options

| Option | Default | Description |
|--------|---------|-------------|
| `autoReconnect` | `true` | Auto-reconnect on disconnect |
| `reconnectDelay` | `3000` | Delay between reconnects (ms) |
| `maxReconnectAttempts` | `10` | Max reconnect attempts |
| `shared` | `false` | Share across browser tabs |
| `token` | `true` | Send CSRF token |

### Client Methods

| Method | Description |
|--------|-------------|
| `ws.send(data)` | Send data to server |
| `ws.close()` | Close connection |
| `ws.on(event, handler)` | Add event listener |
| `ws.off(event, handler)` | Remove event listener |

### Client Properties

| Property | Description |
|----------|-------------|
| `ws.connected` | Connection status (boolean) |
| `ws.state` | WebSocket state |

### Events

| Event | Description |
|-------|-------------|
| `open` | Connection opened |
| `message` | Message received |
| `close` | Connection closed |
| `error` | Error occurred |

## Common Patterns

### Echo Server
```javascript
// With token (default)
Candy.Route.ws('/echo', ws => {
  ws.on('message', data => ws.send(data))
})

// Public (no token)
Candy.Route.ws('/public-echo', ws => {
  ws.on('message', data => ws.send(data))
}, {token: false})
```

### Authenticated Route
```javascript
// Using auth.ws() (recommended)
Candy.Route.auth.ws('/secure', async (ws, Candy) => {
  const user = await Candy.Auth.user()
  // Handle connection
})

// Manual check
Candy.Route.ws('/secure', async (ws, Candy) => {
  if (!await Candy.Auth.check()) {
    return ws.close(4001, 'Unauthorized')
  }
  // Handle connection
})
```

### With Middleware
```javascript
Candy.Route.use('auth', 'rate-limit').ws('/chat', (ws, Candy) => {
  ws.send({type: 'welcome'})
})
```

### Room Broadcasting
```javascript
Candy.Route.ws('/chat', (ws, Candy) => {
  ws.join('room-1')
  ws.on('message', data => {
    ws.to('room-1').send(data)
  })
})
```

### URL Parameters
```javascript
Candy.Route.ws('/room/{id}', (ws, Candy) => {
  const {id} = Candy.Request.data.url
  ws.join(id)
})
```

### Shared Connection (Client)
```javascript
const ws = Candy.ws('/chat', {shared: true})
// All tabs share one connection
```

## Status Codes

| Code | Description |
|------|-------------|
| `1000` | Normal closure |
| `1001` | Going away |
| `1002` | Protocol error |
| `1003` | Unsupported data |
| `1006` | Abnormal closure |
| `4000` | Middleware rejected |
| `4001` | Unauthorized |
| `4002` | Invalid/missing token |
| `4003` | Forbidden (middleware) |

## Best Practices

1. **Always handle authentication**
   ```javascript
   if (!await Candy.Auth.check()) {
     return ws.close(4001, 'Unauthorized')
   }
   ```

2. **Use rooms for targeted messaging**
   ```javascript
   ws.join(`user-${userId}`)
   ```

3. **Clean up on close**
   ```javascript
   ws.on('close', () => {
     clearInterval(interval)
     ws.leave('room')
   })
   ```

4. **Store per-connection data**
   ```javascript
   ws.data.userId = user.id
   ws.data.joinedAt = Date.now()
   ```

5. **Use shared connections for notifications**
   ```javascript
   const ws = Candy.ws('/notifications', {shared: true})
   ```
