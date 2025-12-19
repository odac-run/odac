# User Registration

The `Odac.Auth.register()` method provides a secure and user-friendly way to create new user accounts with automatic password hashing, duplicate checking, and optional auto-login.

## Basic Usage

```javascript
module.exports = async function (Odac) {
  const result = await Odac.Auth.register({
    email: 'user@example.com',
    username: 'johndoe',
    password: 'securePassword123',
    name: 'John Doe'
  })
  
  if (result.success) {
    return {message: 'Registration successful', user: result.user}
  } else {
    return {error: result.error}
  }
}
```

## Advanced Options

```javascript
const result = await Odac.Auth.register(
  {
    email: 'user@example.com',
    username: 'johndoe',
    password: 'securePassword123',
    name: 'John Doe',
    role: 'user'
  },
  {
    passwordField: 'password',      // Field name for password (default: 'password')
    uniqueFields: ['email', 'username'], // Fields to check for duplicates (default: ['email'])
    autoLogin: true                 // Auto-login after registration (default: true)
  }
)
```

## Response Format

### Success Response

```javascript
{
  success: true,
  user: {
    id: 123,
    email: 'user@example.com',
    username: 'johndoe',
    // ... other user fields
  }
}
```

### Error Response

```javascript
{
  success: false,
  error: 'email already exists',
  field: 'email'  // Only present for duplicate field errors
}
```

## Features

- **Automatic Password Hashing**: Passwords are automatically hashed using bcrypt
- **Duplicate Prevention**: Checks for existing users with the same email/username
- **Auto-Login**: Optionally logs in the user immediately after registration
- **Flexible Configuration**: Customize password field name and unique fields
- **Detailed Error Messages**: Returns specific error information for better UX

## Example Controller

```javascript
module.exports = async function (Odac) {
  const validator = Odac.Validator

  // Validate input
  validator.post('email').check('required|email').message('A valid email is required')
  validator.post('username').check('required|minlen:4').message('Username must be at least 4 characters')
  validator.post('password').check('required|minlen:8').message('Password must be at least 8 characters')

  if (await validator.error()) {
    return validator.result('Please fix the errors below')
  }

  // Get validated data
  const email = await Odac.request('email')
  const username = await Odac.request('username')
  const password = await Odac.request('password')
  const name = await Odac.request('name')
  
  // Register user
  const result = await Odac.Auth.register(
    {email, username, password, name},
    {uniqueFields: ['email', 'username']}
  )
  
  if (result.success) {
    // User is now registered and logged in
    return Odac.direct('/dashboard')
  } else {
    // Show error message
    return {error: result.error}
  }
}
```

## Configuration

Make sure your `config.json` has the auth configuration:

```json
{
  "auth": {
    "table": "users",
    "key": "id",
    "token": "user_tokens"
  }
}
```

## Security Notes

- Passwords are automatically hashed with bcrypt before storage
- The system automatically detects already-hashed passwords (bcrypt pattern) to prevent double-hashing
- Never store plain text passwords
- Use HTTPS in production to protect credentials in transit
- Consider adding rate limiting to prevent brute force attacks
