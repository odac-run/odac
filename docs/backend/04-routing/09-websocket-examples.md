# WebSocket Examples

Complete examples for common WebSocket use cases.

## Echo Server

Simple echo server that sends back received messages:

```javascript
// route/websocket.js
Candy.Route.ws('/echo', Candy => {
  Candy.ws.send({type: 'welcome', message: 'Connected!'})

  Candy.ws.on('message', data => {
    Candy.ws.send({type: 'echo', data})
  })
})
```

**Client:**
```javascript
const ws = Candy.ws('/echo')
ws.on('message', data => console.log(data))
ws.send({message: 'Hello!'})
```

## Authenticated Chat Room

### Using auth.ws() (Recommended)

```javascript
Candy.Route.auth.ws('/chat', async Candy => {
  const user = await Candy.Auth.user()

  Candy.ws.join('general')
  Candy.ws.data.user = user

  Candy.ws.to('general').send({
    type: 'user_joined',
    user: user.name
  })

  Candy.ws.on('message', data => {
    Candy.ws.to('general').send({
      type: 'message',
      user: user.name,
      text: data.text
    })
  })

  Candy.ws.on('close', () => {
    Candy.ws.to('general').send({
      type: 'user_left',
      user: user.name
    })
  })
})
```

### Manual Authentication Check

```javascript
Candy.Route.ws('/chat', async Candy => {
  const user = await Candy.Auth.user()
  
  if (!user) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  Candy.ws.join('general')
  Candy.ws.data.user = user

  Candy.ws.to('general').send({
    type: 'user_joined',
    user: user.name
  })

  Candy.ws.on('message', data => {
    Candy.ws.to('general').send({
      type: 'message',
      user: user.name,
      text: data.text
    })
  })

  Candy.ws.on('close', () => {
    Candy.ws.to('general').send({
      type: 'user_left',
      user: user.name
    })
  })
})
```

**Client:**
```javascript
const chat = Candy.ws('/chat')

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
Candy.Route.ws('/room/{roomId}', async Candy => {
  const {roomId} = Candy.Request.data.url
  const user = await Candy.Auth.user()

  if (!user) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  Candy.ws.join(roomId)
  Candy.ws.data.roomId = roomId

  Candy.ws.send({
    type: 'joined',
    room: roomId
  })

  Candy.ws.on('message', data => {
    Candy.ws.to(roomId).send({
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
const room = Candy.ws('/room/gaming')
room.on('message', data => console.log(data))
room.send({text: 'Hi from gaming room!'})
```

## Real-Time Notifications

User-specific notification system:

```javascript
Candy.Route.ws('/notifications', async Candy => {
  const user = await Candy.Auth.user()

  if (!user) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  Candy.ws.join(`user-${user.id}`)
  Candy.ws.data.userId = user.id

  Candy.ws.send({
    type: 'connected',
    unreadCount: await getUnreadCount(user.id)
  })
})

// Send notification from anywhere in your app
async function notifyUser(userId, notification) {
  const wsServer = Candy.Route.wsServer
  wsServer.toRoom(`user-${userId}`, {
    type: 'notification',
    ...notification
  })
}
```

**Client (with cross-tab sharing):**
```javascript
const notifications = Candy.ws('/notifications', {
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
Candy.Route.ws('/broadcast', Candy => {
  Candy.ws.on('message', data => {
    if (data.type === 'broadcast') {
      Candy.ws.broadcast({
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
const broadcast = Candy.ws('/broadcast')

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
Candy.Route.ws('/dashboard', async Candy => {
  const user = await Candy.Auth.user()

  if (!user || !user.isAdmin) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  const sendStats = async () => {
    const stats = await getSystemStats()
    Candy.ws.send({type: 'stats', data: stats})
  }

  sendStats()
  const interval = setInterval(sendStats, 5000)

  Candy.ws.on('close', () => {
    clearInterval(interval)
  })
})
```

**Client:**
```javascript
const dashboard = Candy.ws('/dashboard')

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

module.exports = async Candy => {
  const ip = Candy.Request.ip
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
Candy.Route.use('rate-limit').ws('/chat', Candy => {
  Candy.ws.send({type: 'connected'})
})
```

**Multiple Middleware:**

```javascript
Candy.Route.use('auth', 'rate-limit', 'log-connection').ws('/secure', Candy => {
  Candy.ws.send({type: 'authenticated'})
})
```

## Multiplayer Game

Simple multiplayer game state synchronization:

```javascript
Candy.Route.ws('/game/{gameId}', async Candy => {
  const {gameId} = Candy.Request.data.url
  const user = await Candy.Auth.user()

  if (!user) {
    Candy.ws.close(4001, 'Unauthorized')
    return
  }

  Candy.ws.join(`game-${gameId}`)
  Candy.ws.data.gameId = gameId
  Candy.ws.data.playerId = user.id

  Candy.ws.to(`game-${gameId}`).send({
    type: 'player_joined',
    playerId: user.id,
    name: user.name
  })

  Candy.ws.on('message', data => {
    switch (data.type) {
      case 'move':
        Candy.ws.to(`game-${gameId}`).send({
          type: 'player_moved',
          playerId: user.id,
          position: data.position
        })
        break
      
      case 'action':
        Candy.ws.to(`game-${gameId}`).send({
          type: 'player_action',
          playerId: user.id,
          action: data.action
        })
        break
    }
  })

  Candy.ws.on('close', () => {
    Candy.ws.to(`game-${gameId}`).send({
      type: 'player_left',
      playerId: user.id
    })
  })
})
```

**Client:**
```javascript
const game = Candy.ws('/game/room-123')

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
