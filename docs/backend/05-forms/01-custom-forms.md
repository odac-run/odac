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

- `action` - Form submission URL (optional if using `table`)
- `method` - HTTP method (default: POST)
- `table` - Database table name for automatic insert (optional)
- `redirect` - Redirect URL after success (optional)
- `success` - Success message (optional)
- `class` - Additional CSS classes
- `id` - Form ID attribute

```html
<!-- With custom controller -->
<candy:form action="/api/save" method="POST" class="my-form" id="contact-form">
  <!-- fields here -->
</candy:form>

<!-- With automatic DB insert -->
<candy:form table="waitlist" redirect="/" success="Thank you for joining!">
  <!-- fields here -->
</candy:form>
```

## Field Types

### `<candy:field>`

Supports all standard HTML input types:

```html
<!-- Text input with multiple validations -->
<candy:field name="username" type="text" label="Username" placeholder="Enter username">
  <candy:validate rule="required" message="Username is required"/>
  <candy:validate rule="minlen:3" message="Username must be at least 3 characters"/>
  <candy:validate rule="maxlen:20" message="Username cannot exceed 20 characters"/>
  <candy:validate rule="alphanumeric" message="Username can only contain letters and numbers"/>
</candy:field>

<!-- Email input -->
<candy:field name="email" type="email" label="Email Address" placeholder="your@email.com">
  <candy:validate rule="required" message="Email address is required"/>
  <candy:validate rule="email" message="Please enter a valid email address"/>
  <candy:validate rule="maxlen:100" message="Email is too long"/>
</candy:field>

<!-- Password input with strong validation -->
<candy:field name="password" type="password" label="Password">
  <candy:validate rule="required" message="Password is required"/>
  <candy:validate rule="minlen:8" message="Password must be at least 8 characters long"/>
  <candy:validate rule="maxlen:50" message="Password is too long"/>
</candy:field>

<!-- Textarea with character limits -->
<candy:field name="message" type="textarea" label="Your Message" placeholder="Tell us what you think...">
  <candy:validate rule="required" message="Please enter your message"/>
  <candy:validate rule="minlen:10" message="Message must be at least 10 characters"/>
  <candy:validate rule="maxlen:500" message="Message cannot exceed 500 characters"/>
</candy:field>

<!-- Checkbox for terms acceptance -->
<candy:field name="agree" type="checkbox" label="I agree to the Terms of Service and Privacy Policy">
  <candy:validate rule="accepted" message="You must accept the terms to continue"/>
</candy:field>

<!-- Number input with range -->
<candy:field name="age" type="number" label="Your Age">
  <candy:validate rule="required" message="Age is required"/>
  <candy:validate rule="min:18" message="You must be at least 18 years old"/>
  <candy:validate rule="max:120" message="Please enter a valid age"/>
</candy:field>

<!-- Phone number -->
<candy:field name="phone" type="text" label="Phone Number" placeholder="+1 (555) 123-4567">
  <candy:validate rule="required" message="Phone number is required"/>
  <candy:validate rule="minlen:10" message="Phone number must be at least 10 digits"/>
</candy:field>

<!-- URL input -->
<candy:field name="website" type="url" label="Website" placeholder="https://example.com">
  <candy:validate rule="url" message="Please enter a valid URL"/>
</candy:field>

<!-- Name with alpha validation -->
<candy:field name="full_name" type="text" label="Full Name" placeholder="John Doe">
  <candy:validate rule="required" message="Full name is required"/>
  <candy:validate rule="minlen:2" message="Name must be at least 2 characters"/>
  <candy:validate rule="maxlen:50" message="Name is too long"/>
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

### Unique Validation

For automatic DB insert, use `unique` to check if value already exists:

```html
<candy:validate rule="required|email|unique" message="This email is already registered"/>
```

## Auto-Set Values

### `<candy:set>`

Automatically set field values without user input:

```html
<candy:set name="created_at" compute="now"/>
<candy:set name="ip" compute="ip"/>
<candy:set name="user_agent" compute="user_agent"/>
<candy:set name="status" value="pending"/>
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

## Automatic Database Insert

Forms can automatically insert data into database without writing a controller:

### View (view/content/waitlist.html)

```html
<candy:form table="waitlist" redirect="/" success="Thank you for joining!">
  <candy:field name="email" type="email" label="Email">
    <candy:validate rule="required|email|unique" message="Valid email required"/>
  </candy:field>
  
  <candy:field name="name" type="text" label="Name">
    <candy:validate rule="required|minlen:2" message="Name required"/>
  </candy:field>
  
  <candy:set name="created_at" compute="now"/>
  <candy:set name="ip" compute="ip"/>
  
  <candy:submit text="Join Waitlist" loading="Joining..."/>
</candy:form>
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
Candy.Route.page('/waitlist', 'waitlist')
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
