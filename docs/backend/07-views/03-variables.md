## 📦 Variables in Views

Variables allow you to display dynamic data in your templates. Data is passed from controllers to views using `Candy.set()` and displayed using the `<candy var>` tag.

### Passing Data from Controller

Use `Candy.set()` in your controller to pass data to views:

```javascript
// Controller: controller/profile.js
module.exports = async function(Candy) {
  // Set single variable
  Candy.set('username', 'John Doe')
  
  // Set multiple variables at once
  Candy.set({
    user: {
      name: 'John Doe',
      email: 'john@example.com',
      role: 'admin'
    },
    pageTitle: 'User Profile'
  })
  
  Candy.View.skeleton('main').set('content', 'profile')
}
```

### Displaying Variables

#### HTML-Safe Output (Recommended)

```html
<candy var="username" />
<candy var="user.email" />
<candy var="product.price" />
```

This automatically:
- Escapes HTML to prevent XSS attacks
- Converts newlines (`\n`) to `<br>` tags

**Example:**
```javascript
// Controller
Candy.set('message', 'Hello\nWorld')
```

```html
<!-- View -->
<candy var="message" />
<!-- Output: Hello<br>World -->
```

#### Raw HTML Output

When you need to display HTML content without escaping:

```html
<candy var="htmlContent" raw />
<candy var="user.bio" raw />
```

**Security Warning:** Only use `raw` with trusted content. Never use it with user-generated content to prevent XSS attacks.

**Example:**
```javascript
// Controller
Candy.set('content', '<strong>Bold text</strong>')
```

```html
<!-- View -->
<candy var="content" raw />
<!-- Output: <strong>Bold text</strong> -->
```

### Accessing Nested Properties

You can access nested object properties using dot notation:

```javascript
// Controller
Candy.set('user', {
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
<p>Name: <candy var="user.name" /></p>
<p>Email: <candy var="user.profile.email" /></p>
<p>City: <candy var="user.profile.address.city" /></p>
```

### String Literals

Display static text directly:

```html
<candy>Hello World</candy>
<candy>Welcome to our site</candy>
```

This is useful when you want consistent syntax throughout your templates.

### Accessing the Candy Object

You have full access to the `Candy` object within templates:

```html
<!-- Authentication -->
<candy:if condition="Candy.Auth.check()">
  <p>User ID: <candy var="Candy.Auth.user().id" /></p>
  <p>Email: <candy var="Candy.Auth.user().email" /></p>
</candy:if>

<!-- Request Information -->
<p>Method: <candy var="Candy.Request.method" /></p>
<p>URL: <candy var="Candy.Request.url" /></p>
<p>IP: <candy var="Candy.Request.ip" /></p>

<!-- Configuration -->
<candy:if condition="Candy.Config.debug">
  <div class="debug-info">Debug mode enabled</div>
</candy:if>
```

### Practical Examples

#### User Profile Card

```javascript
// Controller: controller/profile.js
module.exports = async function(Candy) {
  // Fetch user from database
  const userId = Candy.Request.get('id')
  const user = await Candy.Mysql.table('users')
    .where('id', userId)
    .first()
  
  // Pass to view
  Candy.set('user', {
    name: user.name,
    email: user.email,
    bio: user.bio,
    isVerified: user.verified
  })
  
  Candy.View.skeleton('main').set('content', 'profile')
}
```

```html
<!-- View: view/content/profile.html -->
<div class="profile-card">
  <h2><candy var="user.name" /></h2>
  <p><candy var="user.email" /></p>
  
  <candy:if condition="user.isVerified">
    <span class="badge">✓ Verified</span>
  </candy:if>
  
  <div class="bio">
    <candy var="user.bio" raw />
  </div>
</div>
```

#### Product Display with Computed Values

```javascript
// Controller: controller/product.js
module.exports = async function(Candy) {
  const productId = Candy.Request.get('id')
  const product = await Candy.Mysql.table('products')
    .where('id', productId)
    .first()
  
  // Compute values in controller
  const hasDiscount = product.discount > 0
  const finalPrice = product.price * (1 - product.discount / 100)
  
  Candy.set({
    product: product,
    hasDiscount: hasDiscount,
    finalPrice: finalPrice
  })
  
  Candy.View.skeleton('main').set('content', 'product')
}
```

```html
<!-- View: view/content/product.html -->
<div class="product">
  <h1><candy var="product.name" /></h1>
  
  <candy:if condition="hasDiscount">
    <p class="original-price">$<candy var="product.price" /></p>
    <p class="final-price">$<candy var="finalPrice" /></p>
    <span class="discount">-<candy var="product.discount" />%</span>
  <candy:else>
    <p class="price">$<candy var="product.price" /></p>
  </candy:if>
  
  <div class="description">
    <candy var="product.description" />
  </div>
</div>
```

#### Working with Arrays

```javascript
// Controller: controller/products.js
module.exports = async function(Candy) {
  const products = await Candy.Mysql.table('products')
    .where('active', true)
    .get()
  
  Candy.set({
    products: products,
    totalProducts: products.length
  })
  
  Candy.View.skeleton('main').set('content', 'products')
}
```

```html
<!-- View: view/content/products.html -->
<h1>Products (<candy var="totalProducts" />)</h1>

<div class="products-grid">
  <candy:for in="products" value="product">
    <div class="product-card">
      <h3><candy var="product.name" /></h3>
      <p>$<candy var="product.price" /></p>
    </div>
  </candy:for>
</div>
```

### Best Practices

1. **Always use Candy.set()**: Pass all data through `Candy.set()` for consistency
2. **Set data before rendering**: All `Candy.set()` calls should come before `Candy.View.set()`
3. **Compute in controller**: Do calculations in the controller, not in views
4. **Use descriptive names**: `pageTitle`, `userProfile` instead of `title`, `data`
5. **Group related data**: Use objects to organize related data

**Good:**
```javascript
// Controller
const user = await Candy.Mysql.table('users').first()
const isAdmin = user.role === 'admin'

Candy.set({
  user: user,
  isAdmin: isAdmin
})
```

**Avoid:**
```html
<!-- Don't do complex logic in views -->
<candy:if condition="user.role === 'admin' && user.verified && !user.banned">
  ...
</candy:if>
```

### Error Handling

Always handle cases where data might not exist:

```javascript
// Controller
module.exports = async function(Candy) {
  const productId = Candy.Request.get('id')
  const product = await Candy.Mysql.table('products')
    .where('id', productId)
    .first()
  
  if (!product) {
    Candy.set('error', 'Product not found')
  } else {
    Candy.set('product', product)
  }
  
  Candy.View.skeleton('main').set('content', 'product')
}
```

```html
<!-- View -->
<candy:if condition="error">
  <div class="alert alert-danger">
    <candy var="error" />
  </div>
<candy:else>
  <div class="product">
    <h1><candy var="product.name" /></h1>
  </div>
</candy:if>
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

**Note:** The new `<candy>` tag syntax is recommended for all new projects as it provides better IDE support and readability.
