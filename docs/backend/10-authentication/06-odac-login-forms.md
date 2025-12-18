# Odac Login Forms

The `<odac:login>` component provides a zero-configuration way to create secure login forms. Simply write HTML, and Odac handles validation, security, authentication, and session management automatically.

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

### 2. Create Your Form (view/content/login.html)

```html
<odac:login redirect="/dashboard">
  <odac:field name="email" type="email" placeholder="Email">
    <odac:validate rule="required|email" message="Please enter a valid email"/>
  </odac:field>
  
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required" message="Password is required"/>
  </odac:field>
  
  <odac:submit>Login</odac:submit>
</odac:login>
```

That's it! No JavaScript, no controller code needed. The form automatically:
- Validates input (client-side and server-side)
- Authenticates the user
- Creates a secure session
- Redirects to the specified page

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

## Form Attributes

### `<odac:login>`

Main form container with configuration options:

```html
<odac:login 
  redirect="/dashboard">    <!-- Redirect URL after successful login -->
  <!-- fields here -->
</odac:login>
```

**Attributes:**
- `redirect` - URL to redirect after successful login (required)

## Field Types

### `<odac:field>`

Defines an input field with validation rules:

```html
<odac:field 
  name="email"              <!-- Field name (required) -->
  type="email"              <!-- Input type (default: text) -->
  placeholder="Email"       <!-- Placeholder text -->
  label="Email Address"     <!-- Label text (optional) -->
  class="form-input"        <!-- CSS class (optional) -->
  id="email-field">         <!-- HTML ID (optional) -->
  
  <odac:validate rule="required|email" message="Valid email required"/>
</odac:field>
```

**Attributes:**
- `name` - Field name (required)
- `type` - Input type: text, email, password, number, checkbox
- `placeholder` - Placeholder text
- `label` - Label text
- `class` - CSS class for styling
- `id` - HTML ID attribute

**Supported Input Types:**
- `text` - Text input
- `email` - Email input (with HTML5 validation)
- `password` - Password input
- `number` - Number input
- `checkbox` - Checkbox input (for "remember me" functionality)

## Validation Rules

### `<odac:validate>`

Defines validation rules for a field:

```html
<odac:validate 
  rule="required|minlen:4"
  message="This field is required and must be at least 4 characters"/>
```

**Attributes:**
- `rule` - Validation rules (pipe-separated)
- `message` - Error message to display

### Available Rules

**Basic Rules:**
- `required` - Field is required
- `email` - Valid email format
- `numeric` - Only numbers
- `alpha` - Only letters
- `alphanumeric` - Letters and numbers only

**Length Rules:**
- `minlen:X` - Minimum length
- `maxlen:X` - Maximum length
- `len:X` - Exact length

**Number Rules:**
- `min:X` - Minimum value
- `max:X` - Maximum value

**Pattern Rules:**
- `regex:pattern` - Custom regex pattern

### Multiple Validation Rules

You can add multiple `<odac:validate>` tags for different error messages:

```html
<odac:field name="email" type="email">
  <odac:validate rule="required" message="Email is required"/>
  <odac:validate rule="email" message="Please enter a valid email address"/>
</odac:field>
```

Or combine rules in a single tag:

```html
<odac:field name="email" type="email">
  <odac:validate 
    rule="required|email" 
    message="Please enter a valid email address"/>
</odac:field>
```

## Message Placeholders

Use placeholders in error messages for dynamic values:

```html
<odac:field name="username" type="text">
  <odac:validate 
    rule="minlen:4" 
    message="Username '{value}' is too short. Minimum {min} characters required"/>
</odac:field>
```

**Available Placeholders:**
- `{value}` - User's input value
- `{field}` - Field name
- `{label}` - Field label or placeholder
- `{min}` - Minimum value (for minlen, min rules)
- `{max}` - Maximum value (for maxlen, max rules)
- `{len}` - Required length (for len rule)

## Submit Button

### `<odac:submit>`

Defines the submit button:

```html
<odac:submit 
  text="Login"                    <!-- Button text -->
  loading="Logging in..."         <!-- Loading state text -->
  class="btn btn-primary"         <!-- CSS class -->
  id="login-button">              <!-- HTML ID -->
</odac:submit>
```

