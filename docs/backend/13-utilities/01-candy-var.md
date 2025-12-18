## 🔧 Candy.Var - String Manipulation & Validation

`Candy.Var` is a powerful utility class for string manipulation, validation, encryption, and formatting. It provides a chainable, fluent interface for common string operations.

### Basic Usage

```javascript
// Create a Var instance
const result = Candy.Var('hello world').slug()
// Returns: 'hello-world'

// Chain multiple operations
const email = Candy.Var('  USER@EXAMPLE.COM  ').trim().toLowerCase()
```

### String Validation

Check if a string matches specific patterns:

#### is() - Single Validation

```javascript
// Email validation
Candy.Var('user@example.com').is('email')  // true
Candy.Var('invalid-email').is('email')     // false

// Numeric validation
Candy.Var('12345').is('numeric')           // true
Candy.Var('abc123').is('numeric')          // false

// Multiple conditions (AND logic)
Candy.Var('abc123').is('alphanumeric')     // true
```

#### isAny() - Multiple Validation (OR logic)

```javascript
// Check if value matches ANY of the conditions
Candy.Var('user@example.com').isAny('email', 'domain')  // true
Candy.Var('example.com').isAny('email', 'domain')       // true
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
module.exports = async function(Candy) {
  const email = Candy.Request.post('email')
  
  if (!Candy.Var(email).is('email')) {
    return Candy.return({
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
Candy.Var('hello world').contains('world')  // true
Candy.Var('hello world').contains('foo')    // false

// Multiple values (AND logic - must contain all)
Candy.Var('hello world').contains('hello', 'world')  // true
Candy.Var('hello world').contains('hello', 'foo')    // false
```

#### containsAny() - Check if string contains any value

```javascript
// Check if contains ANY of the values (OR logic)
Candy.Var('hello world').containsAny('foo', 'world')  // true
Candy.Var('hello world').containsAny('foo', 'bar')    // false
```

#### isBegin() - Check if string starts with value

```javascript
Candy.Var('hello world').isBegin('hello')  // true
Candy.Var('hello world').isBegin('world')  // false

// Multiple options
Candy.Var('https://example.com').isBegin('http://', 'https://')  // true
```

#### isEnd() - Check if string ends with value

```javascript
Candy.Var('hello world').isEnd('world')  // true
Candy.Var('hello world').isEnd('hello')  // false

// Multiple options
Candy.Var('image.jpg').isEnd('.jpg', '.png', '.gif')  // true
```

### String Manipulation

#### replace() - Replace text

```javascript
// Simple replacement
Candy.Var('hello world').replace('world', 'universe')
// Returns: 'hello universe'

// Multiple replacements with object
Candy.Var('Hello {{name}}, welcome to {{site}}').replace({
  '{{name}}': 'John',
  '{{site}}': 'Odac'
})
// Returns: 'Hello John, welcome to Odac'

// Works with arrays/objects recursively
const data = {
  title: 'Welcome {{name}}',
  message: 'Hello {{name}}'
}
Candy.Var(data).replace({'{{name}}': 'John'})
// Returns: { title: 'Welcome John', message: 'Hello John' }
```

#### clear() - Remove specific strings

```javascript
Candy.Var('hello-world-test').clear('-')
// Returns: 'helloworldtest'

// Remove multiple strings
Candy.Var('a1b2c3').clear('1', '2', '3')
// Returns: 'abc'
```

#### slug() - Create URL-friendly slug

```javascript
Candy.Var('Hello World!').slug()
// Returns: 'hello-world'

Candy.Var('Product Name 2024').slug()
// Returns: 'product-name-2024'

// Custom separator
Candy.Var('Hello World').slug('_')
// Returns: 'hello_world'
```

#### format() - Format string with pattern

```javascript
// ? = single character, * = rest of string
Candy.Var('1234567890').format('(???) ???-????')
// Returns: '(123) 456-7890'

Candy.Var('TR1234567890').format('?? *')
// Returns: 'TR 1234567890'
```

#### html() - Escape HTML

```javascript
Candy.Var('<script>alert("xss")</script>').html()
// Returns: '&lt;script&gt;alert("xss")&lt;/script&gt;'
```

### Encryption & Hashing

#### hash() - BCrypt password hashing

```javascript
// Hash a password
const hashedPassword = Candy.Var('mypassword').hash()
// Returns: '$2b$10$...' (BCrypt hash)

// Custom salt rounds
const hashedPassword = Candy.Var('mypassword').hash(12)
```

#### hashCheck() - Verify BCrypt hash

```javascript
const hashedPassword = '$2b$10$...'
const isValid = Candy.Var(hashedPassword).hashCheck('mypassword')
// Returns: true or false
```

#### md5() - MD5 hash

```javascript
Candy.Var('hello').md5()
// Returns: '5d41402abc4b2a76b9719d911017c592'
```

#### encrypt() - AES-256 encryption

```javascript
// Uses key from Candy.Config.encrypt.key
const encrypted = Candy.Var('secret data').encrypt()

// Custom encryption key
const encrypted = Candy.Var('secret data').encrypt('my-32-character-encryption-key')
```

#### decrypt() - AES-256 decryption

```javascript
// Uses key from Candy.Config.encrypt.key
const decrypted = Candy.Var(encryptedData).decrypt()

// Custom decryption key
const decrypted = Candy.Var(encryptedData).decrypt('my-32-character-encryption-key')
```

