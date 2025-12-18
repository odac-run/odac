## 🌍 Environment Variables

Odac supports environment variables through `.env` files, making it easy to manage sensitive data and environment-specific settings.

### Creating a .env File

Create a `.env` file in your website's root directory (same location as `config.json`):

```bash
# .env

# Application
NODE_ENV=production
DEBUG=false
APP_URL=https://myapp.com

# Database
MYSQL_HOST=localhost
MYSQL_USER=root
MYSQL_PASSWORD=super_secret_123
MYSQL_DATABASE=myapp

# API Keys
STRIPE_SECRET_KEY=sk_live_xxxxx
STRIPE_WEBHOOK_SECRET=whsec_xxxxx
API_KEY=your_api_key_here

# Mail
MAIL_FROM=noreply@myapp.com
SMTP_HOST=smtp.gmail.com
SMTP_PORT=587
SMTP_USER=user@gmail.com
SMTP_PASSWORD=app_password

# Features
FEATURE_BETA=true
MAINTENANCE_MODE=false
```

### Using in config.json

Reference environment variables using `${VARIABLE_NAME}` syntax:

```json
{
  "mysql": {
    "host": "${MYSQL_HOST}",
    "user": "${MYSQL_USER}",
    "password": "${MYSQL_PASSWORD}",
    "database": "${MYSQL_DATABASE}"
  },
  "api": {
    "stripe": {
      "key": "${STRIPE_SECRET_KEY}",
      "webhook": "${STRIPE_WEBHOOK_SECRET}"
    }
  },
  "mail": {
    "from": "${MAIL_FROM}"
  }
}
```

### Accessing in Controllers

You can access environment variables in your controllers in three ways:

#### 1. Using Odac.env() (Recommended)

```javascript
module.exports = function() {
  const apiKey = Odac.env('API_KEY')
  const debug = Odac.env('DEBUG', 'false')
  const port = Odac.env('PORT', '3000')
  
  if (debug === 'true') {
    console.log('Debug mode enabled')
  }
}
```

The second parameter is the default value if the variable is not set.

#### 2. Using process.env

```javascript
module.exports = function() {
  const nodeEnv = process.env.NODE_ENV
  const debug = process.env.DEBUG === 'true'
  const apiKey = process.env.API_KEY
}
```

#### 3. From Odac.Config (if defined in config.json)

```javascript
module.exports = function() {
  const dbHost = Odac.Config.mysql.host
  const apiKey = Odac.Config.api.stripe.key
}
```

### Practical Examples

#### Feature Flags

```javascript
// controller/home.js
module.exports = function() {
  const betaEnabled = Odac.env('FEATURE_BETA', 'false') === 'true'
  const maintenance = Odac.env('MAINTENANCE_MODE', 'false') === 'true'
  
  if (maintenance) {
    return Odac.abort(503)
  }
  
  Odac.set('betaEnabled', betaEnabled)
  return Odac.View.render('home')
}
```

#### API Integration

```javascript
// controller/payment.js
module.exports = async function() {
  const stripeKey = Odac.env('STRIPE_SECRET_KEY')
  const webhookSecret = Odac.env('STRIPE_WEBHOOK_SECRET')
  
  const stripe = require('stripe')(stripeKey)
  
  // Process payment...
}
```

#### Mail Configuration

```javascript
// controller/contact.js
module.exports = async function() {
  const mail = Odac.Mail()
  
  mail.from(Odac.env('MAIL_FROM', 'noreply@example.com'))
  mail.to(Odac.request('email'))
  mail.subject('Thank you for contacting us')
  mail.html('<h1>We received your message!</h1>')
  
  await mail.send()
  
  return Odac.return({ success: true })
}
```

### Security Best Practices

#### 1. Add .env to .gitignore

The `.env` file should never be committed to version control:

```gitignore
# .gitignore
.env
.env.local
.env.*.local
```

#### 2. Create .env.example

Provide a template for other developers:

```bash
# .env.example

# Database
MYSQL_HOST=localhost
MYSQL_USER=root
MYSQL_PASSWORD=your_password_here
MYSQL_DATABASE=myapp

# API Keys
API_KEY=your_api_key_here
```

Commit `.env.example` to git, but not `.env`.

#### 3. Different Environments

Use different `.env` files for different environments:

**Development (.env):**
```bash
NODE_ENV=development
DEBUG=true
MYSQL_HOST=localhost
MYSQL_PASSWORD=dev123
```

**Production (.env):**
```bash
NODE_ENV=production
DEBUG=false
MYSQL_HOST=production.db.com
MYSQL_PASSWORD=super_secure_production_password
```

### Comments and Formatting

The `.env` file supports:

- **Comments:** Lines starting with `#`
- **Quotes:** Values can be wrapped in single or double quotes
- **Spaces:** Spaces around `=` are trimmed

```bash
# This is a comment
API_KEY=simple_value
DB_PASSWORD="password with spaces"
MAIL_FROM='noreply@example.com'
```

### Important Notes

- Environment variables are loaded when the application starts
- Changes to `.env` require restarting the application
- The `.env` file is **optional** - you can use direct values in `config.json` if preferred
- Variables defined in `.env` are available throughout your entire application
- If a variable is not found, `Odac.env()` returns the default value or `undefined`
