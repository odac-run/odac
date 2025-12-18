## 📥 The Request Object

The `Odac.Request` object contains information about the user's incoming request.

### Getting Request Parameters

#### Using Odac.request() (Recommended)

The easiest way to get request parameters is using `Odac.request()`:

```javascript
module.exports = async function (Odac) {
  // Get parameter from GET or POST automatically
  const userName = await Odac.request('name')
  const userId = await Odac.request('id')
  
  return `Hello ${userName}!`
}
```

**Specify Method (Optional):**

```javascript
module.exports = async function (Odac) {
  // Get from GET parameters only
  const searchQuery = await Odac.request('q', 'GET')
  
  // Get from POST parameters only
  const formName = await Odac.request('name', 'POST')
  
  return `Searching for: ${searchQuery}`
}
```

#### Direct Access

You can also access request data directly:

```javascript
module.exports = function (Odac) {
  // GET parameters (URL query string like ?id=123)
  const userId = Odac.Request.get('id')
  
  // POST parameters (form data)
  const userName = Odac.Request.post('name')
  
  return `User: ${userName}`
}
```

### Request Properties

*   `Odac.Request.method` - HTTP method ('GET', 'POST', etc.)
*   `Odac.Request.url` - Full URL the user visited
*   `Odac.Request.host` - Website's hostname
*   `Odac.Request.ip` - User's IP address
*   `Odac.Request.ssl` - Whether connection is SSL/HTTPS

### Request Headers

```javascript
module.exports = function (Odac) {
  const userAgent = Odac.Request.header('user-agent')
  const contentType = Odac.Request.header('content-type')
  
  return `Browser: ${userAgent}`
}
```

### Complete Example

```javascript
module.exports = async function (Odac) {
  // Get request parameters
  const productId = await Odac.request('id')
  const quantity = await Odac.request('quantity') || 1
  
  // Check request method
  if (Odac.Request.method === 'POST') {
    // Handle form submission
    const result = await processOrder(productId, quantity)
    return { success: true, orderId: result.id }
  }
  
  // Show product page
  Odac.set({
    productId: productId,
    quantity: quantity
  })
  
  Odac.View.set({
    skeleton: 'main',
    content: 'product.detail'
  })
}
```
