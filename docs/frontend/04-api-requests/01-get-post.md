# API Requests with odac.js

Learn how to make GET and POST requests to your API endpoints using odac.js.

## GET Requests

### Basic GET Request

```javascript
odac.get('/api/data', function(response) {
  console.log('Data:', response)
})
```

### With Parameters

```javascript
const userId = 123
odac.get(`/api/users/${userId}`, function(user) {
  console.log('User:', user.name)
  console.log('Email:', user.email)
})
```

### Error Handling

```javascript
odac.get('/api/data', function(response) {
  if (response.error) {
    console.error('Error:', response.error)
    return
  }
  
  // Process data
  console.log('Success:', response)
})
```

## POST Requests

### Using Forms

The recommended way to make POST requests is using `Odac.form()`:

```javascript
odac.form('#my-form', function(data) {
  if (data.result.success) {
    console.log('Success!')
  }
})
```

See [Form Handling](../03-forms/01-form-handling.md) for details.

### Manual POST (Advanced)

For custom POST requests without forms, use the internal AJAX method:

```javascript
// Note: This is an advanced pattern
// For most cases, use odac.form() instead

const formData = new FormData()
formData.append('name', 'John')
formData.append('email', 'john@example.com')
formData.append('_token', odac.token())

fetch('/api/submit', {
  method: 'POST',
  body: formData
})
.then(response => response.json())
.then(data => {
  console.log('Success:', data)
})
```

## CSRF Protection

### Automatic Token Management

odac.js automatically handles CSRF tokens:

```javascript
// Token is automatically included
odac.get('/api/data', function(response) {
  // ...
})

// Token is automatically included in forms
odac.form('#my-form', function(data) {
  // ...
})
```

### Manual Token Access

If you need the token manually:

```javascript
const token = odac.token()
console.log('Current token:', token)
```

## Common Patterns

### Fetching Data

```javascript
// Get user profile
odac.get('/api/profile', function(profile) {
  document.querySelector('#username').textContent = profile.name
  document.querySelector('#email').textContent = profile.email
})
```

### Loading List Data

```javascript
// Get products
odac.get('/api/products', function(products) {
  const container = document.querySelector('#products')
  
  products.forEach(product => {
    const html = `
      <div class="product">
        <h3>${product.name}</h3>
        <p>${product.price}</p>
      </div>
    `
    container.insertAdjacentHTML('beforeend', html)
  })
})
```

### Search

```javascript
const searchInput = document.querySelector('#search')

searchInput.addEventListener('input', function() {
  const query = this.value
  
  if (query.length < 3) return
  
  odac.get(`/api/search?q=${encodeURIComponent(query)}`, function(results) {
    displayResults(results)
  })
})

function displayResults(results) {
  const container = document.querySelector('#results')
  container.innerHTML = ''
  
  results.forEach(result => {
    const html = `<div class="result">${result.title}</div>`
    container.insertAdjacentHTML('beforeend', html)
  })
}
```

### Autocomplete

```javascript
let debounceTimer

document.querySelector('#autocomplete').addEventListener('input', function() {
  clearTimeout(debounceTimer)
  
  const value = this.value
  
  debounceTimer = setTimeout(() => {
    if (value.length < 2) return
    
    odac.get(`/api/autocomplete?q=${value}`, function(suggestions) {
      showSuggestions(suggestions)
    })
  }, 300)
})
```

### Infinite Scroll

```javascript
let page = 1
let loading = false

window.addEventListener('scroll', function() {
  if (loading) return
  
  const scrollPosition = window.innerHeight + window.scrollY
  const threshold = document.body.offsetHeight - 500
  
  if (scrollPosition >= threshold) {
    loadMore()
  }
})

function loadMore() {
  loading = true
  page++
  
  odac.get(`/api/posts?page=${page}`, function(posts) {
    posts.forEach(post => {
      appendPost(post)
    })
    loading = false
  })
}
```

### Real-time Updates

```javascript
// Poll for updates every 30 seconds
setInterval(function() {
  odac.get('/api/notifications', function(notifications) {
    updateNotificationBadge(notifications.count)
  })
}, 30000)
```

