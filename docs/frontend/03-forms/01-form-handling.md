# Form Handling with odac.js

Learn how to handle forms with automatic AJAX submission, CSRF protection, and validation in Odac.

## Quick Start

### Basic Form

```html
<form id="contact-form" action="/api/contact" method="POST">
  <input name="email" type="email" required>
  <button type="submit">Submit</button>
</form>
```

```javascript
odac.form('#contact-form', function(data) {
  if (data.result.success) {
    alert('Form submitted successfully!')
  }
})
```

That's it! odac.js handles:
- ✅ AJAX submission
- ✅ CSRF token (automatic)
- ✅ Validation errors (auto-generated)
- ✅ Success messages (auto-generated)
- ✅ Loading states

**Note:** Error and success message elements are automatically created if not present in your HTML. You can optionally add them for custom styling or positioning.

## Form Configuration

### Basic Usage

```javascript
odac.form('#my-form', function(data) {
  console.log('Response:', data)
})
```

### With Options

```javascript
odac.form({
  form: '#my-form',
  messages: true,  // Show error/success messages
  loading: function(percent) {
    console.log('Upload progress:', percent + '%')
  }
}, function(data) {
  if (data.result.success) {
    console.log('Success!')
  }
})
```

### Redirect After Submit

```javascript
odac.form('#my-form', '/success-page')
// Redirects to /success-page on success
```

## Error Handling

### Automatic Error Display

By default, errors are automatically displayed next to the input fields when validation fails. The system creates error elements automatically.

### Custom Error Placement (Optional)

If you want to control where errors appear, add error elements manually:

```html
<input name="email" type="email">
<span odac-form-error="email"></span>
```

**If not present:** The system automatically creates `<span odac-form-error="email">` after the input field.

**If present:** The system uses your existing element and updates its content.

### Custom Error Display

```javascript
odac.form('#my-form', function(data) {
  if (!data.result.success) {
    // Custom error handling
    Object.entries(data.errors).forEach(([field, message]) => {
      console.log(`${field}: ${message}`)
    })
  }
})
```

### Styling Errors

```css
/* Error message */
[odac-form-error] {
  color: #ef4444;
  font-size: 0.875rem;
  margin-top: 0.25rem;
  display: none;
}

/* Invalid input */
input._odac_error {
  border-color: #ef4444;
}
```

## Success Messages

### Automatic Success Display

Success messages are automatically displayed at the end of the form when submission succeeds.

### Custom Success Placement (Optional)

If you want to control where success messages appear, add a success element manually:

```html
<form id="my-form" action="/api/submit" method="POST">
  <!-- form fields -->
  <button type="submit">Submit</button>
  <div odac-form-success></div>
</form>
```

**If not present:** The system automatically creates `<span odac-form-success>` at the end of the form.

**If present:** The system uses your existing element and updates its content.

### Custom Success Message

```javascript
odac.form('#my-form', function(data) {
  if (data.result.success) {
    document.querySelector('#custom-message').innerHTML = 
      'Thank you! Your form has been submitted.'
  }
})
```

## File Uploads

### Basic File Upload

```html
<form id="upload-form" action="/api/upload" method="POST">
  <input name="file" type="file" required>
  <button type="submit">Upload</button>
</form>
```

```javascript
odac.form('#upload-form', function(data) {
  if (data.result.success) {
    console.log('File uploaded:', data.result.filename)
  }
})
```

### Upload Progress

```javascript
odac.form({
  form: '#upload-form',
  loading: function(percent) {
    document.querySelector('#progress').style.width = percent + '%'
    document.querySelector('#progress-text').textContent = percent + '%'
  }
}, function(data) {
  console.log('Upload complete!')
})
```

### Multiple Files

```html
<input name="files[]" type="file" multiple>
```

## Advanced Features

### Disable Messages

```javascript
odac.form({
  form: '#my-form',
  messages: false  // Don't show automatic messages
}, function(data) {
  // Handle messages manually
})
```

### Disable Specific Messages

```javascript
odac.form({
  form: '#my-form',
  messages: ['error']  // Only show errors, not success
}, function(data) {
  // Custom success handling
})
```

### Form Reset

```javascript
odac.form('#my-form', function(data) {
  if (data.result.success) {
    // Reset the form
    document.querySelector('#my-form').reset()
  }
})
```

