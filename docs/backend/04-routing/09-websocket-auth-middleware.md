# WebSocket Authentication & Middleware

Advanced authentication and middleware patterns for WebSocket routes.

## Authentication Methods

### 1. Using auth.ws() (Recommended)

The simplest way to require authentication:

```javascript
Odac.Route.auth.ws('/secure', async Odac => {
  const user = await Odac.Auth.user()
  
  Odac.ws.send({
    type: 'authenticated',
    user: user.name
  })
})
```

**Behavior:**
- Automatically checks authentication before handler runs
- Closes connection with code `4001` if not authenticated
- No need for manual auth checks

### 2. Manual Authentication Check

For custom authentication logic:

```javascript
Odac.Route.ws('/custom-auth', async Odac => {
  const token = Odac.Request.header('Authorization')
  
  if (!token) {
    Odac.ws.close(4001, 'Missing token')
    return
  }

  const user = await validateCustomToken(token)
  
  if (!user) {
    Odac.ws.close(4001, 'Invalid token')
    return
  }

  Odac.ws.data.user = user
  Odac.ws.send({type: 'authenticated'})
})
```

## Middleware Support

### Basic Middleware

```javascript
Odac.Route.use('rate-limit').ws('/chat', Odac => {
  Odac.ws.send({type: 'connected'})
})
```

### Multiple Middleware

Middleware runs in order:

```javascript
Odac.Route.use('auth', 'rate-limit', 'log').ws('/chat', Odac => {
  Odac.ws.send({type: 'ready'})
})
```

### Creating WebSocket Middleware

**middleware/websocket-rate-limit.js:**

```javascript
const connections = new Map()
const RATE_LIMIT = 5
const WINDOW = 60000

module.exports = async Odac => {
  const ip = Odac.Request.ip
  const now = Date.now()
  
  if (!connections.has(ip)) {
    connections.set(ip, [])
  }
  
  const userConnections = connections.get(ip)
  const recentConnections = userConnections.filter(time => now - time < WINDOW)
  
  if (recentConnections.length >= RATE_LIMIT) {
    return false
  }
  
  recentConnections.push(now)
  connections.set(ip, recentConnections)
  
  return true
}
```

**Usage:**

```javascript
Odac.Route.use('websocket-rate-limit').ws('/chat', Odac => {
  Odac.ws.send({type: 'connected'})
})
```

### Middleware Return Values

| Return Value | Behavior |
|--------------|----------|
| `true` or `undefined` | Continue to next middleware/handler |
| `false` | Close connection with code `4003` (Forbidden) |
| Any other value | Close connection with code `4000` |

### Advanced Middleware Example

**middleware/websocket-auth.js:**

```javascript
module.exports = async Odac => {
  const token = Odac.Request.header('X-WS-Token')
  
  if (!token) {
    console.log('WebSocket connection rejected: No token')
    return false
  }
  
  try {
    const user = await verifyToken(token)
    
    if (!user) {
      console.log('WebSocket connection rejected: Invalid token')
      return false
    }
    
    Odac.Auth.setUser(user)
    return true
    
  } catch (error) {
    console.error('WebSocket auth error:', error)
    return false
  }
}
```

## Combining Auth and Middleware

You can combine `auth.ws()` with middleware:

```javascript
// This won't work - auth.ws() doesn't support chaining
// Odac.Route.use('rate-limit').auth.ws('/chat', handler)

// Instead, use middleware that includes auth check
Odac.Route.use('websocket-auth', 'rate-limit').ws('/chat', Odac => {
  const user = Odac.Auth.user()
  Odac.ws.send({user: user.name})
})

// Or use auth.ws() without middleware
Odac.Route.auth.ws('/chat', Odac => {
  const user = Odac.Auth.user()
  Odac.ws.send({user: user.name})
})
```

## Real-World Examples

### Rate-Limited Chat

```javascript
// middleware/chat-rate-limit.js
const userMessages = new Map()

module.exports = async Odac => {
  const user = await Odac.Auth.user()
  if (!user) return false
  
  const userId = user.id
  const now = Date.now()
  
  if (!userMessages.has(userId)) {
    userMessages.set(userId, [])
  }
  
  const messages = userMessages.get(userId)
  const recentMessages = messages.filter(time => now - time < 10000)
  
  if (recentMessages.length >= 10) {
    return false
  }
  
  recentMessages.push(now)
  userMessages.set(userId, recentMessages)
  
  return true
}

// route/websocket.js
Odac.Route.use('chat-rate-limit').auth.ws('/chat', async Odac => {
  const user = await Odac.Auth.user()
  Odac.ws.join('general')
  
  Odac.ws.on('message', data => {
    Odac.ws.to('general').send({
      user: user.name,
      text: data.text
    })
  })
})
```

### Admin-Only WebSocket

```javascript
// middleware/admin-only.js
module.exports = async Odac => {
  const user = await Odac.Auth.user()
  
  if (!user || !user.isAdmin) {
    return false
  }
  
  return true
}

// route/websocket.js
Odac.Route.use('admin-only').ws('/admin-dashboard', Odac => {
  const sendStats = async () => {
    const stats = await getSystemStats()
    Odac.ws.send({type: 'stats', data: stats})
  }
  
  sendStats()
  const interval = setInterval(sendStats, 5000)
  
  Odac.ws.on('close', () => clearInterval(interval))
})
```

### IP Whitelist

```javascript
// middleware/ip-whitelist.js
const ALLOWED_IPS = ['127.0.0.1', '192.168.1.100']

module.exports = async Odac => {
  const ip = Odac.Request.ip
  
  if (!ALLOWED_IPS.includes(ip)) {
    console.log(`WebSocket connection rejected from ${ip}`)
    return false
  }
  
  return true
}

// route/websocket.js
Odac.Route.use('ip-whitelist').ws('/internal', Odac => {
  Odac.ws.send({type: 'internal-access-granted'})
})
```

## Error Handling

Middleware errors should be handled gracefully:

```javascript
module.exports = async Odac => {
  try {
    const result = await someAsyncOperation()
    return result.isValid
  } catch (error) {
    console.error('Middleware error:', error)
    return false
  }
}
```

## Best Practices

1. **Always return a value** from middleware (true/false/undefined)
2. **Log rejections** for debugging
3. **Clean up resources** in middleware if needed
4. **Use auth.ws()** for simple authentication
5. **Use middleware** for complex logic (rate limiting, logging, etc.)
6. **Combine middleware** for layered security
7. **Test middleware** independently before using in routes
