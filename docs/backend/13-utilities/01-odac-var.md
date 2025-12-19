## 🔧 Odac.Var - String Manipulation & Validation

`Odac.Var` is a powerful utility class for string manipulation, validation, encryption, and formatting. It provides a chainable, fluent interface for common string operations.

### Basic Usage

```javascript
// Create a Var instance
const result = Odac.Var('hello world').slug()
// Returns: 'hello-world'

// Chain multiple operations
const email = Odac.Var('  USER@EXAMPLE.COM  ').trim().toLowerCase()
```

### String Validation

Check if a string matches specific patterns:

#### is() - Single Validation

```javascript
// Email validation
Odac.Var('user@example.com').is('email')  // true
Odac.Var('invalid-email').is('email')     // false

// Numeric validation
Odac.Var('12345').is('numeric')           // true
Odac.Var('abc123').is('numeric')          // false

// Multiple conditions (AND logic)
Odac.Var('abc123').is('alphanumeric')     // true
```

#### isAny() - Multiple Validation (OR logic)

```javascript
// Check if value matches ANY of the conditions
Odac.Var('user@example.com').isAny('email', 'domain')  // true
Odac.Var('example.com').isAny('email', 'domain')       // true
```

#### Available Validation Types

```javascript
'alpha'              // Only letters (A-Z, a-z)
'alphaspace'         // Letters and spaces
'alphanumeric'       // Letters and numbers
'alphanumericspace'  // Letters, numbers, and spaces
'bcrypt'             // BCrypt hash format
'date'               // Valid date string
'domain'             // Valid domain name (example.com)
'email'              // Valid email address
'float'              // Floating point number
'host'               // IP address
'ip'                 // IP address
'json'               // Valid JSON string
'mac'                // MAC address
'md5'                // MD5 hash
'numeric'            // Numbers only
'url'                // Valid URL
'emoji'              // Contains emoji
'xss'                // XSS-safe (no HTML tags)
```

#### Practical Examples

```javascript
// Controller validation
module.exports = async function(Odac) {
  const email = Odac.Request.post('email')
  
  if (!Odac.Var(email).is('email')) {
    return Odac.return({
      success: false,
      message: 'Invalid email address'
    })
  }
  
  // Continue with valid email
}
```

### String Checking

#### contains() - Check if string contains values

```javascript
// Single value
Odac.Var('hello world').contains('world')  // true
Odac.Var('hello world').contains('foo')    // false

// Multiple values (AND logic - must contain all)
Odac.Var('hello world').contains('hello', 'world')  // true
Odac.Var('hello world').contains('hello', 'foo')    // false
```

#### containsAny() - Check if string contains any value

```javascript
// Check if contains ANY of the values (OR logic)
Odac.Var('hello world').containsAny('foo', 'world')  // true
Odac.Var('hello world').containsAny('foo', 'bar')    // false
```

#### isBegin() - Check if string starts with value

```javascript
Odac.Var('hello world').isBegin('hello')  // true
Odac.Var('hello world').isBegin('world')  // false

// Multiple options
Odac.Var('https://example.com').isBegin('http://', 'https://')  // true
```

#### isEnd() - Check if string ends with value

```javascript
Odac.Var('hello world').isEnd('world')  // true
Odac.Var('hello world').isEnd('hello')  // false

// Multiple options
Odac.Var('image.jpg').isEnd('.jpg', '.png', '.gif')  // true
```

### String Manipulation

#### replace() - Replace text

```javascript
// Simple replacement
Odac.Var('hello world').replace('world', 'universe')
// Returns: 'hello universe'

// Multiple replacements with object
Odac.Var('Hello {{name}}, welcome to {{site}}').replace({
  '{{name}}': 'John',
  '{{site}}': 'Odac'
})
// Returns: 'Hello John, welcome to Odac'

// Works with arrays/objects recursively
const data = {
  title: 'Welcome {{name}}',
  message: 'Hello {{name}}'
}
Odac.Var(data).replace({'{{name}}': 'John'})
// Returns: { title: 'Welcome John', message: 'Hello John' }
```

#### clear() - Remove specific strings

