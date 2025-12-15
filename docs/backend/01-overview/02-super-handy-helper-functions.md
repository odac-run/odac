## ✨ Super-Handy Helper Functions

On top of that, `Candy` has some quick-and-easy helper functions:

*   `return(data)`: Quickly send a response back to the user and you're done.
*   `direct(url)`: Need to send the user to another page? This is your tool.
*   `cookie(key, value)`: Leave a little cookie in the user's browser.
*   `env(key, defaultValue)`: Access environment variables with an optional default value.
*   `validator()`: A powerful tool to check the user's submitted data.
*   `setInterval(callback, delay)`: Schedule repeating tasks with automatic cleanup.
*   `setTimeout(callback, delay)`: Schedule one-time tasks with automatic cleanup.
*   `clearInterval(id)`: Manually clear an interval.
*   `clearTimeout(id)`: Manually clear a timeout.

### Memory-Safe Timers

CandyPack provides memory-safe timer functions that automatically clean up when the request ends:

```javascript
module.exports = async (Candy) => {
  // ✅ Automatically cleaned up when request ends
  Candy.setInterval(() => {
    console.log('This will stop when the request ends')
  }, 1000)
  
  // ❌ NOT automatically cleaned up - causes memory leaks
  setInterval(() => {
    console.log('This keeps running forever!')
  }, 1000)
}
```

**Why use Candy timers?**
- Prevents memory leaks
- No orphaned intervals after request ends
- Especially important for streaming/SSE endpoints
- Automatic cleanup on connection close
