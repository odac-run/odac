## 🔐 User Logins with `Auth.js`

The `Odac.Auth` service is your bouncer, managing who gets in and who stays out. It handles user login sessions for you.

#### Letting a User In

`Odac.Auth.login(userId, userData)`

*   `userId`: A unique ID for the user (like their database ID).
*   `userData`: An object with any user info you want to remember, like their username or role.

When you call this, `Auth` creates a secure session for the user.

#### Checking the Guest List

*   `Odac.Auth.isLogin()`: Is the current user logged in? Returns `true` or `false`.
*   `Odac.Auth.getId()`: Gets the ID of the logged-in user.
*   `Odac.Auth.get('some-key')`: Grabs a specific piece of info from the `userData` you stored.

#### Showing a User Out

*   `Odac.Auth.logout()`: Ends the user's session and logs them out.

#### Example: A Login Flow
```javascript
// Controller for your login form
module.exports = async function (Odac) {
    const { username, password } = Odac.Request.post;

    // IMPORTANT: You need to write your own code to find the user in your database!
    const user = await yourDatabase.findUser(username, password);

    if (user) {
        // User is valid! Log them in.
        Odac.Auth.login(user.id, { username: user.username });
        return Odac.direct('/dashboard'); // Send them to their dashboard
    } else {
        // Bad credentials, send them back to the login page
        return Odac.direct('/login?error=1');
    }
}

// A protected dashboard page
module.exports = function (Odac) {
    // If they're not logged in, kick them back to the login page.
    if (!Odac.Auth.isLogin()) {
        return Odac.direct('/login');
    }

    const username = Odac.Auth.get('username');
    return `Welcome back, ${username}!`;
}
```
