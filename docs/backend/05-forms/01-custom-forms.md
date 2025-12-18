# Custom Forms

Odac provides an automatic form system with built-in validation, CSRF protection, and seamless client-side integration. The `<odac:form>` tag allows you to create forms with minimal code while maintaining full control.

## Basic Usage

```html
<odac:form action="/contact/submit" method="POST">
  <odac:field name="email" type="email" label="Email">
    <odac:validate rule="required|email" message="Valid email required"/>
  </odac:field>
  
  <odac:submit text="Send" loading="Sending..."/>
</odac:form>
```

## Form Attributes

### `<odac:form>`

- `action` - Form submission URL (optional if using `table`)
- `method` - HTTP method (default: POST)
- `table` - Database table name for automatic insert (optional)
- `redirect` - Redirect URL after success (optional)
- `success` - Success message (optional)
- `class` - Additional CSS classes
- `id` - Form ID attribute

```html
<!-- With custom controller -->
<odac:form action="/api/save" method="POST" class="my-form" id="contact-form">
  <!-- fields here -->
</odac:form>

<!-- With automatic DB insert -->
<odac:form table="waitlist" redirect="/" success="Thank you for joining!">
  <!-- fields here -->
</odac:form>
```

## Field Types

### `<odac:field>`

Supports all standard HTML input types:

```html
<!-- Text input with multiple validations -->
<odac:field name="username" type="text" label="Username" placeholder="Enter username">
  <odac:validate rule="required" message="Username is required"/>
  <odac:validate rule="minlen:3" message="Username must be at least 3 characters"/>
  <odac:validate rule="maxlen:20" message="Username cannot exceed 20 characters"/>
  <odac:validate rule="alphanumeric" message="Username can only contain letters and numbers"/>
</odac:field>

<!-- Email input -->
<odac:field name="email" type="email" label="Email Address" placeholder="your@email.com">
  <odac:validate rule="required" message="Email address is required"/>
  <odac:validate rule="email" message="Please enter a valid email address"/>
  <odac:validate rule="maxlen:100" message="Email is too long"/>
</odac:field>

<!-- Password input with strong validation -->
<odac:field name="password" type="password" label="Password">
  <odac:validate rule="required" message="Password is required"/>
  <odac:validate rule="minlen:8" message="Password must be at least 8 characters long"/>
  <odac:validate rule="maxlen:50" message="Password is too long"/>
</odac:field>

<!-- Textarea with character limits -->
<odac:field name="message" type="textarea" label="Your Message" placeholder="Tell us what you think...">
  <odac:validate rule="required" message="Please enter your message"/>
  <odac:validate rule="minlen:10" message="Message must be at least 10 characters"/>
  <odac:validate rule="maxlen:500" message="Message cannot exceed 500 characters"/>
</odac:field>

<!-- Checkbox for terms acceptance -->
<odac:field name="agree" type="checkbox" label="I agree to the Terms of Service and Privacy Policy">
  <odac:validate rule="accepted" message="You must accept the terms to continue"/>
</odac:field>

<!-- Number input with range -->
<odac:field name="age" type="number" label="Your Age">
  <odac:validate rule="required" message="Age is required"/>
  <odac:validate rule="min:18" message="You must be at least 18 years old"/>
  <odac:validate rule="max:120" message="Please enter a valid age"/>
</odac:field>

<!-- Phone number -->
<odac:field name="phone" type="text" label="Phone Number" placeholder="+1 (555) 123-4567">
  <odac:validate rule="required" message="Phone number is required"/>
  <odac:validate rule="minlen:10" message="Phone number must be at least 10 digits"/>
</odac:field>

<!-- URL input -->
<odac:field name="website" type="url" label="Website" placeholder="https://example.com">
  <odac:validate rule="url" message="Please enter a valid URL"/>
</odac:field>

<!-- Name with alpha validation -->
<odac:field name="full_name" type="text" label="Full Name" placeholder="John Doe">
  <odac:validate rule="required" message="Full name is required"/>
  <odac:validate rule="minlen:2" message="Name must be at least 2 characters"/>
  <odac:validate rule="maxlen:50" message="Name is too long"/>
</odac:field>
```

### Field Attributes

- `name` - Field name (required)
- `type` - Input type (default: text)
- `label` - Field label
- `placeholder` - Placeholder text
- `class` - CSS classes
- `id` - Field ID

## Validation Rules

### `<odac:validate>`

Add validation rules to fields:

```html
<odac:field name="username" type="text">
  <odac:validate rule="required|minlen:3|maxlen:20" message="Username must be 3-20 characters"/>
</odac:field>
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
<odac:validate rule="required|email|maxlen:100" message="Invalid email"/>
```

### Unique Validation

For automatic DB insert, use `unique` to check if value already exists:

```html
<odac:validate rule="required|email|unique" message="This email is already registered"/>
```

