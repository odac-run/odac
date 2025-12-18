# WebSocket Quick Reference

Quick reference for Odac WebSocket API.

## Backend API

### Route Definition
```javascript
Odac.Route.ws('/path', Odac => {
  // Handler - WebSocket client accessible via Odac.ws
})
```

### WebSocket Client Methods (Odac.ws)

| Method | Description |
|--------|-------------|
| `Odac.ws.send(data)` | Send JSON data to client |
| `Odac.ws.sendBinary(buffer)` | Send binary data |
| `Odac.ws.close(code, reason)` | Close connection |
| `Odac.ws.ping()` | Send ping frame |
| `Odac.ws.join(room)` | Join a room |
| `Odac.ws.leave(room)` | Leave a room |
| `Odac.ws.to(room).send(data)` | Send to room |
| `Odac.ws.broadcast(data)` | Send to all clients |
| `Odac.ws.on(event, handler)` | Add event listener |
| `Odac.ws.off(event, handler)` | Remove event listener |

### WebSocket Client Properties

| Property | Description |
|----------|-------------|
| `Odac.ws.id` | Unique client ID |
| `Odac.ws.rooms` | Array of joined rooms |
| `Odac.ws.data` | Custom data storage |

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
| `Odac.Route.wsServer.clients` | Map of all clients |
| `Odac.Route.wsServer.clientCount` | Number of clients |
| `Odac.Route.wsServer.toRoom(room, data)` | Send to room |
| `Odac.Route.wsServer.broadcast(data)` | Broadcast to all |

## Frontend API

### Connection
```javascript
const ws = Odac.ws('/path', options)
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
Odac.Route.ws('/echo', Odac => {
  Odac.ws.on('message', data => Odac.ws.send(data))
})

// Public (no token)
Odac.Route.ws('/public-echo', Odac => {
  Odac.ws.on('message', data => Odac.ws.send(data))
}, {token: false})
```

### Authenticated Route
```javascript
// Using auth.ws() (recommended)
Odac.Route.auth.ws('/secure', async Odac => {
  const user = await Odac.Auth.user()
  // Handle connection
})

// Manual check
Odac.Route.ws('/secure', async Odac => {
  if (!await Odac.Auth.check()) {
    return Odac.ws.close(4001, 'Unauthorized')
  }
  // Handle connection
})
```

### With Middleware
```javascript
Odac.Route.use('auth', 'rate-limit').ws('/chat', Odac => {
  Odac.ws.send({type: 'welcome'})
})
```

### Room Broadcasting
```javascript
Odac.Route.ws('/chat', Odac => {
  Odac.ws.join('room-1')
  Odac.ws.on('message', data => {
    Odac.ws.to('room-1').send(data)
  })
})
```

### URL Parameters
```javascript
Odac.Route.ws('/room/{id}', Odac => {
  const {id} = Odac.Request.data.url
  Odac.ws.join(id)
})
```

### Shared Connection (Client)
```javascript
const ws = Odac.ws('/chat', {shared: true})
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
   if (!await Odac.Auth.check()) {
     return Odac.ws.close(4001, 'Unauthorized')
   }
   ```

2. **Use rooms for targeted messaging**
   ```javascript
   Odac.ws.join(`user-${userId}`)
   ```

3. **Clean up on close**
   ```javascript
   Odac.ws.on('close', () => {
     clearInterval(interval)
     Odac.ws.leave('room')
   })
   ```

4. **Store per-connection data**
   ```javascript
   Odac.ws.data.userId = user.id
   Odac.ws.data.joinedAt = Date.now()
   ```

5. **Use shared connections for notifications**
   ```javascript
   const ws = Odac.ws('/notifications', {shared: true})
   ```
