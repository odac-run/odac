## ⚙️ Configuration Overview

Odac uses a simple and flexible configuration system based on `config.json` and optional `.env` files. You can choose the approach that best fits your needs.

### Configuration Files

#### config.json (Required)
The main configuration file located in your website's root directory. This file contains all your application settings in JSON format.

```json
{
  "request": {
    "timeout": 30000
  },
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "secret123",
    "database": "myapp"
  }
}
```

#### .env (Optional)
An optional environment variables file for storing sensitive information like passwords and API keys. This file should be added to `.gitignore` to keep secrets out of version control.

```bash
# .env
MYSQL_PASSWORD=super_secret_123
API_KEY=your_api_key_here
NODE_ENV=production
```

### Three Ways to Configure

#### 1. Direct Values (Simple)
Perfect for development or non-sensitive settings:

```json
{
  "mysql": {
    "host": "localhost",
    "password": "dev123"
  }
}
```

#### 2. Environment Variables (Secure)
Use `${VARIABLE}` syntax in `config.json` to reference `.env` values:

```json
{
  "mysql": {
    "host": "${MYSQL_HOST}",
    "password": "${MYSQL_PASSWORD}"
  }
}
```

```bash
# .env
MYSQL_HOST=production.db.com
MYSQL_PASSWORD=super_secret_123
```

#### 3. Mixed Approach (Flexible)
Combine both methods - use direct values for non-sensitive data and environment variables for secrets:

```json
{
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "${MYSQL_PASSWORD}",
    "database": "myapp"
  },
  "api": {
    "endpoint": "https://api.example.com",
    "key": "${API_KEY}"
  }
}
```

### Accessing Configuration

#### In Controllers
You can access configuration values in three ways:

```javascript
// 1. From Odac.Config (recommended for structured config)
const dbHost = Odac.Config.mysql.host

// 2. Using Odac.env() helper
const apiKey = Odac.env('API_KEY')
const debug = Odac.env('DEBUG', 'false')

// 3. Direct process.env access
const nodeEnv = process.env.NODE_ENV
```

### Best Practices

**Development:**
- Use direct values in `config.json` for quick setup
- Keep development credentials simple

**Production:**
- Store sensitive data in `.env` file
- Add `.env` to `.gitignore`
- Use environment variables for passwords, API keys, and tokens
- Copy `.env.example` to `.env` and fill in production values

**Version Control:**
- Commit `config.json` with `${VARIABLE}` placeholders
- Commit `.env.example` with dummy values
- Never commit `.env` with real credentials

### Common Configuration Options

**Request timeout:**
```json
{
  "request": {
    "timeout": 30000
  }
}
```

**Database connection:**
```json
{
  "database": {
    "host": "localhost",
    "user": "root",
    "password": "${MYSQL_PASSWORD}",
    "database": "myapp"
  }
}
```

**Authentication sessions:**
```json
{
  "auth": {
    "table": "users",
    "token": "user_tokens",
    "maxAge": 2592000000,
    "updateAge": 86400000
  }
}
```

**Early Hints (HTTP 103):**
```json
{
  "earlyHints": {
    "enabled": true,
    "auto": true,
    "maxResources": 5
  }
}
```

Early Hints is a performance optimization feature that works automatically without any configuration. The server sends preliminary HTTP headers to the browser before the final response, allowing browsers to start preloading critical resources (CSS, JavaScript, fonts) earlier. This is completely zero-config - it detects resources from your HTML automatically and sends hints on subsequent requests.

See individual documentation sections for detailed configuration options.

### Example Setup

**config.json** (committed to git):
```json
{
  "request": {
    "timeout": 30000
  },
  "mysql": {
    "host": "${MYSQL_HOST}",
    "user": "${MYSQL_USER}",
    "password": "${MYSQL_PASSWORD}",
    "database": "myapp"
  },
  "auth": {
    "maxAge": 2592000000,
    "updateAge": 86400000
  },
  "mail": {
    "from": "${MAIL_FROM}"
  }
}
```

**.env.example** (committed to git):
```bash
# Database
MYSQL_HOST=localhost
MYSQL_USER=root
MYSQL_PASSWORD=your_password_here

# Mail
MAIL_FROM=noreply@example.com
```

**.env** (gitignored, not committed):
```bash
# Database
MYSQL_HOST=production.db.com
MYSQL_USER=prod_user
MYSQL_PASSWORD=super_secret_production_password

# Mail
MAIL_FROM=noreply@myapp.com
```

This approach keeps your code secure while maintaining flexibility across different environments.