Or use content as button text:

```html
<odac:submit>Login</odac:submit>
```

## Complete Examples

### Basic Login Form

```html
<odac:login redirect="/dashboard">
  
  <!-- Email Field -->
  <odac:field name="email" type="email" placeholder="Email Address">
    <odac:validate rule="required" message="Email is required"/>
    <odac:validate rule="email" message="Please enter a valid email address"/>
  </odac:field>
  
  <!-- Password Field -->
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required" message="Password is required"/>
  </odac:field>
  
  <!-- Submit Button -->
  <odac:submit text="Login" loading="Logging in..."/>
  
</odac:login>
```

### Login with Username

```html
<odac:login redirect="/dashboard">
  
  <!-- Username Field -->
  <odac:field name="username" type="text" placeholder="Username">
    <odac:validate rule="required" message="Username is required"/>
    <odac:validate rule="minlen:4" message="Username must be at least {min} characters"/>
  </odac:field>
  
  <!-- Password Field -->
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required" message="Password is required"/>
  </odac:field>
  
  <!-- Submit Button -->
  <odac:submit>Login</odac:submit>
  
</odac:login>
```

### Login with Remember Me

```html
<odac:login redirect="/dashboard">
  
  <!-- Email Field -->
  <odac:field name="email" type="email" placeholder="Email">
    <odac:validate rule="required|email" message="Please enter a valid email"/>
  </odac:field>
  
  <!-- Password Field -->
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required" message="Password is required"/>
  </odac:field>
  
  <!-- Remember Me Checkbox -->
  <odac:field name="remember" type="checkbox" label="Remember me">
  </odac:field>
  
  <!-- Submit Button -->
  <odac:submit>Login</odac:submit>
  
</odac:login>
```

### Styled Login Form

```html
<div class="login-container">
  <h2>Welcome Back</h2>
  
  <odac:login redirect="/dashboard">
    
    <div class="form-group">
      <odac:field 
        name="email" 
        type="email" 
        placeholder="Email Address"
        class="form-control">
        <odac:validate rule="required|email" message="Please enter a valid email"/>
      </odac:field>
      <span class="odac-form-error" odac-form-error="email"></span>
    </div>
    
    <div class="form-group">
      <odac:field 
        name="password" 
        type="password" 
        placeholder="Password"
        class="form-control">
        <odac:validate rule="required" message="Password is required"/>
      </odac:field>
      <span class="odac-form-error" odac-form-error="password"></span>
    </div>
    
    <div class="form-group">
      <odac:field name="remember" type="checkbox" label="Remember me" class="form-check-input">
      </odac:field>
    </div>
    
    <odac:submit class="btn btn-primary btn-block" text="Login" loading="Logging in..."/>
    
    <span class="odac-form-error" odac-form-error="_odac_form"></span>
    
  </odac:login>
  
  <p class="text-center mt-3">
    <a href="/forgot-password">Forgot password?</a>
  </p>
</div>
```

## Security Features

### Automatic Security

Odac automatically handles:

1. **CSRF Protection** - Form tokens prevent cross-site attacks
2. **Password Verification** - Passwords are verified against bcrypt hashes
3. **SQL Injection Prevention** - All queries are parameterized
4. **XSS Protection** - Input is sanitized
5. **Field Whitelisting** - Only defined fields are processed
6. **Token Expiration** - Form tokens expire after 30 minutes
7. **Session Security** - Secure session management with token rotation

### Token System

Each form gets a unique token when rendered:
- Token is stored server-side with field whitelist
- Token expires after 30 minutes
- Only fields defined in the view are accepted
- Session validation prevents token hijacking

## HTML5 Validation

Odac automatically adds HTML5 validation attributes for better UX:

```html
<!-- This field -->
<odac:field name="email" type="email">
  <odac:validate rule="required|email"/>
</odac:field>

<!-- Generates this HTML -->
<input 
  type="email" 
  name="email" 
  required>
```

**Automatic HTML5 Attributes:**
- `required` - For required fields
- `minlength` / `maxlength` - For length validation
- `min` / `max` - For number validation
- `pattern` - For alphanumeric, numeric, alpha rules
- `type="email"` - Native email validation

This provides instant feedback to users before form submission.

## Error Handling

