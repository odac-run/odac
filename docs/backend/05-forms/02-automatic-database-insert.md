# Automatic Database Insert

Forms can automatically insert data into your database without writing any controller code. This is perfect for simple use cases like waitlists, newsletter signups, contact forms, and feedback collection.

## Basic Usage

```html
<candy:form table="waitlist">
  <candy:field name="email" type="email" label="Email">
    <candy:validate rule="required|email|unique"/>
  </candy:field>
  
  <candy:submit text="Join"/>
</candy:form>
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
  
  <candy:form table="waitlist" redirect="/" success="Thank you for joining!">
    <candy:field name="email" type="email" label="Email" placeholder="your@email.com">
      <candy:validate rule="required|email|unique" message="Please enter a valid email"/>
    </candy:field>
    
    <candy:field name="name" type="text" label="Name" placeholder="Your name">
      <candy:validate rule="required|minlen:2" message="Name is required"/>
    </candy:field>
    
    <candy:set name="created_at" compute="now"/>
    <candy:set name="ip" compute="ip"/>
    <candy:set name="user_agent" compute="user_agent"/>
    
    <candy:submit text="Join Waitlist" loading="Joining..." class="btn btn-primary"/>
  </candy:form>
</div>
```

### 3. Create Controller

**controller/waitlist.js**
```javascript
module.exports = Candy => {
  Candy.View.skeleton('default')
  Candy.View.set({content: 'waitlist'})
  Candy.View.print()
}
```

### 4. Add Route

**route/www.js**
```javascript
Candy.Route.page('/waitlist', 'waitlist')
```

Done! No form submission handler needed.

## Form Attributes

### `table` (required)
Database table name where data will be inserted.

```html
<candy:form table="newsletter_subscribers">
```

### `redirect` (optional)
URL to redirect after successful submission.

```html
<candy:form table="waitlist" redirect="/thank-you">
```

### `success` (optional)
Custom success message to display.

```html
<candy:form table="waitlist" success="Welcome! We'll notify you soon.">
```

## Unique Validation

Use `unique` rule to prevent duplicate entries:

```html
<candy:field name="email" type="email">
  <candy:validate rule="required|email|unique" message="This email is already registered"/>
</candy:field>
```

The system will:
1. Check if the value exists in the table
2. Return error if duplicate found
3. Show the custom error message

## Auto-Set Values

Use `<candy:set>` to automatically populate fields:

```html
<candy:set name="created_at" compute="now"/>
<candy:set name="ip" compute="ip"/>
<candy:set name="user_agent" compute="user_agent"/>
<candy:set name="status" value="pending"/>
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
<candy:set name="status" value="pending"/>
<candy:set name="source" value="website"/>
<candy:set name="plan" value="free"/>
```

### Conditional Set

Only set if field is empty:

```html
<candy:set name="country" value="US" if-empty/>
```

## Use Cases

### Newsletter Signup

```html
<candy:form table="newsletter" success="Thanks for subscribing!">
  <candy:field name="email" type="email">
    <candy:validate rule="required|email|unique"/>
  </candy:field>
  
  <candy:set name="subscribed_at" compute="now"/>
  <candy:set name="status" value="active"/>
  
  <candy:submit text="Subscribe"/>
</candy:form>
```

### Feedback Form

```html
<candy:form table="feedback" redirect="/" success="Thank you for your feedback!">
  <candy:field name="rating" type="number" label="Rating (1-5)">
    <candy:validate rule="required|min:1|max:5"/>
  </candy:field>
  
  <candy:field name="comment" type="textarea" label="Comment">
    <candy:validate rule="required|minlen:10"/>
  </candy:field>
  
  <candy:set name="created_at" compute="now"/>
  <candy:set name="ip" compute="ip"/>
  
  <candy:submit text="Submit Feedback"/>
</candy:form>
```

### Beta Access Request

```html
<candy:form table="beta_requests" success="You're on the list!">
  <candy:field name="email" type="email">
    <candy:validate rule="required|email|unique"/>
  </candy:field>
  
  <candy:field name="company" type="text">
    <candy:validate rule="required"/>
  </candy:field>
  
  <candy:field name="use_case" type="textarea">
    <candy:validate rule="required|minlen:20"/>
  </candy:field>
  
  <candy:set name="requested_at" compute="now"/>
  <candy:set name="status" value="pending"/>
  
  <candy:submit text="Request Access"/>
</candy:form>
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

You can add custom logic by specifying a custom `action` attribute. When you do this, the form data is validated and prepared, but the automatic DB insert is skipped. Instead, your controller receives the validated data via `Candy.formData`:

```javascript
// In your view:
// <candy:form action="/contact/submit" table="contacts">

// In your controller:
Candy.Route.post('/contact/submit', async Candy => {
  // Candy.formData contains validated form data
  // Candy.formConfig contains form configuration
  
  // Perform custom logic (send email, call API, etc.)
  await sendEmail(Candy.formData.email, 'Thank you!')
  
  // Manually insert to database if needed
  await Candy.Mysql.query('INSERT INTO contacts SET ?', Candy.formData)
  
  return Candy.return({
    result: {success: true, message: 'Message sent!'}
  })
})
  const data = Candy.formData
  
  // Send welcome email
  Candy.Mail()
    .to(data.email)
    .subject('Welcome!')
    .send('Thanks for joining!')
  
  // Return custom response
  return Candy.return({
    result: {
      success: true,
      message: 'Check your email!'
    }
  })
})
```

But for most cases, automatic insert is all you need!
