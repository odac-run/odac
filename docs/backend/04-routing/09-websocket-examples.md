# WebSocket Examples

Complete examples for common WebSocket use cases.

## Echo Server

Simple echo server that sends back received messages:

```javascript
// route/websocket.js
Odac.Route.ws('/echo', Odac => {
  Odac.ws.send({type: 'welcome', message: 'Connected!'})

  Odac.ws.on('message', data => {
    Odac.ws.send({type: 'echo', data})
  })
})
```

**Client:**
```javascript
const ws = Odac.ws('/echo')
ws.on('message', data => console.log(data))
ws.send({message: 'Hello!'})
```

## Authenticated Chat Room

### Using auth.ws() (Recommended)

```javascript
Odac.Route.auth.ws('/chat', async Odac => {
  const user = await Odac.Auth.user()

  Odac.ws.join('general')
  Odac.ws.data.user = user

  Odac.ws.to('general').send({
    type: 'user_joined',
    user: user.name
  })

  Odac.ws.on('message', data => {
    Odac.ws.to('general').send({
      type: 'message',
      user: user.name,
      text: data.text
    })
  })

  Odac.ws.on('close', () => {
    Odac.ws.to('general').send({
      type: 'user_left',
      user: user.name
    })
  })
})
```

### Manual Authentication Check

```javascript
Odac.Route.ws('/chat', async Odac => {
  const user = await Odac.Auth.user()
  
  if (!user) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  Odac.ws.join('general')
  Odac.ws.data.user = user

  Odac.ws.to('general').send({
    type: 'user_joined',
    user: user.name
  })

  Odac.ws.on('message', data => {
    Odac.ws.to('general').send({
      type: 'message',
      user: user.name,
      text: data.text
    })
  })

  Odac.ws.on('close', () => {
    Odac.ws.to('general').send({
      type: 'user_left',
      user: user.name
    })
  })
})
```

**Client:**
```javascript
const chat = Odac.ws('/chat')

chat.on('message', data => {
  if (data.type === 'message') {
    console.log(`${data.user}: ${data.text}`)
  }
})

chat.send({text: 'Hello everyone!'})
```

## Room-Based Chat with URL Parameters

Dynamic rooms using URL parameters:

```javascript
Odac.Route.ws('/room/{roomId}', async Odac => {
  const {roomId} = Odac.Request.data.url
  const user = await Odac.Auth.user()

  if (!user) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  Odac.ws.join(roomId)
  Odac.ws.data.roomId = roomId

  Odac.ws.send({
    type: 'joined',
    room: roomId
  })

  Odac.ws.on('message', data => {
    Odac.ws.to(roomId).send({
      type: 'message',
      user: user.name,
      text: data.text,
      room: roomId
    })
  })
})
```

**Client:**
```javascript
const room = Odac.ws('/room/gaming')
room.on('message', data => console.log(data))
room.send({text: 'Hi from gaming room!'})
```

## Real-Time Notifications

User-specific notification system:

```javascript
Odac.Route.ws('/notifications', async Odac => {
  const user = await Odac.Auth.user()

  if (!user) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  Odac.ws.join(`user-${user.id}`)
  Odac.ws.data.userId = user.id

  Odac.ws.send({
    type: 'connected',
    unreadCount: await getUnreadCount(user.id)
  })
})

// Send notification from anywhere in your app
async function notifyUser(userId, notification) {
  const wsServer = Odac.Route.wsServer
  wsServer.toRoom(`user-${userId}`, {
    type: 'notification',
    ...notification
  })
}
```

**Client (with cross-tab sharing):**
```javascript
const notifications = Odac.ws('/notifications', {
  shared: true,
  autoReconnect: true
})

notifications.on('message', data => {
  if (data.type === 'notification') {
    showNotification(data.title, data.message)
  }
})
```

## Broadcasting System

Broadcast messages to all connected clients:

```javascript
Odac.Route.ws('/broadcast', Odac => {
  Odac.ws.on('message', data => {
    if (data.type === 'broadcast') {
      Odac.ws.broadcast({
        type: 'announcement',
        message: data.message,
        timestamp: Date.now()
      })
    }
  })
})
```

**Client:**
```javascript
const broadcast = Odac.ws('/broadcast')

broadcast.on('message', data => {
  if (data.type === 'announcement') {
    alert(data.message)
  }
})

// Send to all clients
broadcast.send({
  type: 'broadcast',
  message: 'Server maintenance in 5 minutes'
})
```

## Live Dashboard

Real-time data updates for dashboards:

```javascript
Odac.Route.ws('/dashboard', async Odac => {
  const user = await Odac.Auth.user()

  if (!user || !user.isAdmin) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  const sendStats = async () => {
    const stats = await getSystemStats()
    Odac.ws.send({type: 'stats', data: stats})
  }

  sendStats()
  const interval = setInterval(sendStats, 5000)

  Odac.ws.on('close', () => {
    clearInterval(interval)
  })
})
```

**Client:**
```javascript
const dashboard = Odac.ws('/dashboard')

dashboard.on('message', data => {
  if (data.type === 'stats') {
    updateDashboard(data.data)
  }
})
```

## WebSocket with Middleware

Use middleware for rate limiting, authentication, or custom logic:

```javascript
// middleware/rate-limit.js
const connections = new Map()

module.exports = async Odac => {
  const ip = Odac.Request.ip
  const now = Date.now()
  
  if (connections.has(ip)) {
    const lastConnection = connections.get(ip)
    if (now - lastConnection < 1000) {
      return false // Too many connections
    }
  }
  
  connections.set(ip, now)
  return true
}

// route/websocket.js
Odac.Route.use('rate-limit').ws('/chat', Odac => {
  Odac.ws.send({type: 'connected'})
})
```

**Multiple Middleware:**

```javascript
Odac.Route.use('auth', 'rate-limit', 'log-connection').ws('/secure', Odac => {
  Odac.ws.send({type: 'authenticated'})
})
```

## Multiplayer Game

Simple multiplayer game state synchronization:

```javascript
Odac.Route.ws('/game/{gameId}', async Odac => {
  const {gameId} = Odac.Request.data.url
  const user = await Odac.Auth.user()

  if (!user) {
    Odac.ws.close(4001, 'Unauthorized')
    return
  }

  Odac.ws.join(`game-${gameId}`)
  Odac.ws.data.gameId = gameId
  Odac.ws.data.playerId = user.id

  Odac.ws.to(`game-${gameId}`).send({
    type: 'player_joined',
    playerId: user.id,
    name: user.name
  })

  Odac.ws.on('message', data => {
    switch (data.type) {
      case 'move':
        Odac.ws.to(`game-${gameId}`).send({
          type: 'player_moved',
          playerId: user.id,
          position: data.position
        })
        break
      
      case 'action':
        Odac.ws.to(`game-${gameId}`).send({
          type: 'player_action',
          playerId: user.id,
          action: data.action
        })
        break
    }
  })

  Odac.ws.on('close', () => {
    Odac.ws.to(`game-${gameId}`).send({
      type: 'player_left',
      playerId: user.id
    })
  })
})
```

**Client:**
```javascript
const game = Odac.ws('/game/room-123')

game.on('message', data => {
  switch (data.type) {
    case 'player_joined':
      addPlayer(data.playerId, data.name)
      break
    case 'player_moved':
      updatePlayerPosition(data.playerId, data.position)
      break
    case 'player_action':
      handlePlayerAction(data.playerId, data.action)
      break
  }
})

// Send player movement
game.send({
  type: 'move',
  position: {x: 100, y: 200}
})
```
