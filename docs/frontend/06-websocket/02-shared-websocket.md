# Shared WebSocket (Cross-Tab Communication)

Shared WebSocket allows multiple browser tabs to share a single WebSocket connection using the SharedWorker API.

## Why Use Shared WebSocket?

**Without Shared WebSocket:**
- Each tab creates its own connection
- 5 tabs = 5 WebSocket connections
- Higher server load
- Inconsistent state across tabs

**With Shared WebSocket:**
- All tabs share one connection
- 5 tabs = 1 WebSocket connection
- Lower server load
- Synchronized state across tabs

## Basic Usage

```javascript
const ws = Candy.ws('/chat', {shared: true})

ws.on('message', data => {
  console.log('Message received in this tab:', data)
})

ws.send({message: 'Hello from this tab'})
```

## Real-World Example: Notification System

```javascript
// All tabs share the same notification connection
const notifications = Candy.ws('/notifications', {
  shared: true,
  autoReconnect: true
})

notifications.on('open', () => {
  console.log('Notification system connected')
})

notifications.on('message', data => {
  if (data.type === 'notification') {
    showNotification(data.title, data.message)
  }
})

function showNotification(title, message) {
  if (Notification.permission === 'granted') {
    new Notification(title, {body: message})
  }
}
```

## Chat Application Example

```javascript
// Shared chat connection across all tabs
const chat = Candy.ws('/chat/room/general', {shared: true})

chat.on('message', data => {
  if (data.type === 'message') {
    addMessageToUI(data.user, data.text)
  }
})

document.getElementById('send').onclick = () => {
  const text = document.getElementById('input').value
  chat.send({type: 'message', text})
}

// All tabs will receive the same messages
// Only one connection to the server
```

## Browser Compatibility

Shared WebSocket uses the SharedWorker API:

- ✅ Chrome/Edge: Full support
- ✅ Firefox: Full support
- ❌ Safari: Not supported (falls back to regular WebSocket)

**Automatic Fallback:**
```javascript
// If SharedWorker is not available, automatically falls back to regular WebSocket
const ws = Candy.ws('/chat', {shared: true})
// Works in all browsers, shared only where supported
```

## Lifecycle Management

The shared connection automatically manages its lifecycle:

```javascript
// Tab 1 opens
const ws1 = Candy.ws('/chat', {shared: true})
// Connection established

// Tab 2 opens
const ws2 = Candy.ws('/chat', {shared: true})
// Reuses existing connection

// Tab 1 closes
// Connection stays alive (Tab 2 still using it)

// Tab 2 closes
// Connection automatically closes (no tabs using it)
```

## When to Use Shared WebSocket

**Good Use Cases:**
- Notification systems
- Real-time updates (stock prices, sports scores)
- Chat applications
- Collaborative editing
- Live dashboards

**Not Recommended For:**
- User-specific authenticated connections
- File uploads/downloads
- Connections requiring per-tab state

## Performance Comparison

**Regular WebSocket (5 tabs):**
- Server connections: 5
- Memory usage: ~5MB
- Network overhead: 5x

**Shared WebSocket (5 tabs):**
- Server connections: 1
- Memory usage: ~1MB
- Network overhead: 1x

## Debugging

Check if SharedWorker is being used:

```javascript
const ws = Candy.ws('/chat', {shared: true})

// In Chrome DevTools:
// chrome://inspect/#workers
// You'll see "candy-ws-/chat" if shared
```
