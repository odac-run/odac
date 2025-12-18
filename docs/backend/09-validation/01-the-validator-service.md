## ✅ The `Validator` Service

The Validator service provides a fluent, chainable API for validating user input. It's automatically available in your controllers through `Odac.Validator`.

#### Basic Usage

The validator uses a method-chaining pattern:

```javascript
const validator = Odac.Validator
validator.post('email').check('required|email').message('Valid email required')
validator.post('password').check('required|minlen:8').message('Password must be at least 8 characters')

if (await validator.error()) {
  return validator.result('Validation failed')
}
```

#### Available Methods

- `post(key)` - Validate a POST field
- `get(key)` - Validate a GET field
- `var(name, value)` - Validate a custom variable
- `file(name)` - Validate a file upload
- `check(rules)` - Define validation rules (pipe-separated)
- `message(text)` - Set custom error message
- `error()` - Returns true if validation failed (async)
- `result(message, data)` - Returns formatted result object (async)
- `success(callback)` - Returns success result with data
- `brute(maxAttempts)` - Enable brute force protection (default: 5 attempts)

#### Validation Rules

**Type Validation:**
- `required` - Field cannot be empty
- `accepted` - Must be 1, 'on', 'yes', or true
- `numeric` - Must be a number
- `alpha` - Only alphabetic characters
- `alphaspace` - Alphabetic characters and spaces
- `alphanumeric` - Alphanumeric characters only
- `alphanumericspace` - Alphanumeric characters and spaces
- `username` - Alphanumeric username (no spaces or special chars)
- `email` - Valid email address
- `ip` - Valid IP address
- `float` - Floating point number
- `mac` - Valid MAC address
- `domain` - Valid domain name
- `url` - Valid URL
- `array` - Must be an array
- `date` - Valid date format

**Length Validation:**
- `len:X` - Exact length must be X
- `minlen:X` - Minimum length of X characters
- `maxlen:X` - Maximum length of X characters

**Value Validation:**
- `min:X` - Minimum value of X
- `max:X` - Maximum value of X
- `equal:value` - Must equal specific value
- `not:value` - Must not equal specific value
- `same:field` - Must match another field
- `different:field` - Must differ from another field

**Date Validation:**
- `mindate:date` - Must be after specified date
- `maxdate:date` - Must be before specified date

**String Validation:**
- `in:substring` - Must contain substring
- `notin:substring` - Must not contain substring
- `regex:pattern` - Must match regex pattern

**Security:**
- `xss` - Check for HTML tags (XSS protection)
- `usercheck` - User must be authenticated
- `user:field` - Must match authenticated user's field value

**Inverse Rules:**
Use `!` prefix to invert any rule: `!required`, `!email`, etc.

#### Example: User Registration

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator.post('username').check('required|username|minlen:4|maxlen:20').message('Username must be 4-20 alphanumeric characters')
  validator.post('email').check('required|email').message('Valid email address required')
  validator.post('password').check('required|minlen:8').message('Password must be at least 8 characters')
  validator.post('password_confirm').check('required|same:password').message('Passwords must match')

  if (await validator.error()) {
    return validator.result('Validation failed')
  }

  return validator.success('User registered successfully')
}
```

#### Example: Login with Brute Force Protection

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator.post('email').check('required|email').message('Email required')
  validator.post('password').check('required').message('Password required')
  validator.brute(5)

  if (await validator.error()) {
    return validator.result('Login failed')
  }

  return validator.success({token: 'abc123'})
}
```

#### Example: Custom Variable Validation

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator
  const customValue = calculateSomething()

  validator.var('calculated_value', customValue).check('numeric|min:100|max:1000').message('Value must be between 100 and 1000')

  if (await validator.error()) {
    return validator.result('Invalid calculation')
  }

  return validator.success('Calculation valid')
}
```

#### Multiple Checks Per Field

You can chain multiple `check()` calls for the same field, each with its own specific error message. The validator will return the first error it encounters:

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator
    .post('password')
    .check('required').message('Password is required')
    .check('minlen:8').message('Password must be at least 8 characters')
    .check('maxlen:50').message('Password cannot exceed 50 characters')
    .check('regex:[A-Z]').message('Password must contain at least one uppercase letter')
    .check('regex:[0-9]').message('Password must contain at least one number')

  if (await validator.error()) {
    return validator.result('Validation failed')
  }

  return validator.success('Password is strong')
}
```

