# Custom Forms

CandyPack provides an automatic form system with built-in validation, CSRF protection, and seamless client-side integration. The `<candy:form>` tag allows you to create forms with minimal code while maintaining full control.

## Basic Usage

```html
<candy:form action="/contact/submit" method="POST">
  <candy:field name="email" type="email" label="Email">
    <candy:validate rule="required|email" message="Valid email required"/>
  </candy:field>
  
  <candy:submit text="Send" loading="Sending..."/>
</candy:form>
```

## Form Attributes

### `<candy:form>`

- `action` - Form submission URL (required)
- `method` - HTTP method (default: POST)
- `class` - Additional CSS classes
- `id` - Form ID attribute

```html
<candy:form action="/api/save" method="POST" class="my-form" id="contact-form">
  <!-- fields here -->
</candy:form>
```

## Field Types

### `<candy:field>`

Supports all standard HTML input types:

```html
<!-- Text input -->
<candy:field name="username" type="text" label="Username" placeholder="Enter username">
  <candy:validate rule="required|minlen:3" message="Username must be at least 3 characters"/>
</candy:field>

<!-- Email input -->
<candy:field name="email" type="email" label="Email" placeholder="your@email.com">
  <candy:validate rule="required|email" message="Please enter a valid email"/>
</candy:field>

<!-- Password input -->
<candy:field name="password" type="password" label="Password">
  <candy:validate rule="required|minlen:8" message="Password must be at least 8 characters"/>
</candy:field>

<!-- Textarea -->
<candy:field name="message" type="textarea" label="Message" placeholder="Your message...">
  <candy:validate rule="required|minlen:10" message="Message too short"/>
</candy:field>

<!-- Checkbox -->
<candy:field name="agree" type="checkbox" label="I agree to terms">
  <candy:validate rule="accepted" message="You must agree to continue"/>
</candy:field>

<!-- Number input -->
<candy:field name="age" type="number" label="Age">
  <candy:validate rule="required|min:18|max:100" message="Age must be between 18 and 100"/>
</candy:field>
```

### Field Attributes

- `name` - Field name (required)
- `type` - Input type (default: text)
- `label` - Field label
- `placeholder` - Placeholder text
- `class` - CSS classes
- `id` - Field ID

## Validation Rules

### `<candy:validate>`

Add validation rules to fields:

```html
<candy:field name="username" type="text">
  <candy:validate rule="required|minlen:3|maxlen:20" message="Username must be 3-20 characters"/>
</candy:field>
```

### Available Rules

- `required` - Field is required
- `email` - Must be valid email
- `url` - Must be valid URL
- `minlen:n` - Minimum length
- `maxlen:n` - Maximum length
- `min:n` - Minimum value (numbers)
- `max:n` - Maximum value (numbers)
- `numeric` - Only numbers
- `alpha` - Only letters
- `alphanumeric` - Letters and numbers only
- `accepted` - Checkbox must be checked

### Multiple Rules

Combine rules with `|`:

```html
<candy:validate rule="required|email|maxlen:100" message="Invalid email"/>
```

## Submit Button

### `<candy:submit>`

```html
<!-- Simple -->
<candy:submit text="Submit"/>

<!-- With loading state -->
<candy:submit text="Send Message" loading="Sending..."/>

<!-- With styling -->
<candy:submit text="Save" loading="Saving..." class="btn btn-primary" id="save-btn"/>
```

## Controller Handler

Handle form submission in your controller:

```javascript
module.exports = {
  submit: Candy => {
    // Access validated form data
    const data = Candy.formData
    
    // data contains all field values
    console.log(data.email, data.message)
    
    // Process the data (save to database, send email, etc.)
    
    // Return success response
    return Candy.return({
      result: {
        success: true,
        message: 'Form submitted successfully!',
        redirect: '/thank-you' // Optional redirect
      }
    })
  }
}
```

### Error Handling

Return validation errors:

```javascript
module.exports = {
  submit: Candy => {
    const data = Candy.formData
    
    // Custom validation
    if (data.email.includes('spam')) {
      return Candy.return({
        result: {success: false},
        errors: {
          email: 'This email is not allowed'
        }
      })
    }
    
    return Candy.return({
      result: {success: true, message: 'Success!'}
    })
  }
}
```

## Complete Example

### View (view/content/contact.html)

```html
<div class="contact-page">
  <h1>Contact Us</h1>
  
  <candy:form action="/contact/submit" method="POST" class="contact-form">
    <candy:field name="name" type="text" label="Your Name" placeholder="Enter your name">
      <candy:validate rule="required|minlen:3" message="Name must be at least 3 characters"/>
    </candy:field>
    
    <candy:field name="email" type="email" label="Email" placeholder="your@email.com">
      <candy:validate rule="required|email" message="Please enter a valid email"/>
    </candy:field>
    
    <candy:field name="subject" type="text" label="Subject" placeholder="What is this about?">
      <candy:validate rule="required|minlen:5" message="Subject must be at least 5 characters"/>
    </candy:field>
    
    <candy:field name="message" type="textarea" label="Message" placeholder="Your message...">
      <candy:validate rule="required|minlen:10" message="Message must be at least 10 characters"/>
    </candy:field>
    
    <candy:submit text="Send Message" loading="Sending..." class="btn btn-primary"/>
  </candy:form>
</div>
```

### Controller (controller/contact.js)

```javascript
module.exports = {
  index: Candy => {
    Candy.View.skeleton('default')
    Candy.View.set({content: 'contact'})
    Candy.View.print()
  },

  submit: Candy => {
    const data = Candy.formData
    
    // Save to database
    // await Candy.Mysql.query('INSERT INTO contacts SET ?', data)
    
    // Send email notification
    // await Candy.Mail().to('admin@example.com').subject('New Contact').send(data.message)
    
    return Candy.return({
      result: {
        success: true,
        message: 'Thank you! We will get back to you soon.',
        redirect: '/'
      }
    })
  }
}
```

### Route (route/www.js)

```javascript
Candy.Route.page('/contact', 'contact')
Candy.Route.post('/contact/submit', 'contact.submit')
```

## Features

- **Automatic CSRF Protection** - Built-in token validation
- **Client-Side Validation** - HTML5 validation with custom messages
- **Server-Side Validation** - Automatic validation before controller execution
- **Session Security** - Form tokens tied to user session, IP, and user agent
- **Loading States** - Automatic button state management
- **Error Display** - Automatic error message rendering
- **Success Messages** - Built-in success message handling
- **Redirect Support** - Optional redirect after successful submission

## Security

Forms automatically include:

- CSRF token validation
- Session verification
- IP address validation
- User agent verification
- Token expiration (30 minutes)

All validation happens before your controller is executed, ensuring only valid, secure data reaches your code.
