# WebSocket Routes

CandyPack provides built-in WebSocket support for real-time bidirectional communication.

## Route Definition

WebSocket routes are defined in your route files (e.g., `route/www.js` or `route/websocket.js`) using `Candy.Route.ws()`:

```javascript
// route/websocket.js
Candy.Route.ws('/chat', Candy => {
  Candy.ws.send({type: 'welcome', message: 'Connected!'})

  Candy.ws.on('message', data => {
    console.log('Received:', data)
    Candy.ws.send({type: 'echo', data})
  })

  Candy.ws.on('close', () => {
    console.log('Client disconnected')
  })
})
```

**Handler Signature:**

The handler receives the `Candy` instance as the only parameter. The WebSocket client is accessible via `Candy.ws`, providing a consistent API with HTTP routes where everything is accessed through the `Candy` object.

**CSRF Token Protection:**

By default, WebSocket routes require a valid CSRF token (like `Route.get()` and `Route.post()`). The token is sent via the `Sec-WebSocket-Protocol` header during the initial handshake.

**Disable token requirement:**
```javascript
Candy.Route.ws('/public', Candy => {
  Candy.ws.send({type: 'public'})
}, {token: false})
```

**Route File Structure:**
```
web/
├── route/
│   ├── www.js          # HTTP routes
│   └── websocket.js    # WebSocket routes (recommended)
```

## WebSocket Client API (Candy.ws)

The WebSocket client is accessible via `Candy.ws` in your handler, providing a consistent API pattern with HTTP routes.

### Sending Messages

```javascript
Candy.ws.send({type: 'message', text: 'Hello'})  // JSON object
Candy.ws.send('Plain text message')               // String
Candy.ws.sendBinary(buffer)                       // Binary data
```

### Event Handlers

```javascript
Candy.ws.on('message', data => {})  // Incoming message
Candy.ws.on('close', () => {})      // Connection closed
Candy.ws.on('error', err => {})     // Error occurred
```

### Connection Management

```javascript
Candy.ws.close()           // Close connection
Candy.ws.ping()            // Send ping frame
Candy.ws.id                // Unique client ID
```

## Rooms

Group clients into rooms for targeted broadcasting:

```javascript
Candy.Route.ws('/game', Candy => {
  const roomId = Candy.Request.data.url.room || 'lobby'
  
  Candy.ws.join(roomId)
  
  Candy.ws.on('message', data => {
    Candy.ws.to(roomId).send({
      type: 'chat',
      message: data.message
    })
  })

  Candy.ws.on('close', () => {
    Candy.ws.leave(roomId)
  })
})
```

## Broadcasting

```javascript
// Send to all clients except sender
Candy.ws.broadcast({type: 'notification', text: 'New user joined'})

// Send to all clients in a room
Candy.ws.to('room-name').send({type: 'update', data: {}})
```

## URL Parameters

WebSocket routes support dynamic parameters:

```javascript
Candy.Route.ws('/room/{roomId}/user/{userId}', Candy => {
  const {roomId, userId} = Candy.Request.data.url
  
  Candy.ws.join(roomId)
  Candy.ws.data.userId = userId
})
```

## Authentication

### Manual Authentication Check

```javascript
Candy.Route.ws('/secure', async Candy => {
  const isAuthenticated = await Candy.Auth.check()
  
  if (!isAuthenticated) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  const user = await Candy.Auth.user()
  Candy.ws.data.user = user
})
```

### Using auth.ws() (Recommended)

Automatically requires authentication (also requires token by default):

```javascript
Candy.Route.auth.ws('/secure', async Candy => {
  const user = await Candy.Auth.user()
  Candy.ws.data.user = user
  
  Candy.ws.send({
    type: 'welcome',
    user: user.name
  })
})
```

If the user is not authenticated, the connection is automatically closed with code `4001`.

### Options

```javascript
Candy.Route.ws('/path', handler, {
  token: true  // Require CSRF token (default: true)
})
```

**Examples:**

```javascript
// Public WebSocket (no token, no auth)
Candy.Route.ws('/public', handler, {token: false})

// Token required, no auth (default)
Candy.Route.ws('/chat', handler)

// Both token and auth required
Candy.Route.auth.ws('/secure', handler)
```

## Middleware

WebSocket routes support middleware just like HTTP routes:

```javascript
// Define middleware
Candy.Route.use('auth-check', 'rate-limit').ws('/chat', Candy => {
  Candy.ws.send({type: 'welcome'})
})
```

**Middleware behavior:**
- If middleware returns `false`, connection closes with code `4003` (Forbidden)
- If middleware returns anything other than `true` or `undefined`, connection closes with code `4000`
- Middleware runs before the WebSocket handler

**Example with custom middleware:**

```javascript
// middleware/websocket-auth.js
module.exports = async Candy => {
  const token = Candy.Request.header('Authorization')
  if (!token) return false
  
  const user = await validateToken(token)
  if (!user) return false
  
  Candy.Auth.setUser(user)
  return true
}

// route/websocket.js
Candy.Route.use('websocket-auth').ws('/secure', Candy => {
  Candy.ws.send({type: 'authenticated'})
})
```

## Client Data Storage

Store per-connection data:

```javascript
Candy.ws.data.username = 'john'
Candy.ws.data.joinedAt = Date.now()
```

## Real-Time Notifications Example

```javascript
Candy.Route.ws('/notifications', async Candy => {
  const user = await Candy.Auth.user()
  if (!user) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  Candy.ws.data.userId = user.id
  Candy.ws.join(`user-${user.id}`)

  Candy.ws.on('close', () => {
    console.log(`User ${user.id} disconnected`)
  })
})

// Send notification to specific user from anywhere in your app
function notifyUser(userId, message) {
  const wsServer = Candy.Route.wsServer
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
const ws = Candy.ws('/notifications', {shared: true})

ws.on('message', data => {
  console.log('Notification:', data)
})
```
