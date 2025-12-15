# Streaming API

CandyPack provides a unified streaming API that automatically handles Server-Sent Events (SSE), with future support for WebSocket and HTTP/3.

## Quick Start

### Inline Route (Simple)

```javascript
// route/www.js
Candy.Route.get('/hello', async (Candy) => {
  Candy.stream('Hello World')
})
```

### Controller (Recommended)

```javascript
// route/www.js
Candy.Route.get('/hello', 'hello')

// controller/hello/get/index.js
module.exports = async (Candy) => {
  Candy.stream('Hello World')
}
```

### JSON Message

```javascript
// controller/get/index.js
module.exports = async (Candy) => {
  Candy.stream({ message: 'Hello', time: Date.now() })
}
```

## Multiple Messages

### Callback Pattern with Auto-Cleanup

```javascript
// controller/get/index.js
module.exports = async (Candy) => {
  Candy.stream((send) => {
    send({ type: 'connected' })
    
    // Use Candy.setInterval for automatic cleanup
    Candy.setInterval(() => {
      send({ time: Date.now() })
    }, 1000)
  })
}
```

**Important:** Always use `Candy.setInterval()` and `Candy.setTimeout()` instead of global functions. They are automatically cleaned up when the connection closes.

### Manual Cleanup (Alternative)

```javascript
// controller/get/index.js
module.exports = async (Candy) => {
  Candy.stream((send, close) => {
    send({ type: 'connected' })
    
    const interval = setInterval(() => {
      send({ time: Date.now() })
    }, 1000)
    
    // Return cleanup function
    return () => {
      clearInterval(interval)
    }
  })
}
```

## Automatic Piping

### Array

```javascript
// controller/get/index.js
module.exports = async (Candy) => {
  Candy.stream([1, 2, 3, 4, 5])
}
```

### Async Generator

```javascript
// controller/users/get/index.js
module.exports = async (Candy) => {
  Candy.stream(async function* () {
    const users = await Candy.Mysql.table('users').get()
    
    for (const user of users) {
      yield user
    }
  })
}
```

### Promise

```javascript
// controller/app/get/index.js
module.exports = async (Candy) => {
  Candy.stream(
    fetch('https://api.example.com/data')
      .then(r => r.json())
  )
}
```

### Node.js Stream

```javascript
// controller/file/get/index.js
module.exports = async (Candy) => {
  const fs = require('fs')
  Candy.stream(fs.createReadStream('large-file.txt'))
}
```

## Advanced Usage

### Full Control

```javascript
// controller/monitor/get/index.js
module.exports = async (Candy) => {
  return Candy.stream((send) => {
    send({ type: 'connected' })
    
    // Use Candy.setInterval for automatic cleanup
    Candy.setInterval(() => {
      send({ time: Date.now() })
    }, 1000)
  })
}
```

**Note:** When using `Candy.setInterval()` or `Candy.setTimeout()`, cleanup is automatic. No need for manual `clearInterval()` or `clearTimeout()`.

### Error Handling

```javascript
// controller/data/get/fetch.js
module.exports = async (Candy) => {
  const stream = Candy.stream()
  
  try {
    const data = await fetchData()
    stream.send(data)
  } catch (error) {
    stream.error(error.message)
  }
}
```

## Real-World Examples

### Real-time Logs

```javascript
// route/www.js
Candy.Route.get('/logs', 'logs')

// controller/logs/get/index.js
module.exports = async (Candy) => {
  return Candy.stream(async function* () {
    const logStream = await getDeploymentLogs()
    
    for await (const log of logStream) {
      yield { 
        timestamp: Date.now(),
        message: log 
      }
    }
  })
}
```

### Database Pagination

```javascript
// route/www.js
Candy.Route.get('/posts', 'posts')

// controller/posts/get/index.js
module.exports = async (Candy) => {
  return Candy.stream(async function* () {
    let page = 1
    let hasMore = true
    
    while (hasMore) {
      const posts = await Candy.Mysql.table('posts')
        .limit(10)
        .offset((page - 1) * 10)
        .get()
      
      if (posts.length === 0) {
        hasMore = false
      } else {
        for (const post of posts) {
          yield post
        }
        page++
      }
    }
  })
}
```

## Client-Side Usage

### JavaScript

```javascript
const eventSource = new EventSource('/events')

eventSource.onmessage = (event) => {
  const data = JSON.parse(event.data)
  console.log(data)
}

eventSource.onerror = (error) => {
  console.error('Connection error:', error)
}

// Close connection
eventSource.close()
```

### React Hook

```javascript
import { useEffect, useState } from 'react'

function useStream(url) {
  const [data, setData] = useState(null)
  
  useEffect(() => {
    const eventSource = new EventSource(url)
    
    eventSource.onmessage = (event) => {
      setData(JSON.parse(event.data))
    }
    
    return () => eventSource.close()
  }, [url])
  
  return data
}

// Usage
function Dashboard() {
  const status = useStream('/auth/listen')
  
  return <div>{status?.message}</div>
}
```

## Protocol

CandyPack uses **Server-Sent Events (SSE)** for streaming:
- ✅ One-way communication (server → client)
- ✅ Automatic reconnection
- ✅ Works over HTTP/2
- ✅ No extra ports needed

## Technical Details

- **Protocol:** HTTP/2 compatible
- **Port:** Standard HTTPS (443)
- **Heartbeat:** Automatic (every 30 seconds)
- **Reconnection:** Automatic (browser handles)
- **Compression:** Supported via HTTP/2

## Memory Management

CandyPack automatically manages timers and intervals in streaming contexts:

```javascript
module.exports = async (Candy) => {
  return Candy.stream((send) => {
    // ✅ Automatically cleaned up when connection closes
    Candy.setInterval(() => {
      send({ time: Date.now() })
    }, 1000)
    
    Candy.setTimeout(() => {
      send({ type: 'delayed' })
    }, 5000)
  })
}
```

**Why this matters:**
- Prevents memory leaks
- No orphaned intervals after disconnect
- Automatic cleanup on connection close

**Manual cleanup (if needed):**
```javascript
const intervalId = Candy.setInterval(() => { ... }, 1000)
Candy.clearInterval(intervalId)

const timeoutId = Candy.setTimeout(() => { ... }, 5000)
Candy.clearTimeout(timeoutId)
```

## Best Practices

1. **Use Candy timers:** Always use `Candy.setInterval()` and `Candy.setTimeout()` instead of global functions
2. **Return the stream:** Always `return Candy.stream(...)` from your controller
3. **Throttle messages:** Don't send too frequently (use intervals)
4. **Handle errors:** Use try-catch for async operations
5. **Test reconnection:** Ensure your app handles connection drops

## Troubleshooting

**Connection drops immediately:**
- Check if you're calling `Candy.return()` or `res.end()`
- Don't use both streaming and regular responses
- Make sure to `return Candy.stream(...)`

**Messages not received:**
- Verify JSON format
- Check browser console for errors
- Ensure CORS headers if cross-origin

**High memory usage / Memory leaks:**
- Use `Candy.setInterval()` instead of global `setInterval()`
- Use `Candy.setTimeout()` instead of global `setTimeout()`
- Avoid creating intervals outside the stream callback
- Check for other resource leaks (database connections, file handles)

**Intervals keep running after disconnect:**
- Replace `setInterval()` with `Candy.setInterval()`
- Replace `setTimeout()` with `Candy.setTimeout()`
- These are automatically cleaned up when the connection closes
