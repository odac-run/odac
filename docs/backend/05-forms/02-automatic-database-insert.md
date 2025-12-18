# Automatic Database Insert

Forms can automatically insert data into your database without writing any controller code. This is perfect for simple use cases like waitlists, newsletter signups, contact forms, and feedback collection.

## Basic Usage

```html
<odac:form table="waitlist">
  <odac:field name="email" type="email" label="Email">
    <odac:validate rule="required|email|unique"/>
  </odac:field>
  
  <odac:submit text="Join"/>
</odac:form>
```

That's it! The form will automatically:
1. Validate the email
2. Check if it's unique in the database
3. Insert the record into `waitlist` table
4. Show success message

## Complete Example

### 1. Create Database Table

```sql
CREATE TABLE `waitlist` (
  `id` INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `email` VARCHAR(255) NOT NULL UNIQUE,
  `name` VARCHAR(255) NOT NULL,
  `created_at` INT UNSIGNED NOT NULL,
  `ip` VARCHAR(45) NULL,
  `user_agent` TEXT NULL,
  INDEX `idx_email` (`email`),
  INDEX `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2. Create View

**view/content/waitlist.html**
```html
<div class="waitlist-page">
  <h1>Join Our Waitlist</h1>
  
  <odac:form table="waitlist" redirect="/" success="Thank you for joining!">
    <odac:field name="email" type="email" label="Email" placeholder="your@email.com">
      <odac:validate rule="required|email|unique" message="Please enter a valid email"/>
    </odac:field>
    
    <odac:field name="name" type="text" label="Name" placeholder="Your name">
      <odac:validate rule="required|minlen:2" message="Name is required"/>
    </odac:field>
    
    <odac:set name="created_at" compute="now"/>
    <odac:set name="ip" compute="ip"/>
    <odac:set name="user_agent" compute="user_agent"/>
    
    <odac:submit text="Join Waitlist" loading="Joining..." class="btn btn-primary"/>
  </odac:form>
</div>
```

### 3. Create Controller

**controller/waitlist.js**
```javascript
module.exports = Odac => {
  Odac.View.skeleton('default')
  Odac.View.set({content: 'waitlist'})
  Odac.View.print()
}
```

### 4. Add Route

**route/www.js**
```javascript
Odac.Route.page('/waitlist', 'waitlist')
```

Done! No form submission handler needed.

## Form Attributes

### `table` (required)
Database table name where data will be inserted.

```html
<odac:form table="newsletter_subscribers">
```

### `redirect` (optional)
URL to redirect after successful submission.

```html
<odac:form table="waitlist" redirect="/thank-you">
```

### `success` (optional)
Custom success message to display.

```html
<odac:form table="waitlist" success="Welcome! We'll notify you soon.">
```

## Unique Validation

Use `unique` rule to prevent duplicate entries:

```html
<odac:field name="email" type="email">
  <odac:validate rule="required|email|unique" message="This email is already registered"/>
</odac:field>
```

The system will:
1. Check if the value exists in the table
2. Return error if duplicate found
3. Show the custom error message

## Auto-Set Values

Use `<odac:set>` to automatically populate fields:

```html
<odac:set name="created_at" compute="now"/>
<odac:set name="ip" compute="ip"/>
<odac:set name="user_agent" compute="user_agent"/>
<odac:set name="status" value="pending"/>
```

### Compute Types

- `now` - Unix timestamp in seconds
- `date` - Current date (YYYY-MM-DD)
- `datetime` - ISO datetime string
- `timestamp` - Timestamp in milliseconds
- `ip` - User's IP address
- `user_agent` - Browser user agent
- `uuid` - Generate UUID v4

### Static Values

```html
<odac:set name="status" value="pending"/>
<odac:set name="source" value="website"/>
<odac:set name="plan" value="free"/>
```

### Conditional Set

Only set if field is empty:

```html
<odac:set name="country" value="US" if-empty/>
```

## Use Cases

### Newsletter Signup

```html
<odac:form table="newsletter" success="Thanks for subscribing!">
  <odac:field name="email" type="email">
    <odac:validate rule="required|email|unique"/>
  </odac:field>
  
  <odac:set name="subscribed_at" compute="now"/>
  <odac:set name="status" value="active"/>
  
  <odac:submit text="Subscribe"/>
</odac:form>
```

### Feedback Form

```html
<odac:form table="feedback" redirect="/" success="Thank you for your feedback!">
  <odac:field name="rating" type="number" label="Rating (1-5)">
    <odac:validate rule="required|min:1|max:5"/>
  </odac:field>
  
  <odac:field name="comment" type="textarea" label="Comment">
    <odac:validate rule="required|minlen:10"/>
  </odac:field>
  
  <odac:set name="created_at" compute="now"/>
  <odac:set name="ip" compute="ip"/>
  
  <odac:submit text="Submit Feedback"/>
</odac:form>
```

### Beta Access Request

```html
<odac:form table="beta_requests" success="You're on the list!">
  <odac:field name="email" type="email">
    <odac:validate rule="required|email|unique"/>
  </odac:field>
  
  <odac:field name="company" type="text">
    <odac:validate rule="required"/>
  </odac:field>
  
  <odac:field name="use_case" type="textarea">
    <odac:validate rule="required|minlen:20"/>
  </odac:field>
  
  <odac:set name="requested_at" compute="now"/>
  <odac:set name="status" value="pending"/>
  
  <odac:submit text="Request Access"/>
</odac:form>
```

## Error Handling

The system automatically handles:

- **Validation errors** - Shows field-specific error messages
- **Duplicate entries** - Shows unique constraint errors
- **Database errors** - Shows generic error message
- **Missing database** - Shows configuration error

All errors are displayed inline next to the relevant field.

## Security

Automatic DB insert includes all security features:

- CSRF token validation
- Session verification
- IP address validation
- User agent verification
- SQL injection prevention (parameterized queries)
- Token expiration (30 minutes)

## When to Use

**Use automatic DB insert when:**
- Simple data collection (waitlist, newsletter, feedback)
- No complex business logic needed
- Direct database insert is sufficient
- You want rapid development

**Use custom controller when:**
- Need to send emails
- Complex validation logic
- Multiple database operations
- External API calls
- Custom response handling

## Combining with Custom Logic

You can add custom logic by specifying a custom `action` attribute. When you do this, the form data is validated and prepared, but the automatic DB insert is skipped. Instead, your controller receives the validated data via `Odac.formData`:

```javascript
// In your view:
// <odac:form action="/contact/submit" table="contacts">

// In your controller:
Odac.Route.post('/contact/submit', async Odac => {
  // Odac.formData contains validated form data
  // Odac.formConfig contains form configuration
  
  // Perform custom logic (send email, call API, etc.)
  await sendEmail(Odac.formData.email, 'Thank you!')
  
  // Manually insert to database if needed
  await Odac.Mysql.query('INSERT INTO contacts SET ?', Odac.formData)
  
  return Odac.return({
    result: {success: true, message: 'Message sent!'}
  })
})
  const data = Odac.formData
  
  // Send welcome email
  Odac.Mail()
    .to(data.email)
    .subject('Welcome!')
    .send('Thanks for joining!')
  
  // Return custom response
  return Odac.return({
    result: {
      success: true,
      message: 'Check your email!'
    }
  })
})
```

But for most cases, automatic insert is all you need!
