# WebSocket Routes

Odac provides built-in WebSocket support for real-time bidirectional communication.

## Route Definition

WebSocket routes are defined in your route files (e.g., `route/www.js` or `route/websocket.js`) using `Odac.Route.ws()`:

```javascript
// route/websocket.js
Odac.Route.ws('/chat', Odac => {
  Odac.ws.send({type: 'welcome', message: 'Connected!'})

  Odac.ws.on('message', data => {
    console.log('Received:', data)
    Odac.ws.send({type: 'echo', data})
  })

  Odac.ws.on('close', () => {
    console.log('Client disconnected')
  })
})
```

**Handler Signature:**

The handler receives the `Odac` instance as the only parameter. The WebSocket client is accessible via `Odac.ws`, providing a consistent API with HTTP routes where everything is accessed through the `Odac` object.

**CSRF Token Protection:**

By default, WebSocket routes require a valid CSRF token (like `Route.get()` and `Route.post()`). The token is sent via the `Sec-WebSocket-Protocol` header during the initial handshake.

**Disable token requirement:**
```javascript
Odac.Route.ws('/public', Odac => {
  Odac.ws.send({type: 'public'})
}, {token: false})
```

**Route File Structure:**
```
web/
├── route/
│   ├── www.js          # HTTP routes
│   └── websocket.js    # WebSocket routes (recommended)
```

## WebSocket Client API (Odac.ws)

The WebSocket client is accessible via `Odac.ws` in your handler, providing a consistent API pattern with HTTP routes.

### Sending Messages

```javascript
Odac.ws.send({type: 'message', text: 'Hello'})  // JSON object
Odac.ws.send('Plain text message')               // String
Odac.ws.sendBinary(buffer)                       // Binary data
```

### Event Handlers

```javascript
Odac.ws.on('message', data => {})  // Incoming message
Odac.ws.on('close', () => {})      // Connection closed
Odac.ws.on('error', err => {})     // Error occurred
```

### Connection Management

```javascript
Odac.ws.close()           // Close connection
Odac.ws.ping()            // Send ping frame
Odac.ws.id                // Unique client ID
```

## Rooms

Group clients into rooms for targeted broadcasting:

```javascript
Odac.Route.ws('/game', Odac => {
  const roomId = Odac.Request.data.url.room || 'lobby'
  
  Odac.ws.join(roomId)
  
  Odac.ws.on('message', data => {
    Odac.ws.to(roomId).send({
      type: 'chat',
      message: data.message
    })
  })

  Odac.ws.on('close', () => {
    Odac.ws.leave(roomId)
  })
})
```

## Broadcasting

```javascript
// Send to all clients except sender
Odac.ws.broadcast({type: 'notification', text: 'New user joined'})

// Send to all clients in a room
Odac.ws.to('room-name').send({type: 'update', data: {}})
```

## URL Parameters

WebSocket routes support dynamic parameters:

```javascript
Odac.Route.ws('/room/{roomId}/user/{userId}', Odac => {
  const {roomId, userId} = Odac.Request.data.url
  
  Odac.ws.join(roomId)
  Odac.ws.data.userId = userId
})
```

## Authentication

### Manual Authentication Check

```javascript
Odac.Route.ws('/secure', async Odac => {
  const isAuthenticated = await Odac.Auth.check()
  
  if (!isAuthenticated) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  const user = await Odac.Auth.user()
  Odac.ws.data.user = user
})
```

### Using auth.ws() (Recommended)

Automatically requires authentication (also requires token by default):

```javascript
Odac.Route.auth.ws('/secure', async Odac => {
  const user = await Odac.Auth.user()
  Odac.ws.data.user = user
  
  Odac.ws.send({
    type: 'welcome',
    user: user.name
  })
})
```

If the user is not authenticated, the connection is automatically closed with code `4001`.

### Options

```javascript
Odac.Route.ws('/path', handler, {
  token: true  // Require CSRF token (default: true)
})
```

**Examples:**

```javascript
// Public WebSocket (no token, no auth)
Odac.Route.ws('/public', handler, {token: false})

// Token required, no auth (default)
Odac.Route.ws('/chat', handler)

// Both token and auth required
Odac.Route.auth.ws('/secure', handler)
```

## Middleware

WebSocket routes support middleware just like HTTP routes:

```javascript
// Define middleware
Odac.Route.use('auth-check', 'rate-limit').ws('/chat', Odac => {
  Odac.ws.send({type: 'welcome'})
})
```

**Middleware behavior:**
- If middleware returns `false`, connection closes with code `4003` (Forbidden)
- If middleware returns anything other than `true` or `undefined`, connection closes with code `4000`
- Middleware runs before the WebSocket handler

**Example with custom middleware:**

```javascript
// middleware/websocket-auth.js
module.exports = async Odac => {
  const token = Odac.Request.header('Authorization')
  if (!token) return false
  
  const user = await validateToken(token)
  if (!user) return false
  
  Odac.Auth.setUser(user)
  return true
}

// route/websocket.js
Odac.Route.use('websocket-auth').ws('/secure', Odac => {
  Odac.ws.send({type: 'authenticated'})
})
```

## Client Data Storage

Store per-connection data:

```javascript
Odac.ws.data.username = 'john'
Odac.ws.data.joinedAt = Date.now()
```

## Intervals and Timeouts

Use `Odac.setInterval()` and `Odac.setTimeout()` instead of global functions. They are automatically cleaned up when the WebSocket connection closes:

```javascript
Odac.Route.ws('/live-updates', Odac => {
  Odac.setInterval(() => {
    Odac.ws.send({
      type: 'update',
      timestamp: Date.now()
    })
  }, 1000)

  Odac.setTimeout(() => {
    Odac.ws.send({type: 'delayed-message'})
  }, 5000)
})
```

**Why use Odac.setInterval/setTimeout?**
- Prevents memory leaks by auto-cleanup on disconnect
- No need to manually track and clear intervals
- Works seamlessly with WebSocket lifecycle

**Manual cleanup (if needed):**

```javascript
const intervalId = Odac.setInterval(() => {}, 1000)
Odac.clearInterval(intervalId)

const timeoutId = Odac.setTimeout(() => {}, 5000)
Odac.clearTimeout(timeoutId)
```

## Real-Time Notifications Example

```javascript
Odac.Route.ws('/notifications', async Odac => {
  const user = await Odac.Auth.user()
  if (!user) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  Odac.ws.data.userId = user.id
  Odac.ws.join(`user-${user.id}`)

  Odac.ws.on('close', () => {
    console.log(`User ${user.id} disconnected`)
  })
})

// Send notification to specific user from anywhere in your app
function notifyUser(userId, message) {
  const wsServer = Odac.Route.wsServer
  wsServer.toRoom(`user-${userId}`, {
    type: 'notification',
    message
  })
}
```

## Client-Side Usage

Frontend clients can use shared connections across tabs:

```javascript
// All browser tabs share one connection
const ws = Odac.ws('/notifications', {shared: true})

ws.on('message', data => {
  console.log('Notification:', data)
})
```
