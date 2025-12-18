## 🛡️ Foiling Villains with CSRF Protection

Cross-Site Request Forgery (CSRF) is a scary-sounding attack where a bad guy tries to trick your users into submitting forms they didn't mean to. The `Odac.Token` service is your shield against this!

#### How it Works

The idea is simple:
1.  When you show a form, you generate a secret, one-time-use token.
2.  You put this token in a hidden field in the form.
3.  When the user submits the form, you check if the token they sent back matches the one you generated.

If they don't match, it's a trap!

#### Generating and Checking Tokens

*   `Odac.Token.get()`: Creates a new secret token.
*   `Odac.Token.check(theToken)`: Checks if `theToken` is valid.

#### Example: Securing a Form

**1. Add the token to your form view:**
```html
<form action="/some-action" method="post">
    <!-- Add the secret token here! -->
    <input type="hidden" name="csrf_token" value="{{ csrfToken }}">

    <!-- ... your other form fields ... -->
    <button type="submit">Submit</button>
</form>
```

**2. Your controller that shows the form:**
```javascript
module.exports = function (Odac) {
    // Get a token and pass it to the view
    const token = Odac.Token.get();
    return Odac.View.render('your_form_view', { csrfToken: token });
}
```

**3. Your controller that handles the form submission:**
```javascript
module.exports = function (Odac) {
    const submittedToken = Odac.Request.post.csrf_token;

    // Check the token!
    if (!Odac.Token.check(submittedToken)) {
        // If it's bad, stop right here.
        return Odac.return('Invalid CSRF Token!').status(403);
    }

    // If we get here, the token was good!
    // ...you can now safely process the form...
}
```