```javascript
Odac.Var('hello-world-test').clear('-')
// Returns: 'helloworldtest'

// Remove multiple strings
Odac.Var('a1b2c3').clear('1', '2', '3')
// Returns: 'abc'
```

#### slug() - Create URL-friendly slug

```javascript
Odac.Var('Hello World!').slug()
// Returns: 'hello-world'

Odac.Var('Product Name 2024').slug()
// Returns: 'product-name-2024'

// Custom separator
Odac.Var('Hello World').slug('_')
// Returns: 'hello_world'
```

#### format() - Format string with pattern

```javascript
// ? = single character, * = rest of string
Odac.Var('1234567890').format('(???) ???-????')
// Returns: '(123) 456-7890'

Odac.Var('TR1234567890').format('?? *')
// Returns: 'TR 1234567890'
```

#### html() - Escape HTML

```javascript
Odac.Var('<script>alert("xss")</script>').html()
// Returns: '&lt;script&gt;alert("xss")&lt;/script&gt;'
```

### Encryption & Hashing

#### hash() - BCrypt password hashing

```javascript
// Hash a password
const hashedPassword = Odac.Var('mypassword').hash()
// Returns: '$2b$10$...' (BCrypt hash)

// Custom salt rounds
const hashedPassword = Odac.Var('mypassword').hash(12)
```

#### hashCheck() - Verify BCrypt hash

```javascript
const hashedPassword = '$2b$10$...'
const isValid = Odac.Var(hashedPassword).hashCheck('mypassword')
// Returns: true or false
```

#### md5() - MD5 hash

```javascript
Odac.Var('hello').md5()
// Returns: '5d41402abc4b2a76b9719d911017c592'
```

#### encrypt() - AES-256 encryption

```javascript
// Uses key from Odac.Config.encrypt.key
const encrypted = Odac.Var('secret data').encrypt()

// Custom encryption key
const encrypted = Odac.Var('secret data').encrypt('my-32-character-encryption-key')
```

#### decrypt() - AES-256 decryption

```javascript
// Uses key from Odac.Config.encrypt.key
const decrypted = Odac.Var(encryptedData).decrypt()

// Custom decryption key
const decrypted = Odac.Var(encryptedData).decrypt('my-32-character-encryption-key')
```

### Date Formatting

#### date() - Format date strings

```javascript
const timestamp = '2024-03-15 14:30:45'

Odac.Var(timestamp).date('Y-m-d')
// Returns: '2024-03-15'

Odac.Var(timestamp).date('d/m/Y')
// Returns: '15/03/2024'

Odac.Var(timestamp).date('H:i:s')
// Returns: '14:30:45'

Odac.Var(timestamp).date('Y-m-d H:i')
// Returns: '2024-03-15 14:30'
```

**Format tokens:**
- `Y` - 4-digit year (2024)
- `y` - 2-digit year (24)
- `m` - Month with leading zero (01-12)
- `d` - Day with leading zero (01-31)
- `H` - Hour with leading zero (00-23)
- `i` - Minute with leading zero (00-59)
- `s` - Second with leading zero (00-59)

### File Operations

#### save() - Save string to file

```javascript
// Save content to file
Odac.Var('Hello World').save('/path/to/file.txt')

// Automatically creates directories if needed
Odac.Var(jsonData).save('/path/to/nested/dir/data.json')
```

### Practical Examples

#### User Registration with Validation

```javascript
module.exports = async function(Odac) {
  const email = Odac.Request.post('email')
  const password = Odac.Request.post('password')
  const username = Odac.Request.post('username')
  
  // Validate email
  if (!Odac.Var(email).is('email')) {
    return Odac.return({
      success: false,
      message: 'Invalid email address'
    })
  }
  
  // Validate username (alphanumeric only)
  if (!Odac.Var(username).is('alphanumeric')) {
    return Odac.return({
      success: false,
      message: 'Username must be alphanumeric'
    })
  }
  
  // Hash password
  const hashedPassword = Odac.Var(password).hash()
  
  // Create slug for profile URL
  const profileSlug = Odac.Var(username).slug()
  
  // Save user
  await Odac.Mysql.table('users').insert({
    email: email,
    username: username,
    password: hashedPassword,
    slug: profileSlug
  })
  
  return Odac.return({success: true})
}
```

