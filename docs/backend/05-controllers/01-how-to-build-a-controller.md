## 🏗️ How to Build a Controller

A controller is just a JavaScript module that exports a function. This function automatically gets the magical `Odac` context object we talked about in the overview.

#### A Simple "Hello World" Controller

Check out this basic example from `controller/page/index.js`:

```javascript
// This function is our controller!
module.exports = function (Odac) {
  // It simply returns a string.
  return 'Welcome to my awesome Odac server!'
}
```

This little guy is responsible for handling the homepage route (`/`). When it runs, it just sends a simple string back to the user's browser.
