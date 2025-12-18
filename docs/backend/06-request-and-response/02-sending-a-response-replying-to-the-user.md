## 📤 Sending a Response: Replying to the User

Once you've processed the request, it's time to send something back. You've got a few options.

#### The Simple Way: Just Return It!

For many cases, you can just `return` a value from your controller. Odac is smart enough to figure out what to do.

```javascript
// Return some HTML
module.exports = function (Odac) {
  return '<h1>Welcome to the site!</h1>';
}

// Return some JSON for an API
module.exports = function (Odac) {
  return { status: 'success', message: 'Your data was saved!' };
}
```

#### The Helper Functions: More Control

Need a bit more control? The `Odac` object has your back.

*   `Odac.return(data)`: Does the same thing as a direct return, but you can call it from anywhere in your function. It stops everything and sends the response immediately.
*   `Odac.direct(url)`: Need to send the user to a different page? This function performs a redirect, telling the user's browser to go to a new URL.

**Example:**
```javascript
module.exports = function (Odac) {
  // If the user isn't logged in...
  if (!Odac.Auth.isLogin()) {
    // ...send them to the login page!
    return Odac.direct('/login');
  }

  // Otherwise, give them their data.
  Odac.return({ data: 'here is your secret stuff' });
}
```