#### Login with Password Verification

```javascript
module.exports = async function(Odac) {
  const email = Odac.Request.post('email')
  const password = Odac.Request.post('password')
  
  // Find user
  const user = await Odac.Mysql.table('users')
    .where('email', email)
    .first()
  
  if (!user) {
    return Odac.return({
      success: false,
      message: 'User not found'
    })
  }
  
  // Verify password
  const isValid = Odac.Var(user.password).hashCheck(password)
  
  if (!isValid) {
    return Odac.return({
      success: false,
      message: 'Invalid password'
    })
  }
  
  // Login successful
  Odac.Auth.login(user.id)
  return Odac.return({success: true})
}
```

#### URL Slug Generation

```javascript
module.exports = async function(Odac) {
  const title = Odac.Request.post('title')
  
  // Create URL-friendly slug
  const slug = Odac.Var(title).slug()
  
  // Check if slug exists
  const exists = await Odac.Mysql.table('posts')
    .where('slug', slug)
    .first()
  
  if (exists) {
    // Add timestamp to make unique
    const uniqueSlug = `${slug}-${Date.now()}`
    await Odac.Mysql.table('posts').insert({
      title: title,
      slug: uniqueSlug
    })
  } else {
    await Odac.Mysql.table('posts').insert({
      title: title,
      slug: slug
    })
  }
  
  return Odac.return({success: true})
}
```

#### Template Variable Replacement

```javascript
module.exports = async function(Odac) {
  const user = await Odac.Auth.user()
  
  // Email template
  const template = `
    Hello {{name}},
    
    Your account {{email}} has been verified.
    You can now access your dashboard at {{url}}.
    
    Thanks,
    {{site}}
  `
  
  // Replace variables
  const emailContent = Odac.Var(template).replace({
    '{{name}}': user.name,
    '{{email}}': user.email,
    '{{url}}': 'https://example.com/dashboard',
    '{{site}}': 'Odac'
  })
  
  // Send email
  await Odac.Mail.send({
    to: user.email,
    subject: 'Account Verified',
    body: emailContent
  })
  
  return Odac.return({success: true})
}
```

#### Phone Number Formatting

```javascript
module.exports = async function(Odac) {
  const phone = Odac.Request.post('phone')
  
  // Remove all non-numeric characters
  const cleanPhone = Odac.Var(phone).clear('-', ' ', '(', ')', '+')
  
  // Validate it's numeric
  if (!Odac.Var(cleanPhone).is('numeric')) {
    return Odac.return({
      success: false,
      message: 'Invalid phone number'
    })
  }
  
  // Format for display
  const formattedPhone = Odac.Var(cleanPhone).format('(???) ???-????')
  
  return Odac.return({
    success: true,
    phone: formattedPhone
  })
}
```

#### Data Encryption for Storage

```javascript
module.exports = async function(Odac) {
  const creditCard = Odac.Request.post('credit_card')
  
  // Encrypt sensitive data
  const encryptedCard = Odac.Var(creditCard).encrypt()
  
  // Save encrypted data
  await Odac.Mysql.table('payments').insert({
    user_id: Odac.Auth.id(),
    card: encryptedCard
  })
  
  return Odac.return({success: true})
}

// Later, to retrieve and decrypt
module.exports = async function(Odac) {
  const payment = await Odac.Mysql.table('payments')
    .where('user_id', Odac.Auth.id())
    .first()
  
  // Decrypt data
  const creditCard = Odac.Var(payment.card).decrypt()
  
  return Odac.return({
    card: creditCard
  })
}
```

### Best Practices

1. **Always validate user input** before processing
2. **Use hash() for passwords**, never store plain text
3. **Use encrypt() for sensitive data** like credit cards, SSNs
4. **Create slugs for URLs** to make them SEO-friendly
5. **Sanitize HTML** with html() to prevent XSS attacks
6. **Use isBegin/isEnd** for protocol or file extension checks

### Notes

- `Odac.Var()` returns the processed string value, not a Var instance (except for chaining)
- Encryption uses AES-256-CBC with a fixed IV
- BCrypt hashing is one-way and cannot be decrypted
- Date formatting works with any valid JavaScript date string
