# WebSocket Client

candy.js provides a simple WebSocket client with automatic reconnection and cross-tab sharing support.

## Basic Usage

```javascript
const ws = Candy.ws('/chat')

ws.on('open', () => {
  console.log('Connected!')
})

ws.on('message', data => {
  console.log('Received:', data)
})

ws.send({type: 'hello', message: 'Hi there!'})
```

## Configuration Options

```javascript
const ws = Candy.ws('/chat', {
  autoReconnect: true,        // Auto-reconnect on disconnect (default: true)
  reconnectDelay: 3000,       // Delay between reconnect attempts (default: 3000ms)
  maxReconnectAttempts: 10,   // Max reconnect attempts (default: 10)
  shared: false,              // Share connection across browser tabs (default: false)
  token: true                 // Send CSRF token (default: true)
})
```

## CSRF Token Protection

By default, candy.js automatically sends a CSRF token during the WebSocket handshake (similar to AJAX requests). The token is sent via the `Sec-WebSocket-Protocol` header.

**Disable token (for public WebSockets):**
```javascript
const ws = Candy.ws('/public', {token: false})
```

**How it works:**
1. Client calls `Candy.token()` to get current CSRF token
2. Token is sent as `candy-token-{token}` in WebSocket protocol header
3. Server validates token before accepting connection
4. If invalid, connection closes with code `4002`

## Shared WebSocket (Cross-Tab)

Enable `shared: true` to share a single WebSocket connection across all browser tabs:

```javascript
const ws = Candy.ws('/chat', {shared: true})
```

**Benefits:**
- Single connection shared across all tabs
- Reduced server load
- Synchronized state across tabs
- Automatic cleanup when all tabs close

**Browser Support:**
- Uses SharedWorker API (supported in Chrome, Edge, Firefox)
- Falls back to regular WebSocket if SharedWorker is unavailable

**Example:**
```javascript
// Tab 1
const ws = Candy.ws('/notifications', {shared: true})
ws.on('message', data => {
  console.log('Notification:', data)
})

// Tab 2 (same connection)
const ws2 = Candy.ws('/notifications', {shared: true})
ws2.on('message', data => {
  console.log('Same notification:', data)
})
```

## Event Handlers

```javascript
ws.on('open', () => {})       // Connection established
ws.on('message', data => {})  // Message received (auto-parsed JSON)
ws.on('close', event => {})   // Connection closed
ws.on('error', event => {})   // Error occurred
```

## Sending Messages

```javascript
// Objects are automatically JSON-stringified
ws.send({type: 'chat', message: 'Hello!'})

// Strings sent as-is
ws.send('Plain text')
```

## Connection State

```javascript
ws.connected  // true if connected
ws.state      // WebSocket.OPEN, CLOSED, etc.
```

## Closing Connection

```javascript
ws.close()
```

## Removing Event Handlers

```javascript
const handler = data => console.log(data)
ws.on('message', handler)
ws.off('message', handler)  // Remove specific handler
ws.off('message')           // Remove all message handlers
```

## Example: Chat Application

```javascript
const ws = Candy.ws('/chat')
const messages = document.getElementById('messages')
const input = document.getElementById('input')

ws.on('message', data => {
  if (data.type === 'chat') {
    messages.innerHTML += `<p>${data.user}: ${data.text}</p>`
  }
})

document.getElementById('send').onclick = () => {
  ws.send({type: 'chat', text: input.value})
  input.value = ''
}
```
