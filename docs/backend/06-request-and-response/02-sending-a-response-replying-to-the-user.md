## 📤 Sending a Response: Replying to the User

Once you've processed the request, it's time to send something back. You've got a few options.

#### The Simple Way: Just Return It!

For many cases, you can just `return` a value from your controller. Odac is smart enough to figure out what to do.

```javascript
// Return some HTML
module.exports = function (Candy) {
  return '<h1>Welcome to the site!</h1>';
}

// Return some JSON for an API
module.exports = function (Candy) {
  return { status: 'success', message: 'Your data was saved!' };
}
```

#### The Helper Functions: More Control

Need a bit more control? The `Candy` object has your back.

*   `Candy.return(data)`: Does the same thing as a direct return, but you can call it from anywhere in your function. It stops everything and sends the response immediately.
*   `Candy.direct(url)`: Need to send the user to a different page? This function performs a redirect, telling the user's browser to go to a new URL.

**Example:**
```javascript
module.exports = function (Candy) {
  // If the user isn't logged in...
  if (!Candy.Auth.isLogin()) {
    // ...send them to the login page!
    return Candy.direct('/login');
  }

  // Otherwise, give them their data.
  Candy.return({ data: 'here is your secret stuff' });
}
```