### Date Formatting

#### date() - Format date strings

```javascript
const timestamp = '2024-03-15 14:30:45'

Candy.Var(timestamp).date('Y-m-d')
// Returns: '2024-03-15'

Candy.Var(timestamp).date('d/m/Y')
// Returns: '15/03/2024'

Candy.Var(timestamp).date('H:i:s')
// Returns: '14:30:45'

Candy.Var(timestamp).date('Y-m-d H:i')
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
Candy.Var('Hello World').save('/path/to/file.txt')

// Automatically creates directories if needed
Candy.Var(jsonData).save('/path/to/nested/dir/data.json')
```

### Practical Examples

#### User Registration with Validation

```javascript
module.exports = async function(Candy) {
  const email = Candy.Request.post('email')
  const password = Candy.Request.post('password')
  const username = Candy.Request.post('username')
  
  // Validate email
  if (!Candy.Var(email).is('email')) {
    return Candy.return({
      success: false,
      message: 'Invalid email address'
    })
  }
  
  // Validate username (alphanumeric only)
  if (!Candy.Var(username).is('alphanumeric')) {
    return Candy.return({
      success: false,
      message: 'Username must be alphanumeric'
    })
  }
  
  // Hash password
  const hashedPassword = Candy.Var(password).hash()
  
  // Create slug for profile URL
  const profileSlug = Candy.Var(username).slug()
  
  // Save user
  await Candy.Mysql.table('users').insert({
    email: email,
    username: username,
    password: hashedPassword,
    slug: profileSlug
  })
  
  return Candy.return({success: true})
}
```

#### Login with Password Verification

```javascript
module.exports = async function(Candy) {
  const email = Candy.Request.post('email')
  const password = Candy.Request.post('password')
  
  // Find user
  const user = await Candy.Mysql.table('users')
    .where('email', email)
    .first()
  
  if (!user) {
    return Candy.return({
      success: false,
      message: 'User not found'
    })
  }
  
  // Verify password
  const isValid = Candy.Var(user.password).hashCheck(password)
  
  if (!isValid) {
    return Candy.return({
      success: false,
      message: 'Invalid password'
    })
  }
  
  // Login successful
  Candy.Auth.login(user.id)
  return Candy.return({success: true})
}
```

#### URL Slug Generation

```javascript
module.exports = async function(Candy) {
  const title = Candy.Request.post('title')
  
  // Create URL-friendly slug
  const slug = Candy.Var(title).slug()
  
  // Check if slug exists
  const exists = await Candy.Mysql.table('posts')
    .where('slug', slug)
    .first()
  
  if (exists) {
    // Add timestamp to make unique
    const uniqueSlug = `${slug}-${Date.now()}`
    await Candy.Mysql.table('posts').insert({
      title: title,
      slug: uniqueSlug
    })
  } else {
    await Candy.Mysql.table('posts').insert({
      title: title,
      slug: slug
    })
  }
  
  return Candy.return({success: true})
}
```

#### Template Variable Replacement

```javascript
module.exports = async function(Candy) {
  const user = await Candy.Auth.user()
  
  // Email template
  const template = `
    Hello {{name}},
    
    Your account {{email}} has been verified.
    You can now access your dashboard at {{url}}.
    
    Thanks,
    {{site}}
  `
  
  // Replace variables
  const emailContent = Candy.Var(template).replace({
    '{{name}}': user.name,
    '{{email}}': user.email,
    '{{url}}': 'https://example.com/dashboard',
    '{{site}}': 'Odac'
  })
  
  // Send email
  await Candy.Mail.send({
    to: user.email,
    subject: 'Account Verified',
    body: emailContent
  })
  
  return Candy.return({success: true})
}
```

#### Phone Number Formatting

```javascript
module.exports = async function(Candy) {
  const phone = Candy.Request.post('phone')
  
  // Remove all non-numeric characters
  const cleanPhone = Candy.Var(phone).clear('-', ' ', '(', ')', '+')
  
  // Validate it's numeric
  if (!Candy.Var(cleanPhone).is('numeric')) {
    return Candy.return({
      success: false,
      message: 'Invalid phone number'
    })
  }
  
  // Format for display
  const formattedPhone = Candy.Var(cleanPhone).format('(???) ???-????')
  
  return Candy.return({
    success: true,
    phone: formattedPhone
  })
}
```

#### Data Encryption for Storage

```javascript
module.exports = async function(Candy) {
  const creditCard = Candy.Request.post('credit_card')
  
  // Encrypt sensitive data
  const encryptedCard = Candy.Var(creditCard).encrypt()
  
  // Save encrypted data
  await Candy.Mysql.table('payments').insert({
    user_id: Candy.Auth.id(),
    card: encryptedCard
  })
  
  return Candy.return({success: true})
}

// Later, to retrieve and decrypt
module.exports = async function(Candy) {
  const payment = await Candy.Mysql.table('payments')
    .where('user_id', Candy.Auth.id())
    .first()
  
  // Decrypt data
  const creditCard = Candy.Var(payment.card).decrypt()
  
  return Candy.return({
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

- `Candy.Var()` returns the processed string value, not a Var instance (except for chaining)
- Encryption uses AES-256-CBC with a fixed IV
- BCrypt hashing is one-way and cannot be decrypted
- Date formatting works with any valid JavaScript date string
