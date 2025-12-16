# WebSocket Routes

CandyPack provides built-in WebSocket support for real-time bidirectional communication.

## Basic Usage

Define WebSocket routes in your route files using `Candy.ws()`:

```javascript
// route/main.js
Candy.ws('/chat', (ws, Candy) => {
  ws.send({type: 'welcome', message: 'Connected!'})

  ws.on('message', data => {
    console.log('Received:', data)
    ws.send({type: 'echo', data})
  })

  ws.on('close', () => {
    console.log('Client disconnected')
  })
})
```

## WebSocket Client Methods

### Sending Messages

```javascript
ws.send({type: 'message', text: 'Hello'})  // JSON object
ws.send('Plain text message')               // String
ws.sendBinary(buffer)                       // Binary data
```

### Event Handlers

```javascript
ws.on('message', data => {})  // Incoming message
ws.on('close', () => {})      // Connection closed
ws.on('error', err => {})     // Error occurred
```

### Connection Management

```javascript
ws.close()           // Close connection
ws.ping()            // Send ping frame
ws.id                // Unique client ID
```

## Rooms

Group clients into rooms for targeted broadcasting:

```javascript
Candy.ws('/game', (ws, Candy) => {
  const roomId = Candy.Request.data.url.room || 'lobby'
  
  ws.join(roomId)
  
  ws.on('message', data => {
    ws.to(roomId).send({
      type: 'chat',
      message: data.message
    })
  })

  ws.on('close', () => {
    ws.leave(roomId)
  })
})
```

## Broadcasting

```javascript
// Send to all clients except sender
ws.broadcast({type: 'notification', text: 'New user joined'})

// Send to all clients in a room
ws.to('room-name').send({type: 'update', data: {}})
```

## URL Parameters

WebSocket routes support dynamic parameters:

```javascript
Candy.ws('/room/{roomId}/user/{userId}', (ws, Candy) => {
  const {roomId, userId} = Candy.Request.data.url
  
  ws.join(roomId)
  ws.data.userId = userId
})
```

## Authentication

Access the Candy context for authentication:

```javascript
Candy.ws('/secure', async (ws, Candy) => {
  const isAuthenticated = await Candy.Auth.check()
  
  if (!isAuthenticated) {
    ws.send({error: 'Unauthorized'})
    ws.close(4001, 'Unauthorized')
    return
  }

  const user = await Candy.Auth.user()
  ws.data.user = user
})
```

## Client Data Storage

Store per-connection data:

```javascript
ws.data.username = 'john'
ws.data.joinedAt = Date.now()
```

## Real-Time Notifications Example

```javascript
Candy.ws('/notifications', async (ws, Candy) => {
  const user = await Candy.Auth.user()
  if (!user) {
    ws.close(4001, 'Unauthorized')
    return
  }

  ws.data.userId = user.id
  ws.join(`user-${user.id}`)

  ws.on('close', () => {
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