#### Example: Complex Form Validation

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator
    .post('username')
    .check('required').message('Username is required')
    .check('username').message('Username can only contain letters and numbers')
    .check('minlen:3').message('Username must be at least 3 characters')
    .check('maxlen:20').message('Username cannot exceed 20 characters')

  validator
    .post('email')
    .check('required').message('Email is required')
    .check('email').message('Please enter a valid email address')

  validator
    .post('age')
    .check('required').message('Age is required')
    .check('numeric').message('Age must be a number')
    .check('min:18').message('You must be at least 18 years old')
    .check('max:120').message('Please enter a valid age')

  validator
    .post('website')
    .check('!required').message('Website is optional')
    .check('url').message('Please enter a valid URL')

  validator
    .post('bio')
    .check('maxlen:500').message('Bio cannot exceed 500 characters')
    .check('xss').message('Bio contains invalid HTML tags')

  if (await validator.error()) {
    return validator.result('Please fix the errors')
  }

  return validator.success('Profile updated successfully')
}
```

#### Example: Date Range Validation

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator
    .post('start_date')
    .check('required').message('Start date is required')
    .check('date').message('Invalid date format')
    .check('mindate:2024-01-01').message('Start date must be after January 1, 2024')

  validator
    .post('end_date')
    .check('required').message('End date is required')
    .check('date').message('Invalid date format')
    .check('maxdate:2025-12-31').message('End date must be before December 31, 2025')

  if (await validator.error()) {
    return validator.result('Invalid date range')
  }

  return validator.success('Date range is valid')
}
```

#### Example: Conditional Validation with Custom Variables

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator
  const userRole = Odac.Auth.user('role')
  const userCredits = Odac.Auth.user('credits') || 0

  validator.post('title').check('required').message('Title is required')
  validator.post('content').check('required').message('Content is required')

  if (userRole !== 'premium') {
    validator
      .var('user_credits', userCredits)
      .check('numeric').message('Invalid credits value')
      .check('min:10').message('You need at least 10 credits to publish')

    validator
      .post('content')
      .check('maxlen:1000').message('Free users are limited to 1000 characters')
  }

  validator
    .var('role_check', userRole)
    .check('!equal:banned').message('Your account has been banned')

  if (await validator.error()) {
    return validator.result('Validation failed')
  }

  return validator.success('Article published')
}
```

#### Example: User Authentication Validation

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator
    .post('current_password')
    .check('required').message('Current password is required')
    .check('user:password').message('Current password is incorrect')

  validator
    .post('new_password')
    .check('required').message('New password is required')
    .check('minlen:8').message('New password must be at least 8 characters')
    .check('different:current_password').message('New password must be different from current')

  if (await validator.error()) {
    return validator.result('Password change failed')
  }

  return validator.success('Password changed successfully')
}
```

#### Example: Boolean Function Results

You can use boolean values directly in `check()` for custom validation logic:

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator
  const userId = await Odac.request('user_id')
  
  const isOwner = await checkIfUserOwnsResource(userId)
  const hasPermission = await checkUserPermission('edit')
  const isWithinLimit = await checkDailyLimit(userId)

  validator.post('title').check('required').message('Title is required')
  
  validator
    .var('ownership', null)
    .check(isOwner).message('You do not own this resource')
  
  validator
    .var('permission', null)
    .check(hasPermission).message('You do not have permission to edit')
  
  validator
    .var('limit', null)
    .check(isWithinLimit).message('You have reached your daily edit limit')

  if (await validator.error()) {
    return validator.result('Validation failed')
  }

  return validator.success('Resource updated')
}
```

#### Example: Chained Validation (Single Statement)

```javascript
module.exports = async function (Odac) {
  return await Odac.Validator
    .post('email').check('required').message('Email required').check('email').message('Invalid email')
    .post('password').check('required').message('Password required').check('minlen:8').message('Min 8 chars')
    .post('age').check('required').message('Age required').check('numeric').message('Must be number').check('min:18').message('Must be 18+')
    .success('Registration successful')
}
```

#### Example: Admin-Only Action with User Check

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  validator
    .var('auth_check', null)
    .check('usercheck').message('You must be logged in')

  validator
    .var('admin_role', null)
    .check('user:role').message('Admin access required')
    .check('equal:admin').message('Only admins can perform this action')

  validator.post('action').check('required').message('Action is required')

  if (await validator.error()) {
    return validator.result('Access denied')
  }

  return validator.success('Action completed')
}
```

#### Frontend Integration

When using `Odac.form()` on the frontend, validation errors are automatically displayed:

**Automatic Error Display:**
- Each field's error message appears below the input with attribute `[odac-form-error="fieldname"]`
- Invalid inputs get the `_odac_error` CSS class automatically
- Errors fade out when the user focuses on the input

**Success Messages:**
- Success messages appear in elements with `[odac-form-success]` attribute
- Automatically fades in when validation passes

**Auto-Redirect:**
- If you pass a URL as the second parameter to `Odac.form()`, successful submissions automatically redirect:
  ```javascript
  Odac.form('myForm', '/dashboard') // Redirects to /dashboard on success
  ```

**Example HTML:**
```html
<form odac-form="register" action="/api/register" method="POST">
  <input type="email" name="email" placeholder="Email">
  <span odac-form-error="email"></span>
  
  <input type="password" name="password" placeholder="Password">
  <span odac-form-error="password"></span>
  
  <button type="submit">Register</button>
  
  <span odac-form-success></span>
</form>

<script>
  Odac.form('register', '/dashboard') // Auto-redirect on success
</script>
```

#### Response Format

The `result()` method returns a standardized response:

**Success:**
```json
{
  "result": {
    "success": true,
    "message": "Operation successful"
  },
  "data": null
}
```

**Error:**
```json
{
  "result": {
    "success": false
  },
  "errors": {
    "email": "Valid email address required",
    "password": "Password must be at least 8 characters"
  }
}
```
