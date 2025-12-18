## 🤝 Your trusty `Odac` Assistant

Remember the `Odac` object? It's your best friend inside a controller. It's passed to your controller function and gives you all the tools you need for the current request.

#### Awesome Services at Your Fingertips

*   `Odac.Request`: Info about the user's request.
*   `Odac.View`: Renders your HTML pages.
*   `Odac.Auth`: Manages user logins.
*   `Odac.Token`: Protects your forms.
*   `Odac.Lang`: Helps with different languages.

#### Handy Helper Functions

*   `Odac.return(data)`: Send back a response.
*   `Odac.direct(url)`: Redirect the user to a new page.
*   `Odac.cookie(key, value)`: Set a browser cookie.
*   `Odac.validator()`: Check user input easily.
*   `Odac.setInterval(callback, delay)`: Schedule repeating tasks (auto-cleanup).
*   `Odac.setTimeout(callback, delay)`: Schedule one-time tasks (auto-cleanup).
*   `Odac.stream(input)`: Create streaming responses (SSE).

#### Memory-Safe Timers

Always use `Odac.setInterval()` and `Odac.setTimeout()` instead of global functions:

```javascript
module.exports = async (Odac) => {
  // ✅ Good - automatically cleaned up
  Odac.setInterval(() => {
    // This stops when request ends
  }, 1000)
  
  // ❌ Bad - memory leak!
  setInterval(() => {
    // This runs forever
  }, 1000)
}
```

With controllers and the `Odac` object, you have everything you need to start building powerful application logic!
