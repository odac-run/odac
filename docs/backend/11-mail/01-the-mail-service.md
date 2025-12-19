## ✉️ The `Mail` Service

The `Odac.Mail` service is your friendly neighborhood postal worker. It provides a super simple way to send emails using the mail server settings you've already configured in the Odac core.

#### How to Send an Email

`Odac.Mail.send(to, subject, htmlBody)`

*   `to`: The email address of the person you're sending it to.
*   `subject`: The subject line for your email.
*   `htmlBody`: The main content of your email. You can even use HTML tags to make it look fancy!

Just like the database service, `send` is an `async` method, so using it with `async/await` is the way to go.

#### Example: A Simple Contact Form

Let's imagine you have a controller that handles a contact form on your website.

```javascript
module.exports = async function (Odac) {
  const { recipient, subject, message } = Odac.Request.post;

  // It's always a good idea to check your data first!
  if (!recipient || !subject || !message) {
    return { error: 'Oops! You missed a required field.' };
  }

  try {
    // Let's try to send the email
    await Odac.Mail.send(recipient, subject, `<p>${message}</p>`);

    // If we get here, it worked!
    return { success: true, message: 'Email sent successfully!' };
  } catch (error) {
    // If something went wrong...
    console.error('Oh no, the email failed to send:', error);
    return { error: 'Something went wrong while trying to send the email.' };
  }
}
```

And that's all there is to it. You're now a master of email!
