## 🤝 Your trusty `Candy` Assistant

Remember the `Candy` object? It's your best friend inside a controller. It's passed to your controller function and gives you all the tools you need for the current request.

#### Awesome Services at Your Fingertips

*   `Candy.Request`: Info about the user's request.
*   `Candy.View`: Renders your HTML pages.
*   `Candy.Auth`: Manages user logins.
*   `Candy.Token`: Protects your forms.
*   `Candy.Lang`: Helps with different languages.

#### Handy Helper Functions

*   `Candy.return(data)`: Send back a response.
*   `Candy.direct(url)`: Redirect the user to a new page.
*   `Candy.cookie(key, value)`: Set a browser cookie.
*   `Candy.validator()`: Check user input easily.
*   `Candy.setInterval(callback, delay)`: Schedule repeating tasks (auto-cleanup).
*   `Candy.setTimeout(callback, delay)`: Schedule one-time tasks (auto-cleanup).
*   `Candy.stream(input)`: Create streaming responses (SSE).

#### Memory-Safe Timers

Always use `Candy.setInterval()` and `Candy.setTimeout()` instead of global functions:

```javascript
module.exports = async (Candy) => {
  // ✅ Good - automatically cleaned up
  Candy.setInterval(() => {
    // This stops when request ends
  }, 1000)
  
  // ❌ Bad - memory leak!
  setInterval(() => {
    // This runs forever
  }, 1000)
}
```

With controllers and the `Candy` object, you have everything you need to start building powerful application logic!