## Server-Side Setup

### GET Endpoint

```javascript
// controller/get/users.js
module.exports = function(Odac) {
  // Get all users
  const users = [
    {id: 1, name: 'John', email: 'john@example.com'},
    {id: 2, name: 'Jane', email: 'jane@example.com'}
  ]
  
  return users
}
```

```javascript
// route/www.js
odac.Route.get('/api/users', 'users')
```

### GET with Parameters

```javascript
// controller/get/user.js
module.exports = async function(Odac) {
  const userId = odac.Request.data.url.id
  
  // Fetch user from database
  const user = await getUserById(userId)
  
  if (!user) {
    odac.Request.status(404)
    return {error: 'User not found'}
  }
  
  return user
}
```

```javascript
// route/www.js
odac.Route.get('/api/users/{id}', 'user')
```

### POST Endpoint

```javascript
// controller/post/create.js
module.exports = async function(Odac) {
  const name = await odac.Request.request('name')
  const email = await odac.Request.request('email')
  
  // Validation
  if (!name || !email) {
    return {
      result: {success: false},
      errors: {
        name: !name ? 'Name is required' : null,
        email: !email ? 'Email is required' : null
      }
    }
  }
  
  // Create user
  const user = await createUser({name, email})
  
  return {
    result: {
      success: true,
      message: 'User created successfully'
    },
    user: user
  }
}
```

```javascript
// route/www.js
odac.Route.post('/api/users/create', 'create')
```

## Response Format

### Success Response

```json
{
  "result": {
    "success": true,
    "message": "Operation successful"
  },
  "data": {
    "id": 123,
    "name": "John"
  }
}
```

### Error Response

```json
{
  "result": {
    "success": false,
    "message": "Validation failed"
  },
  "errors": {
    "email": "Email is required",
    "password": "Password must be at least 8 characters"
  }
}
```

## Best Practices

1. **Use Appropriate Methods**: GET for reading, POST for writing
2. **Validate Input**: Always validate on the server
3. **Handle Errors**: Provide meaningful error messages
4. **Use HTTPS**: Always use HTTPS for sensitive data
5. **Rate Limiting**: Implement rate limiting on the server
6. **Debounce Requests**: Debounce rapid requests (search, autocomplete)

## Advanced Techniques

### Request Caching

```javascript
const cache = {}

function getCached(url, callback) {
  if (cache[url]) {
    callback(cache[url])
    return
  }
  
  odac.get(url, function(data) {
    cache[url] = data
    callback(data)
  })
}

// Usage
getCached('/api/data', function(data) {
  console.log('Data:', data)
})
```

### Request Queue

```javascript
const requestQueue = []
let processing = false

function queueRequest(url, callback) {
  requestQueue.push({url, callback})
  processQueue()
}

function processQueue() {
  if (processing || requestQueue.length === 0) return
  
  processing = true
  const {url, callback} = requestQueue.shift()
  
  odac.get(url, function(data) {
    callback(data)
    processing = false
    processQueue()
  })
}
```

### Retry Logic

```javascript
function getWithRetry(url, callback, maxRetries = 3) {
  let attempts = 0
  
  function attempt() {
    attempts++
    
    odac.get(url, function(data) {
      if (data.error && attempts < maxRetries) {
        setTimeout(attempt, 1000 * attempts)
      } else {
        callback(data)
      }
    })
  }
  
  attempt()
}
```

## Troubleshooting

### Request Not Working

- Check the URL is correct
- Verify the endpoint exists in your routes
- Check browser console for errors
- Ensure CSRF token is valid

### CORS Errors

- CORS is not an issue for same-origin requests
- For cross-origin requests, configure server CORS headers

### Token Errors

- Tokens are managed automatically
- If you get token errors, check server configuration
- Ensure cookies are enabled

## Next Steps

- Learn about [Form Handling](../03-forms/01-form-handling.md)
- Explore [AJAX Navigation](../02-ajax-navigation/01-quick-start.md)
- Check [odac.js Overview](../01-overview/01-introduction.md)