## Auto-Set Values

### `<odac:set>`

Automatically set field values without user input:

```html
<odac:set name="created_at" compute="now"/>
<odac:set name="ip" compute="ip"/>
<odac:set name="user_agent" compute="user_agent"/>
<odac:set name="status" value="pending"/>
```

### Available Compute Types

- `now` - Current Unix timestamp (seconds)
- `date` - Current date (YYYY-MM-DD)
- `datetime` - Current ISO datetime
- `timestamp` - Current timestamp (milliseconds)
- `ip` - User's IP address
- `user_agent` - User's browser user agent
- `uuid` - Generate UUID v4

### Set Attributes

- `name` - Field name (required)
- `value` - Static value
- `compute` - Computed value type
- `callback` - Custom function name
- `if-empty` - Only set if field is empty

## Submit Button

### `<odac:submit>`

```html
<!-- Simple -->
<odac:submit text="Submit"/>

<!-- With loading state -->
<odac:submit text="Send Message" loading="Sending..."/>

<!-- With styling -->
<odac:submit text="Save" loading="Saving..." class="btn btn-primary" id="save-btn"/>
```

## Controller Handler

Handle form submission in your controller:

```javascript
module.exports = {
  submit: Odac => {
    // Access validated form data
    const data = Odac.formData
    
    // data contains all field values
    console.log(data.email, data.message)
    
    // Process the data (save to database, send email, etc.)
    
    // Return success response
    return Odac.return({
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
  submit: Odac => {
    const data = Odac.formData
    
    // Custom validation
    if (data.email.includes('spam')) {
      return Odac.return({
        result: {success: false},
        errors: {
          email: 'This email is not allowed'
        }
      })
    }
    
    return Odac.return({
      result: {success: true, message: 'Success!'}
    })
  }
}
```

## Automatic Database Insert

Forms can automatically insert data into database without writing a controller:

### View (view/content/waitlist.html)

```html
<odac:form table="waitlist" redirect="/" success="Thank you for joining!">
  <odac:field name="email" type="email" label="Email">
    <odac:validate rule="required|email|unique" message="Valid email required"/>
  </odac:field>
  
  <odac:field name="name" type="text" label="Name">
    <odac:validate rule="required|minlen:2" message="Name required"/>
  </odac:field>
  
  <odac:set name="created_at" compute="now"/>
  <odac:set name="ip" compute="ip"/>
  
  <odac:submit text="Join Waitlist" loading="Joining..."/>
</odac:form>
```

### Database Table

```sql
CREATE TABLE `waitlist` (
  `id` INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `email` VARCHAR(255) NOT NULL UNIQUE,
  `name` VARCHAR(255) NOT NULL,
  `created_at` INT UNSIGNED NOT NULL,
  `ip` VARCHAR(45) NULL,
  INDEX `idx_email` (`email`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### Route (route/www.js)

```javascript
Odac.Route.page('/waitlist', 'waitlist')
```

That's it! No controller needed. The form will:
- Validate all fields
- Check email uniqueness
- Insert data into `waitlist` table
- Show success message
- Redirect to home page

## Complete Example with Custom Controller

### View (view/content/contact.html)

```html
<div class="contact-page">
  <h1>Contact Us</h1>
  
  <odac:form action="/contact/submit" method="POST" class="contact-form">
    <odac:field name="name" type="text" label="Your Name" placeholder="Enter your name">
      <odac:validate rule="required|minlen:3" message="Name must be at least 3 characters"/>
    </odac:field>
    
    <odac:field name="email" type="email" label="Email" placeholder="your@email.com">
      <odac:validate rule="required|email" message="Please enter a valid email"/>
    </odac:field>
    
    <odac:field name="subject" type="text" label="Subject" placeholder="What is this about?">
      <odac:validate rule="required|minlen:5" message="Subject must be at least 5 characters"/>
    </odac:field>
    
    <odac:field name="message" type="textarea" label="Message" placeholder="Your message...">
      <odac:validate rule="required|minlen:10" message="Message must be at least 10 characters"/>
    </odac:field>
    
    <odac:submit text="Send Message" loading="Sending..." class="btn btn-primary"/>
  </odac:form>
</div>
```

### Controller (controller/contact.js)

```javascript
module.exports = {
  index: Odac => {
    Odac.View.skeleton('default')
    Odac.View.set({content: 'contact'})
    Odac.View.print()
  },

  submit: Odac => {
    const data = Odac.formData
    
    // Save to database
    // await Odac.Mysql.query('INSERT INTO contacts SET ?', data)
    
    // Send email notification
    // await Odac.Mail().to('admin@example.com').subject('New Contact').send(data.message)
    
    return Odac.return({
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
Odac.Route.page('/contact', 'contact')
Odac.Route.post('/contact/submit', 'contact.submit')
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
