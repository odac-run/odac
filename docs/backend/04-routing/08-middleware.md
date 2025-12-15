## 🔗 Middleware

Middleware allows you to run code before your controllers execute. Perfect for authentication, logging, rate limiting, and more.

### Creating Middleware

Create middleware files in the `middleware/` directory:

```javascript
// middleware/auth.js
module.exports = async (Candy) => {
  if (!await Candy.Auth.check()) {
    return Candy.direct('/login')  // Redirect to login
  }
  // No return = continue to next middleware or controller
}
```

**Middleware Rules:**
- Return `false` → Stop execution (403 Forbidden)
- Return `Candy.abort(code)` → Stop with custom error code
- Return `Candy.direct(url)` → Stop and redirect
- Return nothing or `true` → Continue to next middleware/controller

### Using Middleware

#### Single Route
```javascript
Candy.Route
  .use('logger')
  .page('/contact', 'contact')
```

#### Multiple Routes
```javascript
Candy.Route
  .use('cors')
  .page('/api/users', 'api.users')
  .page('/api/posts', 'api.posts')
  .get('/api/stats', 'api.stats')
```

#### Multiple Middlewares
```javascript
Candy.Route
  .use('cors', 'rateLimit')
  .post('/api/upload', 'api.upload')
```

#### With Auth Routes

`Candy.Route.auth` already requires authentication. You can add additional middleware on top:

```javascript
// Admin-only routes (requires login + admin role)
Candy.Route.auth
  .use('admin')
  .page('/admin/dashboard', 'admin.dashboard')
  .page('/admin/users', 'admin.users')
  .post('/admin/settings', 'admin.settings')
```

```javascript
// Premium user routes (requires login + premium subscription)
Candy.Route.auth
  .use('premium')
  .page('/premium/content', 'premium.content')
  .page('/premium/downloads', 'premium.downloads')
```

```javascript
// Multiple middlewares with auth
Candy.Route.auth
  .use('verified', 'rateLimit')
  .post('/api/sensitive', 'api.sensitive')
```

### Middleware Examples

#### Authentication
```javascript
// middleware/auth.js
module.exports = async (Candy) => {
  if (!await Candy.Auth.check()) {
    return Candy.direct('/login')
  }
}
```

#### Admin Check
```javascript
// middleware/admin.js
module.exports = async (Candy) => {
  const user = await Candy.Auth.user()
  if (!user || user.role !== 'admin') {
    return false  // 403 Forbidden
  }
}
```

#### CORS Headers
```javascript
// middleware/cors.js
module.exports = async (Candy) => {
  Candy.Request.header('Access-Control-Allow-Origin', '*')
  Candy.Request.header('Access-Control-Allow-Methods', 'GET, POST, PUT, DELETE')
}
```

#### Rate Limiting
```javascript
// middleware/rateLimit.js
const requests = new Map()
let lastCleanup = Date.now()

module.exports = async (Candy) => {
  const ip = Candy.Request.ip
  const now = Date.now()
  const limit = 100
  const window = 60000
  
  if (now - lastCleanup > window) {
    for (const [key, times] of requests.entries()) {
      const recent = times.filter(time => now - time < window)
      if (recent.length === 0) {
        requests.delete(key)
      } else {
        requests.set(key, recent)
      }
    }
    lastCleanup = now
  }
  
  if (!requests.has(ip)) {
    requests.set(ip, [])
  }
  
  const userRequests = requests.get(ip).filter(time => now - time < window)
  
  if (userRequests.length >= limit) {
    return Candy.abort(429, 'Too many requests')
  }
  
  userRequests.push(now)
  requests.set(ip, userRequests)
}
```

> **Note:** This example uses in-memory storage for simplicity. For production environments with multiple server instances, consider using Redis or Memcached for distributed rate limiting.

#### Premium Check with View
```javascript
// middleware/premium.js
module.exports = async (Candy) => {
  const user = await Candy.Auth.user()
  
  if (!user.isPremium) {
    return Candy.View.render('premium/upgrade', {
      user: user,
      currentPage: Candy.Request.url
    })
  }
}
```

#### Logging
```javascript
// middleware/logger.js
module.exports = async (Candy) => {
  console.log(`${Candy.Request.method} ${Candy.Request.url}`)
}
```

#### Inline Middleware
```javascript
Candy.Route
  .use(async (Candy) => {
    console.log('Custom middleware')
  })
  .page('/special', 'special')
```

### Complete Example

```javascript
// route/www.js

// Public routes
Candy.Route.page('/', 'index')
Candy.Route.page('/about', 'about')

// API routes with CORS
Candy.Route
  .use('cors', 'rateLimit')
  .get('/api/public', 'api.public')
  .post('/api/contact', 'api.contact')

// User routes (requires login)
Candy.Route.auth
  .page('/profile', 'profile')
  .page('/settings', 'settings')
  .post('/api/update', 'update')

// Admin routes (requires login + admin role)
Candy.Route.auth
  .use('admin')
  .page('/admin', 'admin.index')
  .page('/admin/users', 'admin.users')
  .post('/admin/delete', 'admin.delete')
```

### Hot Reloading

Middleware files are automatically reloaded when changed (every 5 seconds), just like controllers and routes. No server restart needed during development.