### Display Errors

Error messages are automatically displayed in `<span>` elements with `odac-form-error` attribute:

```html
<!-- Errors appear here automatically -->
<span class="odac-form-error" odac-form-error="email" style="display:none;"></span>
<span class="odac-form-error" odac-form-error="password" style="display:none;"></span>
<span class="odac-form-error" odac-form-error="_odac_form" style="display:none;"></span>
```

### Success Messages

Success messages appear in elements with `odac-form-success` class:

```html
<!-- Success message appears here -->
<span class="odac-form-success" style="display:none;"></span>
```

### Custom Styling

Style error and success messages with CSS:

```css
.odac-form-error {
  color: red;
  font-size: 0.875rem;
  margin-top: 0.25rem;
  display: block;
}

.odac-form-success {
  color: green;
  font-size: 1rem;
  padding: 1rem;
  background: #d4edda;
  border: 1px solid #c3e6cb;
  border-radius: 0.25rem;
}

input._odac_error {
  border-color: red;
}
```

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
    "token": "user_tokens"      // Session token table (default: "user_tokens")
  }
}
```

**Default Values:**
- If `auth` is not specified, defaults are used
- Table: `users`
- Primary key: `id`
- Token table: `candy_auth`

### Database Schema

The users table should have been created during registration. If you need to create it manually:

```sql
CREATE TABLE `users` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `email` VARCHAR(255) NOT NULL UNIQUE,
  `username` VARCHAR(255) NULL UNIQUE,
  `password` VARCHAR(255) NOT NULL,
  `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

**Important:**
- Password field must be `VARCHAR(255)` to store bcrypt hashes
- Email or username should have `UNIQUE` constraint

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

## Authentication Methods

The login form supports multiple authentication methods:

### Email + Password

```html
<odac:login redirect="/dashboard">
  <odac:field name="email" type="email" placeholder="Email">
    <odac:validate rule="required|email"/>
  </odac:field>
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required"/>
  </odac:field>
  <odac:submit>Login</odac:submit>
</odac:login>
```

### Username + Password

```html
<odac:login redirect="/dashboard">
  <odac:field name="username" type="text" placeholder="Username">
    <odac:validate rule="required"/>
  </odac:field>
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required"/>
  </odac:field>
  <odac:submit>Login</odac:submit>
</odac:login>
```

### Email or Username + Password

```html
<odac:login redirect="/dashboard">
  <odac:field name="identifier" type="text" placeholder="Email or Username">
    <odac:validate rule="required"/>
  </odac:field>
  <odac:field name="password" type="password" placeholder="Password">
    <odac:validate rule="required"/>
  </odac:field>
  <odac:submit>Login</odac:submit>
</odac:login>
```

## Best Practices

1. **Always use HTTPS** - Login forms should only be served over HTTPS
2. **Validate on both client and server** - Odac does this automatically
3. **Provide clear error messages** - Don't reveal whether email/username exists
4. **Add "Forgot Password" link** - Help users recover their accounts
5. **Consider rate limiting** - Prevent brute force attacks (implement in controller)
6. **Use strong password requirements** - Enforce during registration
7. **Test form expiration** - Forms expire after 30 minutes

## Troubleshooting

### Form Not Submitting

- Check that `config.json` has MySQL configuration
- Verify database table exists
- Check browser console for JavaScript errors
- Ensure CSRF token is valid

### Validation Not Working

- Ensure validation rules are spelled correctly
- Check that field names match between `<odac:field>` and validation
- Verify HTML5 validation isn't blocking submission

### Login Failing

- Verify user exists in database
- Check that password was hashed with bcrypt during registration
- Ensure auth table name in config matches your database
- Check that credentials match exactly (case-sensitive)

### Token Expired

- Forms expire after 30 minutes
- User needs to refresh the page to get a new token
- Consider adding a message about session expiration

### Redirect Not Working

- Ensure `redirect` attribute is set on `<odac:login>`
- Check that the redirect URL is valid
- Verify user has permission to access the redirect page

## Related Documentation

- [Odac Register Forms](04-odac-register-forms.md) - Create registration forms
- [Session Management](05-session-management.md) - Understanding sessions
- [Authentication Overview](00-authentication-overview.md) - Auth system basics