### Conditional Submission

```javascript
document.querySelector('#my-form').addEventListener('submit', function(e) {
  if (!confirm('Are you sure?')) {
    e.preventDefault()
    e.stopPropagation()
  }
})

odac.form('#my-form', function(data) {
  console.log('Submitted!')
})
```

## Server-Side Setup

### Controller Example

```javascript
// controller/post/contact.js
module.exports = async function(Odac) {
  const email = await odac.Request.request('email')
  const message = await odac.Request.request('message')
  
  // Validation
  const errors = {}
  if (!email) errors.email = 'Email is required'
  if (!message) errors.message = 'Message is required'
  
  if (Object.keys(errors).length > 0) {
    return {
      result: {success: false},
      errors: errors
    }
  }
  
  // Process form
  // ... send email, save to database, etc.
  
  return {
    result: {
      success: true,
      message: 'Thank you for your message!'
    }
  }
}
```

### Route Setup

```javascript
// route/www.js
odac.Route.post('/api/contact', 'contact')
```

## Validation

### Client-Side Validation

Use HTML5 validation:

```html
<input name="email" type="email" required>
<input name="age" type="number" min="18" max="100">
<input name="website" type="url">
```

### Server-Side Validation

Always validate on the server:

```javascript
module.exports = async function(Odac) {
  const email = await odac.Request.request('email')
  
  const errors = {}
  
  // Required field
  if (!email) {
    errors.email = 'Email is required'
  }
  
  // Email format
  else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
    errors.email = 'Invalid email format'
  }
  
  // Check if exists
  else if (await emailExists(email)) {
    errors.email = 'Email already registered'
  }
  
  if (Object.keys(errors).length > 0) {
    return {result: {success: false}, errors}
  }
  
  // Process...
}
```

## Common Patterns

### Contact Form

```html
<form id="contact-form" action="/api/contact" method="POST">
  <input name="name" type="text" required>
  <input name="email" type="email" required>
  <textarea name="message" required></textarea>
  <button type="submit">Send Message</button>
</form>
```

**Note:** Error and success elements are auto-generated. Add them manually only if you need custom positioning or styling.

```javascript
odac.form('#contact-form', function(data) {
  if (data.result.success) {
    document.querySelector('#contact-form').reset()
  }
})
```

### Login Form

```html
<form id="login-form" action="/api/login" method="POST">
  <input name="email" type="email" required>
  <input name="password" type="password" required>
  <button type="submit">Login</button>
</form>
```

```javascript
odac.form('#login-form', function(data) {
  if (data.result.success) {
    // Redirect to dashboard
    window.location.href = '/dashboard'
  }
})
```

### Registration Form

```html
<form id="register-form" action="/api/register" method="POST">
  <input name="name" type="text" required>
  <input name="email" type="email" required>
  <input name="password" type="password" required minlength="8">
  <input name="password_confirm" type="password" required>
  <button type="submit">Register</button>
</form>
```

**Tip:** For better UX, you can add custom error elements for specific positioning:

```html
<div class="form-group">
  <input name="email" type="email" required>
  <span odac-form-error="email" class="error-message"></span>
</div>
```

## Best Practices

1. **Always Validate Server-Side**: Never trust client-side validation alone
2. **Show Clear Errors**: Display errors next to the relevant fields
3. **Provide Feedback**: Show loading states during submission
4. **Reset on Success**: Clear the form after successful submission
5. **Handle Errors Gracefully**: Provide helpful error messages
6. **Use HTTPS**: Always use HTTPS for forms with sensitive data

## Troubleshooting

### Form Not Submitting

- Check that `Odac.form()` is called after DOM is ready
- Verify the form selector is correct
- Check browser console for errors

### Errors Not Displaying

- Errors are automatically created - no manual elements needed
- Check that server returns errors in correct format: `{result: {success: false}, errors: {fieldName: 'message'}}`
- Verify `messages` option is not set to `false`
- If using custom error elements, ensure `odac-form-error` attributes match field names exactly

### CSRF Token Errors

- CSRF tokens are handled automatically
- If you get token errors, check server configuration
- Ensure cookies are enabled

## Next Steps

- Learn about [Validation](02-validation.md)
- Explore [File Uploads](03-file-uploads.md)
- Check [API Requests](../04-api-requests/01-get-post.md)
