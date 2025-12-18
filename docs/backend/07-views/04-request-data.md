## 🔗 Request Data (Query Parameters)

The `<odac get>` tag allows you to access URL query parameters directly in your views. This is useful for forms, filters, and pagination.

### Getting Query Parameters

Use `<odac get="key" />` to access URL query parameters:

**Important:** `<odac get>` is for **query parameters** (URL parameters), not for data from controllers. For controller data, use `<odac var>` (see [Variables](./03-variables.md)).

```html
<!-- URL: /search?q=laptop&page=2 -->

<p>Search query: <odac get="q" /></p>
<p>Current page: <odac get="page" /></p>
```

**How it works:**
1. User visits `/search?q=laptop&page=2`
2. `<odac get="q" />` retrieves the value of `q` parameter
3. If parameter doesn't exist, it returns empty string (no error)

### Undefined Parameters

If a parameter doesn't exist, it safely returns an empty string:

```html
<!-- URL: /products (no query parameters) -->

<odac get="search" />
<!-- Output: (empty string, no error) -->
```

This prevents errors when parameters are optional.

### Difference: get vs var

**`<odac get>` - Query Parameters (from URL):**
```html
<!-- URL: /search?q=laptop -->
<odac get="q" />
<!-- Output: laptop -->
```

**`<odac var>` - Controller Data (from Odac.set()):**
```javascript
// Controller
Odac.set('productName', 'Laptop')
```
```html
<!-- View -->
<odac var="productName" />
<!-- Output: Laptop -->
```

### Processing Request Data in Controllers

While you can access query parameters directly in views with `<odac get>`, it's often better to process them in the controller:

```javascript
// Controller: controller/search.js
module.exports = async function(Odac) {
  // Get query parameters
  const query = Odac.Request.get('q') || 'all products'
  const page = parseInt(Odac.Request.get('page')) || 1
  
  // Validate and process
  const validatedQuery = query.trim()
  const validatedPage = Math.max(1, page)
  
  // Fetch results
  const results = await Odac.Mysql.table('products')
    .where('name', 'like', `%${validatedQuery}%`)
    .limit(20)
    .offset((validatedPage - 1) * 20)
    .get()
  
  // Pass processed data to view
  Odac.set({
    query: validatedQuery,
    page: validatedPage,
    results: results
  })
  
  Odac.View.skeleton('main').set('content', 'search')
}
```

```html
<!-- View: view/content/search.html -->
<h1>Search Results for "<odac var="query" />"</h1>
<p>Page <odac var="page" /></p>

<odac:for in="results" value="product">
  <div class="product">
    <h3><odac var="product.name" /></h3>
    <p><odac var="product.price" /></p>
  </div>
</odac:for>
```

### Accessing Request Object

You can access the full Request object through the Odac object:

```html
<!-- Request method -->
<p>Method: <odac var="Odac.Request.method" /></p>

<!-- Current URL -->
<p>URL: <odac var="Odac.Request.url" /></p>

<!-- Client IP -->
<p>IP: <odac var="Odac.Request.ip" /></p>

<!-- User agent -->
<p>Browser: <odac var="Odac.Request.headers['user-agent']" /></p>
```

### Practical Examples

#### Search Form with Results

```html
<!-- Search form -->
<form action="/search" method="GET">
  <input 
    type="text" 
    name="q" 
    value="<odac get="q" />" 
    placeholder="Search products..."
  >
  <button type="submit">Search</button>
</form>

<!-- Display search query if exists -->
<odac:if condition="Odac.Request.get('q')">
  <p>Showing results for: "<odac get="q" />"</p>
</odac:if>
```

#### Pagination

```html
<script:odac>
  const currentPage = parseInt(Odac.Request.get('page')) || 1
  const totalPages = 10
</script:odac>

<div class="pagination">
  <odac:if condition="currentPage > 1">
    <a href="?page=<odac var="currentPage - 1" />">Previous</a>
  </odac:if>
  
  <span>Page <odac var="currentPage" /> of <odac var="totalPages" /></span>
  
  <odac:if condition="currentPage < totalPages">
    <a href="?page=<odac var="currentPage + 1" />">Next</a>
  </odac:if>
</div>
```

#### Filter Form

```html
<!-- URL: /products?category=electronics&sort=price&order=asc -->

<form action="/products" method="GET">
  <select name="category">
    <option value="">All Categories</option>
    <option value="electronics" <odac:if condition="Odac.Request.get('category') === 'electronics'">selected</odac:if>>
      Electronics
    </option>
    <option value="clothing" <odac:if condition="Odac.Request.get('category') === 'clothing'">selected</odac:if>>
      Clothing
    </option>
  </select>
  
  <select name="sort">
    <option value="name" <odac:if condition="Odac.Request.get('sort') === 'name'">selected</odac:if>>
      Name
    </option>
    <option value="price" <odac:if condition="Odac.Request.get('sort') === 'price'">selected</odac:if>>
      Price
    </option>
  </select>
  
  <button type="submit">Filter</button>
</form>
```

#### Active Navigation

```html
<nav>
  <a href="/" class="<odac:if condition="Odac.Request.url === '/'">active</odac:if>">
    Home
  </a>
  <a href="/products" class="<odac:if condition="Odac.Request.url.startsWith('/products')">active</odac:if>">
    Products
  </a>
  <a href="/about" class="<odac:if condition="Odac.Request.url === '/about'">active</odac:if>">
    About
  </a>
</nav>
```

### Best Practices

1. **Validate in Controller**: Always validate and sanitize request data in the controller before using it
2. **Default Values**: Provide default values for optional parameters
3. **Type Conversion**: Convert string parameters to appropriate types (numbers, booleans)
4. **Security**: Never trust user input - always validate and escape

**Good:**
```javascript
// Controller
const page = Math.max(1, parseInt(Odac.Request.get('page')) || 1)
const limit = Math.min(100, parseInt(Odac.Request.get('limit')) || 20)

Odac.set('page', page)
Odac.set('limit', limit)
```

**Avoid:**
```html
<!-- Don't do complex logic in views -->
<odac:if condition="parseInt(Odac.Request.get('page')) > 0 && parseInt(Odac.Request.get('page')) < 100">
  ...
</odac:if>
```
