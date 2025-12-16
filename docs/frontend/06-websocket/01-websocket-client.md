# WebSocket Client

candy.js provides a simple WebSocket client with automatic reconnection.

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
  maxReconnectAttempts: 10    // Max reconnect attempts (default: 10)
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
