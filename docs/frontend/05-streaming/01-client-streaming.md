# Client-Side Streaming

Odac provides a simple client-side API for consuming Server-Sent Events (SSE) streams.

### Basic Usage

```javascript
// Simple callback
odac.listen('/events', (data) => {
  console.log('Received:', data)
})
```

### With Options

```javascript
const stream = odac.listen('/events', 
  (data) => {
    console.log('Message:', data)
  },
  {
    onOpen: () => console.log('Connected'),
    onError: (err) => console.error('Error:', err),
    autoReconnect: true,
    reconnectDelay: 3000
  }
)

// Close connection manually
stream.close()
```

### Auto JSON Parsing

```javascript
// Server sends: data: {"message": "Hello"}\n\n
odac.listen('/events', (data) => {
  console.log(data.message) // "Hello"
})
```



## Real-World Examples

### Live Dashboard

```javascript
// Server
odac.Route.get('/dashboard/stats', async (Odac) => {
  odac.stream((send) => {
    const interval = setInterval(async () => {
      const stats = await getServerStats()
      send(stats)
    }, 1000)
    
    // Cleanup function
    return () => {
      clearInterval(interval)
    }
  })
})

// Client
odac.listen('/dashboard/stats', (stats) => {
  document.getElementById('cpu').textContent = stats.cpu + '%'
  document.getElementById('memory').textContent = stats.memory + 'MB'
})
```

### Real-time Notifications

```javascript
// Server
odac.Route.get('/notifications', async (Odac) => {
  const userId = await odac.request('userId')
  
  odac.stream((send) => {
    global.notificationStreams[userId] = { send }
  })
})

// Client
odac.listen('/notifications', (notification) => {
  showToast(notification.message)
})
```

### Build Logs

```javascript
// Server
odac.Route.get('/build/logs', async (Odac) => {
  odac.stream(async function* () {
    for await (const log of getBuildLogs()) {
      yield { timestamp: Date.now(), message: log }
    }
  })
})

// Client
const logs = []
odac.listen('/build/logs', (log) => {
  logs.push(log)
  updateLogsUI(logs)
})
```

## API Reference

### odac.listen(url, onMessage, options)

**Parameters:**
- `url` (string): Stream endpoint URL
- `onMessage` (function): Callback for each message
- `options` (object): Optional configuration

**Options:**
- `onOpen` (function): Called when connection opens
- `onError` (function): Called on error
- `autoReconnect` (boolean): Auto-reconnect on disconnect (default: true)
- `reconnectDelay` (number): Delay before reconnect in ms (default: 3000)

**Returns:**
- `close()` (function): Close the connection



## Best Practices

1. **Always cleanup:** Close streams when component unmounts
2. **Handle errors:** Provide error UI feedback
3. **Show connection status:** Let users know when disconnected
4. **Limit data:** Don't store unlimited messages in state
5. **Throttle updates:** Use debounce for frequent updates

## Troubleshooting

**Connection keeps dropping:**
- Check server heartbeat (should be < 60s)
- Verify network stability
- Check browser console for errors

**Messages not received:**
- Verify JSON format from server
- Check CORS headers
- Ensure EventSource is supported

**Memory leaks:**
- Always call `close()` or use hooks
- Limit stored messages
- Clear intervals/timers

## Browser Support

- ✅ Chrome 6+
- ✅ Firefox 6+
- ✅ Safari 5+
- ✅ Edge (all versions)
- ✅ Opera 11+
- ❌ IE (not supported)

For IE support, use a polyfill like `event-source-polyfill`.
