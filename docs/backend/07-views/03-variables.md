## 📦 Variables in Views

Variables allow you to display dynamic data in your templates. Data is passed from controllers to views using `Odac.set()` and displayed using the `<odac var>` tag.

### Passing Data from Controller

Use `Odac.set()` in your controller to pass data to views:

```javascript
// Controller: controller/profile.js
module.exports = async function(Odac) {
  // Set single variable
  Odac.set('username', 'John Doe')
  
  // Set multiple variables at once
  Odac.set({
    user: {
      name: 'John Doe',
      email: 'john@example.com',
      role: 'admin'
    },
    pageTitle: 'User Profile'
  })
  
  Odac.View.skeleton('main').set('content', 'profile')
}
```

### Displaying Variables

#### HTML-Safe Output (Recommended)

```html
<odac var="username" />
<odac var="user.email" />
<odac var="product.price" />
```

This automatically:
- Escapes HTML to prevent XSS attacks
- Converts newlines (`\n`) to `<br>` tags

**Example:**
```javascript
// Controller
Odac.set('message', 'Hello\nWorld')
```

```html
<!-- View -->
<odac var="message" />
<!-- Output: Hello<br>World -->
```

#### Raw HTML Output

When you need to display HTML content without escaping:

```html
<odac var="htmlContent" raw />
<odac var="user.bio" raw />
```

**Security Warning:** Only use `raw` with trusted content. Never use it with user-generated content to prevent XSS attacks.

**Example:**
```javascript
// Controller
Odac.set('content', '<strong>Bold text</strong>')
```

```html
<!-- View -->
<odac var="content" raw />
<!-- Output: <strong>Bold text</strong> -->
```

### Accessing Nested Properties

You can access nested object properties using dot notation:

```javascript
// Controller
Odac.set('user', {
  name: 'John',
  profile: {
    email: 'john@example.com',
    address: {
      city: 'Istanbul'
    }
  }
})
```

```html
<!-- View -->
<p>Name: <odac var="user.name" /></p>
<p>Email: <odac var="user.profile.email" /></p>
<p>City: <odac var="user.profile.address.city" /></p>
```

### String Literals

Display static text directly:

```html
<odac>Hello World</odac>
<odac>Welcome to our site</odac>
```

This is useful when you want consistent syntax throughout your templates.

### Accessing the Odac Object

You have full access to the `Odac` object within templates:

```html
<!-- Authentication -->
<odac:if condition="Odac.Auth.check()">
  <p>User ID: <odac var="Odac.Auth.user().id" /></p>
  <p>Email: <odac var="Odac.Auth.user().email" /></p>
</odac:if>

<!-- Request Information -->
<p>Method: <odac var="Odac.Request.method" /></p>
<p>URL: <odac var="Odac.Request.url" /></p>
<p>IP: <odac var="Odac.Request.ip" /></p>

<!-- Configuration -->
<odac:if condition="Odac.Config.debug">
  <div class="debug-info">Debug mode enabled</div>
</odac:if>
```

### Practical Examples

#### User Profile Card

```javascript
// Controller: controller/profile.js
module.exports = async function(Odac) {
  // Fetch user from database
  const userId = Odac.Request.get('id')
  const user = await Odac.Mysql.table('users')
    .where('id', userId)
    .first()
  
  // Pass to view
  Odac.set('user', {
    name: user.name,
    email: user.email,
    bio: user.bio,
    isVerified: user.verified
  })
  
  Odac.View.skeleton('main').set('content', 'profile')
}
```

```html
<!-- View: view/content/profile.html -->
<div class="profile-card">
  <h2><odac var="user.name" /></h2>
  <p><odac var="user.email" /></p>
  
  <odac:if condition="user.isVerified">
    <span class="badge">✓ Verified</span>
  </odac:if>
  
  <div class="bio">
    <odac var="user.bio" raw />
  </div>
</div>
```

#### Product Display with Computed Values

```javascript
// Controller: controller/product.js
module.exports = async function(Odac) {
  const productId = Odac.Request.get('id')
  const product = await Odac.Mysql.table('products')
    .where('id', productId)
    .first()
  
  // Compute values in controller
  const hasDiscount = product.discount > 0
  const finalPrice = product.price * (1 - product.discount / 100)
  
  Odac.set({
    product: product,
    hasDiscount: hasDiscount,
    finalPrice: finalPrice
  })
  
  Odac.View.skeleton('main').set('content', 'product')
}
```

```html
<!-- View: view/content/product.html -->
<div class="product">
  <h1><odac var="product.name" /></h1>
  
  <odac:if condition="hasDiscount">
    <p class="original-price">$<odac var="product.price" /></p>
    <p class="final-price">$<odac var="finalPrice" /></p>
    <span class="discount">-<odac var="product.discount" />%</span>
  <odac:else>
    <p class="price">$<odac var="product.price" /></p>
  </odac:if>
  
  <div class="description">
    <odac var="product.description" />
  </div>
</div>
```

#### Working with Arrays

```javascript
// Controller: controller/products.js
module.exports = async function(Odac) {
  const products = await Odac.Mysql.table('products')
    .where('active', true)
    .get()
  
  Odac.set({
    products: products,
    totalProducts: products.length
  })
  
  Odac.View.skeleton('main').set('content', 'products')
}
```

```html
<!-- View: view/content/products.html -->
<h1>Products (<odac var="totalProducts" />)</h1>

<div class="products-grid">
  <odac:for in="products" value="product">
    <div class="product-card">
      <h3><odac var="product.name" /></h3>
      <p>$<odac var="product.price" /></p>
    </div>
  </odac:for>
</div>
```

### Best Practices

1. **Always use Odac.set()**: Pass all data through `Odac.set()` for consistency
2. **Set data before rendering**: All `Odac.set()` calls should come before `Odac.View.set()`
3. **Compute in controller**: Do calculations in the controller, not in views
4. **Use descriptive names**: `pageTitle`, `userProfile` instead of `title`, `data`
5. **Group related data**: Use objects to organize related data

**Good:**
```javascript
// Controller
const user = await Odac.Mysql.table('users').first()
const isAdmin = user.role === 'admin'

Odac.set({
  user: user,
  isAdmin: isAdmin
})
```

**Avoid:**
```html
<!-- Don't do complex logic in views -->
<odac:if condition="user.role === 'admin' && user.verified && !user.banned">
  ...
</odac:if>
```

### Error Handling

Always handle cases where data might not exist:

```javascript
// Controller
module.exports = async function(Odac) {
  const productId = Odac.Request.get('id')
  const product = await Odac.Mysql.table('products')
    .where('id', productId)
    .first()
  
  if (!product) {
    Odac.set('error', 'Product not found')
  } else {
    Odac.set('product', product)
  }
  
  Odac.View.skeleton('main').set('content', 'product')
}
```

```html
<!-- View -->
<odac:if condition="error">
  <div class="alert alert-danger">
    <odac var="error" />
  </div>
<odac:else>
  <div class="product">
    <h1><odac var="product.name" /></h1>
  </div>
</odac:if>
```

### Legacy Syntax (Backward Compatibility)

Odac also supports legacy syntax:

```html
<!-- HTML-safe output -->
{{ username }}
{{ user.email }}

<!-- Raw HTML output -->
{!! htmlContent !!}
{!! user.bio !!}
```

**Note:** The new `<odac>` tag syntax is recommended for all new projects as it provides better IDE support and readability.
