# Candy Register Forms

The `<candy:register>` component provides a zero-configuration way to create secure registration forms. Simply write HTML, and Odac handles validation, security, database operations, and auto-login automatically.

## Quick Start

### 1. Configure Database (config.json)

```json
{
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "",
    "database": "your_database"
  }
}
```

That's all you need! The `auth` configuration is optional.

### 2. Create Your Form (view/content/register.html)

```html
<candy:register redirect="/dashboard">
  <candy:field name="email" type="email" placeholder="Email" unique>
    <candy:validate rule="required|email" message="Please enter a valid email"/>
  </candy:field>
  
  <candy:field name="username" type="text" placeholder="Username" unique>
    <candy:validate rule="required|minlen:4" message="Username must be at least 4 characters"/>
  </candy:field>
  
  <candy:field name="password" type="password" placeholder="Password">
    <candy:validate rule="required|minlen:8" message="Password must be at least 8 characters"/>
  </candy:field>
  
  <candy:submit>Create Account</candy:submit>
</candy:register>
```

That's it! No JavaScript, no controller code, no SQL needed. The form automatically:
- Creates the database table (if it doesn't exist)
- Validates input (client-side and server-side)
- Checks for unique email/username
- Hashes passwords with bcrypt
- Creates the user account
- Logs in the user
- Redirects to dashboard

### 3. Optional: Customize Auth Configuration

If you want to customize table names or primary key:

```json
{
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "",
    "database": "your_database"
  },
  "auth": {
    "table": "users",           // Optional: User table name (default: "users")
    "key": "id",                // Optional: Primary key (default: "id")
    "token": "candy_auth"       // Optional: Token table (default: "candy_auth")
  }
}
```

**If you don't specify `auth` config:**
- Table name defaults to `users`
- Primary key defaults to `id`
- Token table defaults to `candy_auth`
- Table is created automatically based on your form fields

## Form Attributes

### `<candy:register>`

Main form container with configuration options:

```html
<candy:register 
  redirect="/dashboard"    <!-- Redirect URL after successful registration -->
  autologin="true">        <!-- Auto-login after registration (default: true) -->
  <!-- fields here -->
</candy:register>
```

**Attributes:**
- `redirect` - URL to redirect after successful registration
- `autologin` - Whether to automatically log in the user (default: `true`)

## Field Types

### `<candy:field>`

Defines an input field with validation rules:

```html
<candy:field 
  name="email"              <!-- Field name (required) -->
  type="email"              <!-- Input type (default: text) -->
  placeholder="Email"       <!-- Placeholder text -->
  label="Email Address"     <!-- Label text (optional) -->
  unique>                   <!-- Check uniqueness in database -->
  
  <candy:validate rule="required|email" message="Valid email required"/>
</candy:field>
```

**Attributes:**
- `name` - Field name (required)
- `type` - Input type: text, email, password, number, checkbox, textarea
- `placeholder` - Placeholder text
- `label` - Label text (for checkbox or explicit labels)
- `unique` - Check if value already exists in database
- `skip` - Validate field but don't save to database (useful for password confirmation, terms acceptance)

**Supported Input Types:**
- `text` - Text input
- `email` - Email input (with HTML5 validation)
- `password` - Password input
- `number` - Number input
- `checkbox` - Checkbox input
- `textarea` - Textarea

### Skip Attribute

Use the `skip` attribute for fields that should be validated but not saved to the database:

```html
<!-- Password confirmation - validate but don't save -->
<candy:field name="confirm_password" type="password" placeholder="Confirm Password" skip>
  <candy:validate rule="required|same:password" message="Passwords must match"/>
</candy:field>

<!-- Terms acceptance - validate but don't save -->
<candy:field name="terms" type="checkbox" label="I accept the terms" skip>
  <candy:validate rule="accepted" message="You must accept the terms"/>
</candy:field>

<!-- Captcha verification - validate but don't save -->
<candy:field name="captcha" type="text" placeholder="Enter captcha" skip>
  <candy:validate rule="required" message="Please complete the captcha"/>
</candy:field>
```

**Common Use Cases:**
- Password confirmation fields
- Terms and conditions checkboxes
- Privacy policy acceptance
- Captcha verification
- Temporary validation fields

## Validation Rules

### `<candy:validate>`

Defines validation rules for a field:

```html
<candy:validate 
  rule="required|minlen:4|alphanumeric"
  message="Username must be 4+ alphanumeric characters"/>
```

**Attributes:**
- `rule` - Validation rules (pipe-separated)
- `message` - Error message to display

### Available Rules

**Basic Rules:**
- `required` - Field is required
- `email` - Valid email format
- `url` - Valid URL format
- `numeric` - Only numbers
- `alpha` - Only letters
- `alphanumeric` - Letters and numbers only
- `alphaspace` - Letters and spaces only
- `alphanumericspace` - Letters, numbers, and spaces
- `accepted` - Must be accepted (for checkboxes)

**Length Rules:**
- `minlen:X` - Minimum length
- `maxlen:X` - Maximum length
- `len:X` - Exact length

**Number Rules:**
- `min:X` - Minimum value
- `max:X` - Maximum value

**Comparison Rules:**
- `same:field` - Must match another field
- `different:field` - Must be different from another field
- `equal:value` - Must equal specific value
- `not:value` - Must not equal specific value

**Pattern Rules:**
- `regex:pattern` - Custom regex pattern
- `domain` - Valid domain name
- `ip` - Valid IP address
- `mac` - Valid MAC address

**Special Rules:**
- `unique` - Check uniqueness in database (use with `unique` attribute on field)
- `xss` - Check for XSS attempts

### Multiple Validation Rules

You can add multiple `<candy:validate>` tags for different error messages:

```html
<candy:field name="username" type="text" unique>
  <candy:validate rule="required" message="Username is required"/>
  <candy:validate rule="minlen:4" message="Username must be at least {min} characters"/>
  <candy:validate rule="maxlen:20" message="Username cannot exceed {max} characters"/>
  <candy:validate rule="alphanumeric" message="Username can only contain letters and numbers"/>
  <candy:validate rule="unique" message="Username '{value}' is already taken"/>
</candy:field>
```

Or combine rules in a single tag:

```html
<candy:field name="username" type="text" unique>
  <candy:validate 
    rule="required|minlen:4|maxlen:20|alphanumeric" 
    message="Username must be 4-20 alphanumeric characters"/>
  <candy:validate rule="unique" message="Username '{value}' is already taken"/>
</candy:field>
```

## Message Placeholders

Use placeholders in error messages for dynamic values:

```html
<candy:field name="username" type="text">
  <candy:validate 
    rule="minlen:4" 
    message="Username '{value}' is too short. Minimum {min} characters required"/>
</candy:field>

<candy:field name="age" type="number">
  <candy:validate 
    rule="min:18|max:120" 
    message="Age must be between {min} and {max} years"/>
</candy:field>
```

**Available Placeholders:**
- `{value}` - User's input value
- `{field}` - Field name
- `{label}` - Field label or placeholder
- `{min}` - Minimum value (for minlen, min rules)
- `{max}` - Maximum value (for maxlen, max rules)
- `{len}` - Required length (for len rule)
- `{other}` - Other field name (for same, different rules)

## Backend-Only Values

### `<candy:set>`

Set values that are processed only on the backend (not visible in HTML):

```html
<candy:register redirect="/dashboard">
  <!-- User input fields -->
  <candy:field name="email" type="email" unique>
    <candy:validate rule="required|email"/>
  </candy:field>
  
  <!-- Backend-only values -->
  <candy:set name="role" value="user"/>
  <candy:set name="status" value="pending"/>
  <candy:set name="registered_at" compute="now"/>
  <candy:set name="ip_address" compute="ip"/>
  
  <candy:submit>Register</candy:submit>
</candy:register>
```

**Attributes:**
- `name` - Field name (required)
- `value` - Static value
- `compute` - Computed value (see below)
- `callback` - Custom callback function
- `if-empty` - Only set if user didn't provide a value

### Computed Values

Use `compute` attribute for dynamic values:

```html
<candy:set name="registered_at" compute="now"/>        <!-- Unix timestamp -->
<candy:set name="date" compute="date"/>                <!-- 2025-01-20 -->
<candy:set name="datetime" compute="datetime"/>        <!-- ISO 8601 -->
<candy:set name="timestamp" compute="timestamp"/>      <!-- Milliseconds -->
<candy:set name="ip_address" compute="ip"/>            <!-- User's IP -->
<candy:set name="user_agent" compute="user_agent"/>    <!-- Browser info -->
<candy:set name="uuid" compute="uuid"/>                <!-- UUID v4 -->
```

### Conditional Values

Use `if-empty` to set a default only if the user didn't provide a value:

```html
<candy:field name="country" type="text" placeholder="Country (optional)">
  <!-- User can optionally fill this -->
</candy:field>

<candy:set name="country" value="TR" if-empty/>
<!-- If user leaves it empty, set to "TR" -->
```

## Submit Button

### `<candy:submit>`

Defines the submit button:

```html
<candy:submit 
  text="Create Account"           <!-- Button text -->
  loading="Creating account...">  <!-- Loading state text -->
</candy:submit>
```

Or use content as button text:

```html
<candy:submit>Create Account</candy:submit>
```

## Complete Example

```html
<candy:register redirect="/dashboard" autologin="true">
  
  <!-- Email Field -->
  <candy:field name="email" type="email" placeholder="Email Address" unique>
    <candy:validate rule="required" message="Email is required"/>
    <candy:validate rule="email" message="Please enter a valid email address"/>
    <candy:validate rule="unique" message="The email '{value}' is already registered"/>
  </candy:field>
  
  <!-- Username Field -->
  <candy:field name="username" type="text" placeholder="Username" unique>
    <candy:validate rule="required" message="Username is required"/>
    <candy:validate rule="minlen:4" message="Username must be at least {min} characters"/>
    <candy:validate rule="maxlen:20" message="Username cannot exceed {max} characters"/>
    <candy:validate rule="alphanumeric" message="Only letters and numbers allowed"/>
    <candy:validate rule="unique" message="Username '{value}' is already taken"/>
  </candy:field>
  
  <!-- Password Field -->
  <candy:field name="password" type="password" placeholder="Password">
    <candy:validate rule="required" message="Password is required"/>
    <candy:validate rule="minlen:8" message="Password must be at least {min} characters"/>
  </candy:field>
  
  <!-- Password Confirmation -->
  <candy:field name="password_confirm" type="password" placeholder="Confirm Password" skip>
    <candy:validate rule="required" message="Please confirm your password"/>
    <candy:validate rule="same:password" message="Passwords do not match"/>
  </candy:field>
  
  <!-- Full Name -->
  <candy:field name="name" type="text" placeholder="Full Name">
    <candy:validate rule="required" message="Name is required"/>
    <candy:validate rule="alphaspace" message="Name can only contain letters and spaces"/>
    <candy:validate rule="minlen:3" message="Name must be at least {min} characters"/>
  </candy:field>
  
  <!-- Age -->
  <candy:field name="age" type="number" placeholder="Age">
    <candy:validate rule="required" message="Age is required"/>
    <candy:validate rule="min:18" message="You must be at least {min} years old"/>
    <candy:validate rule="max:120" message="Please enter a valid age"/>
  </candy:field>
  
  <!-- Terms Checkbox -->
  <candy:field name="terms" type="checkbox" label="I agree to the terms and conditions" skip>
    <candy:validate rule="accepted" message="You must accept the terms to continue"/>
  </candy:field>
  
  <!-- Backend-only values -->
  <candy:set name="role" value="user"/>
  <candy:set name="status" value="active"/>
  <candy:set name="registered_at" compute="now"/>
  <candy:set name="ip_address" compute="ip"/>
  <candy:set name="user_agent" compute="user_agent"/>
  
  <!-- Submit Button -->
  <candy:submit text="Create Account" loading="Creating your account..."/>
  
</candy:register>
```

## Security Features

### Automatic Security

Odac automatically handles:

1. **CSRF Protection** - Form tokens prevent cross-site attacks
2. **Password Hashing** - Passwords are hashed with bcrypt
3. **SQL Injection Prevention** - All queries are parameterized
4. **XSS Protection** - Input is sanitized
5. **Field Whitelisting** - Only defined fields are processed
6. **Token Expiration** - Form tokens expire after 30 minutes

### Token System

Each form gets a unique token when rendered:
- Token is stored server-side with field whitelist
- Token expires after 30 minutes
- Only fields defined in the view are accepted
- Backend-only values (`<candy:set>`) are never exposed to HTML

### Unique Field Checking

Fields marked with `unique` attribute are checked against the database:

```html
<candy:field name="email" type="email" unique>
  <candy:validate rule="unique" message="Email already exists"/>
</candy:field>
```

The system automatically queries the auth table to check for duplicates.

## HTML5 Validation

Odac automatically adds HTML5 validation attributes for better UX:

```html
<!-- This field -->
<candy:field name="username" type="text">
  <candy:validate rule="required|minlen:4|maxlen:20|alphanumeric"/>
</candy:field>

<!-- Generates this HTML -->
<input 
  type="text" 
  name="username" 
  required 
  minlength="4" 
  maxlength="20" 
  pattern="[a-zA-Z0-9]+">
```

**Automatic HTML5 Attributes:**
- `required` - For required fields
- `minlength` / `maxlength` - For length validation
- `min` / `max` - For number validation
- `pattern` - For alphanumeric, numeric, alpha rules
- `type="email"` - Native email validation
- `type="url"` - Native URL validation

This provides instant feedback to users before form submission.

## Configuration Reference

### Required Configuration

Only MySQL configuration is required:

```json
{
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "",
    "database": "your_database"
  }
}
```

### Optional Auth Configuration

Customize table names and primary key if needed:

```json
{
  "auth": {
    "table": "users",           // User table name (default: "users")
    "key": "id",                // Primary key column (default: "id")
    "token": "candy_auth"       // Session token table (default: "candy_auth")
  }
}
```

**Default Values:**
- If `auth` is not specified, defaults are used
- Table: `users`
- Primary key: `id`
- Token table: `candy_auth`
- All tables are created automatically if they don't exist

### Database Schema

**Auto-Creation (Recommended):**

The users table is created automatically on first registration! Odac analyzes your form fields and creates the appropriate table structure:

- Fields with `unique` attribute → `VARCHAR(255) NOT NULL UNIQUE`
- Password field → `VARCHAR(255) NOT NULL` (for bcrypt hashes)
- Number fields → `INT` or `BIGINT`
- Text fields → `VARCHAR(255)` or `TEXT` (if > 255 chars)
- Boolean fields → `TINYINT(1)`
- Timestamps → `created_at` and `updated_at` added automatically

**Manual Creation (Optional):**

If you prefer to create the table manually:

```sql
CREATE TABLE `users` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `email` VARCHAR(255) NOT NULL UNIQUE,
  `username` VARCHAR(255) NOT NULL UNIQUE,
  `password` VARCHAR(255) NOT NULL,
  `name` VARCHAR(255) NULL,
  `role` VARCHAR(50) DEFAULT 'user',
  `status` VARCHAR(50) DEFAULT 'active',
  `registered_at` INT NULL,
  `ip_address` VARCHAR(45) NULL,
  `user_agent` TEXT NULL,
  `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

**Important:**
- Password field must be `VARCHAR(255)` to store bcrypt hashes (60 chars + future-proofing)
- Fields marked with `unique` attribute should have `UNIQUE` constraint
- Auto-creation uses `utf8mb4_unicode_ci` collation for full Unicode support

### Token Table

The token table is created automatically on first login, but you can create it manually:

```sql
CREATE TABLE `candy_auth` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `user` INT NOT NULL,
  `token_x` VARCHAR(255) NOT NULL,
  `token_y` VARCHAR(255) NOT NULL,
  `browser` VARCHAR(255) NOT NULL,
  `ip` VARCHAR(255) NOT NULL,
  `date` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `active` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

## Error Handling

### Display Errors

Error messages are automatically displayed in `<span>` elements with `candy-form-error` attribute:

```html
<!-- Errors appear here automatically -->
<span class="candy-form-error" candy-form-error="email" style="display:none;"></span>
```

### Success Messages

Success messages appear in elements with `candy-form-success` class:

```html
<!-- Success message appears here -->
<span class="candy-form-success" style="display:none;"></span>
```

### Custom Styling

Style error and success messages with CSS:

```css
.candy-form-error {
  color: red;
  font-size: 0.875rem;
  margin-top: 0.25rem;
}

.candy-form-success {
  color: green;
  font-size: 1rem;
  padding: 1rem;
  background: #d4edda;
  border: 1px solid #c3e6cb;
  border-radius: 0.25rem;
}

.candy-field {
  margin-bottom: 1rem;
}

input._candy_error {
  border-color: red;
}
```

## Advanced Usage

### Custom Callbacks

You can define custom callbacks for computed values:

```javascript
// In your page controller or global script
Candy.fn.generateReferralCode = async (Candy) => {
  return Math.random().toString(36).substring(2, 10).toUpperCase()
}
```

```html
<candy:set name="referral_code" callback="generateReferralCode"/>
```

### Conditional Registration

Disable auto-login and handle redirect manually:

```html
<candy:register autologin="false">
  <!-- fields -->
</candy:register>
```

Then handle the response in JavaScript if needed (though not required for basic usage).

## Best Practices

1. **Always validate on both client and server** - Odac does this automatically
2. **Use unique attribute for email/username** - Prevents duplicate accounts
3. **Set minimum password length** - Use `minlen:8` or higher
4. **Use password confirmation** - Add a `password_confirm` field with `same:password` rule
5. **Add terms checkbox** - Use `accepted` rule for legal compliance
6. **Use backend-only values** - Store metadata with `<candy:set>`
7. **Provide clear error messages** - Use placeholders for dynamic values
8. **Test form expiration** - Forms expire after 30 minutes

## Troubleshooting

### Form Not Submitting

- Check that `config.json` has auth configuration
- Verify database table exists
- Check browser console for JavaScript errors

### Validation Not Working

- Ensure validation rules are spelled correctly
- Check that field names match between `<candy:field>` and validation
- Verify HTML5 validation isn't blocking submission

### Unique Check Failing

- Verify `unique` attribute is on the field
- Check that auth table name in config matches your database
- Ensure the field exists in your database table

### Token Expired

- Forms expire after 30 minutes
- User needs to refresh the page to get a new token
- Consider adding a message about session expiration
