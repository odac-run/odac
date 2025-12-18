## 💻 Backend JavaScript (Server-Side Execution)

Backend JavaScript allows you to execute JavaScript code during template rendering on the server. This code runs **before** the HTML is sent to the browser.

### Basic Usage

```html
<script:odac>
  // This runs on the SERVER during template rendering
  let total = 0;
  for (let item of cart) {
    total += item.price * item.quantity;
  }
</script:odac>

<p>Total: $<odac var="total" /></p>
```

### Key Characteristics

- ✅ Runs on the **server** during template rendering
- ✅ Has access to all backend variables and Odac object
- ✅ Perfect for calculations, data manipulation, filtering
- ✅ Full IDE syntax highlighting and autocomplete
- ❌ Does NOT run in the browser
- ❌ Cannot access browser APIs (window, document, localStorage, etc.)

### When to Use Backend JavaScript

Use backend JavaScript for:
- **Calculations**: Totals, averages, statistics
- **Data transformation**: Filtering, sorting, mapping arrays
- **Complex logic**: Logic that's too complex for inline conditions
- **Variable preparation**: Creating temporary variables for display

### Accessing Variables

You have access to all variables set in the controller:

```javascript
// Controller
module.exports = async function(Odac) {
  Odac.set('products', [
    { name: 'Laptop', price: 999, quantity: 2 },
    { name: 'Mouse', price: 29, quantity: 5 }
  ])
  
  Odac.View.skeleton('main').set('content', 'cart')
}
```

```html
<!-- View -->
<script:odac>
  let total = 0;
  let itemCount = 0;
  
  for (let product of products) {
    total += product.price * product.quantity;
    itemCount += product.quantity;
  }
  
  const avgPrice = total / itemCount;
</script:odac>

<div class="cart-summary">
  <p>Total Items: <odac var="itemCount" /></p>
  <p>Total Price: $<odac var="total" /></p>
  <p>Average Price: $<odac var="avgPrice.toFixed(2)" /></p>
</div>
```

### Accessing the Odac Object

Full access to the Odac object and all its methods:

```html
<script:odac>
  const isLoggedIn = Odac.Auth.check();
  const currentUser = isLoggedIn ? Odac.Auth.user() : null;
  const requestMethod = Odac.Request.method;
  const currentUrl = Odac.Request.url;
</script:odac>

<odac:if condition="isLoggedIn">
  <p>Welcome, <odac var="currentUser.name" />!</p>
</odac:if>
```

### Practical Examples

#### Shopping Cart Calculations

```html
<script:odac>
  let subtotal = 0;
  let totalItems = 0;
  
  for (let item of cart) {
    subtotal += item.price * item.quantity;
    totalItems += item.quantity;
  }
  
  const tax = subtotal * 0.18; // 18% tax
  const shipping = subtotal > 100 ? 0 : 10;
  const total = subtotal + tax + shipping;
</script:odac>

<div class="cart-summary">
  <h3>Order Summary</h3>
  <p>Items (<odac var="totalItems" />): $<odac var="subtotal.toFixed(2)" /></p>
  <p>Tax (18%): $<odac var="tax.toFixed(2)" /></p>
  <p>Shipping: 
    <odac:if condition="shipping === 0">
      <span class="free">FREE</span>
    <odac:else>
      $<odac var="shipping.toFixed(2)" />
    </odac:if>
  </p>
  <hr>
  <p class="total">Total: $<odac var="total.toFixed(2)" /></p>
</div>
```

#### Filtering and Sorting

```html
<script:odac>
  // Filter active products
  const activeProducts = products.filter(p => p.isActive && p.stock > 0);
  
  // Sort by price
  activeProducts.sort((a, b) => a.price - b.price);
  
  // Get featured products
  const featured = activeProducts.filter(p => p.featured).slice(0, 3);
  
  // Calculate statistics
  const avgPrice = activeProducts.reduce((sum, p) => sum + p.price, 0) / activeProducts.length;
  const maxPrice = Math.max(...activeProducts.map(p => p.price));
  const minPrice = Math.min(...activeProducts.map(p => p.price));
</script:odac>

<div class="products-section">
  <h2>Featured Products</h2>
  <p>Showing <odac var="featured.length" /> of <odac var="activeProducts.length" /> products</p>
  <p>Price range: $<odac var="minPrice" /> - $<odac var="maxPrice" /></p>
  
  <odac:for in="featured" value="product">
    <div class="product">
      <h3><odac var="product.name" /></h3>
      <p>$<odac var="product.price" /></p>
      
      <odac:if condition="product.price < avgPrice">
        <span class="badge">Great Deal!</span>
      </odac:if>
    </div>
  </odac:for>
</div>
```

#### Date and Time Formatting

```html
<script:odac>
  const now = new Date();
  const postDate = new Date(post.createdAt);
  
  // Calculate time difference
  const diffMs = now - postDate;
  const diffDays = Math.floor(diffMs / (1000 * 60 * 60 * 24));
  const diffHours = Math.floor(diffMs / (1000 * 60 * 60));
  const diffMinutes = Math.floor(diffMs / (1000 * 60));
  
  let timeAgo;
  if (diffDays > 0) {
    timeAgo = diffDays + ' days ago';
  } else if (diffHours > 0) {
    timeAgo = diffHours + ' hours ago';
  } else if (diffMinutes > 0) {
    timeAgo = diffMinutes + ' minutes ago';
  } else {
    timeAgo = 'Just now';
  }
  
  const formattedDate = postDate.toLocaleDateString('en-US', {
    year: 'numeric',
    month: 'long',
    day: 'numeric'
  });
</script:odac>

<div class="post">
  <h2><odac var="post.title" /></h2>
  <p class="meta">
    Posted <odac var="timeAgo" /> (<odac var="formattedDate" />)
  </p>
</div>
```

#### Grouping Data

```html
<script:odac>
  // Group products by category
  const grouped = {};
  for (let product of products) {
    if (!grouped[product.category]) {
      grouped[product.category] = [];
    }
    grouped[product.category].push(product);
  }
  
  // Sort categories
  const categories = Object.keys(grouped).sort();
</script:odac>

<div class="products-by-category">
  <odac:for in="categories" value="category">
    <div class="category-section">
      <h2><odac var="category" /></h2>
      <p><odac var="grouped[category].length" /> products</p>
      
      <odac:for in="grouped[category]" value="product">
        <div class="product">
          <h3><odac var="product.name" /></h3>
          <p>$<odac var="product.price" /></p>
        </div>
      </odac:for>
    </div>
  </odac:for>
</div>
```

#### Pagination Logic

```html
<script:odac>
  const itemsPerPage = 10;
  const currentPage = parseInt(Odac.Request.get('page')) || 1;
  const totalItems = products.length;
  const totalPages = Math.ceil(totalItems / itemsPerPage);
  
  const startIndex = (currentPage - 1) * itemsPerPage;
  const endIndex = Math.min(startIndex + itemsPerPage, totalItems);
  const currentItems = products.slice(startIndex, endIndex);
  
  const hasPrevious = currentPage > 1;
  const hasNext = currentPage < totalPages;
</script:odac>

<div class="products">
  <odac:for in="currentItems" value="product">
    <div class="product">
      <h3><odac var="product.name" /></h3>
    </div>
  </odac:for>
</div>

<div class="pagination">
  <odac:if condition="hasPrevious">
    <a href="?page=<odac var="currentPage - 1" />">Previous</a>
  </odac:if>
  
  <span>Page <odac var="currentPage" /> of <odac var="totalPages" /></span>
  
  <odac:if condition="hasNext">
    <a href="?page=<odac var="currentPage + 1" />">Next</a>
  </odac:if>
</div>
```

#### Complex Conditional Logic

```html
<script:odac>
  const user = Odac.Auth.check() ? Odac.Auth.user() : null;
  
  const canEdit = user && (
    user.role === 'admin' || 
    user.id === post.authorId
  );
  
  const canDelete = user && user.role === 'admin';
  
  const canComment = user && !user.isBanned && post.commentsEnabled;
  
  const showActions = canEdit || canDelete || canComment;
</script:odac>

<div class="post">
  <h2><odac var="post.title" /></h2>
  <p><odac var="post.content" /></p>
  
  <odac:if condition="showActions">
    <div class="actions">
      <odac:if condition="canEdit">
        <a href="/posts/<odac var="post.id" />/edit">Edit</a>
      </odac:if>
      
      <odac:if condition="canDelete">
        <a href="/posts/<odac var="post.id" />/delete">Delete</a>
      </odac:if>
      
      <odac:if condition="canComment">
        <a href="#comments">Add Comment</a>
      </odac:if>
    </div>
  </odac:if>
</div>
```

### Multiple Script Blocks

You can use multiple `<script:odac>` blocks in the same view:

```html
<script:odac>
  let total = 0;
</script:odac>

<odac:for in="items" value="item">
  <div><odac var="item.name" /></div>
  
  <script:odac>
    total += item.price;
  </script:odac>
</odac:for>

<p>Total: $<odac var="total" /></p>
```

### Comparison with Client-Side JavaScript

**Backend JavaScript (`<script:odac>`):**
```html
<script:odac>
  // Runs on SERVER during rendering
  const total = products.reduce((sum, p) => sum + p.price, 0);
</script:odac>
<p>Total: $<odac var="total" /></p>
```

**Client-Side JavaScript (`<script>`):**
```html
<script>
  // Runs in BROWSER after page loads
  document.addEventListener('DOMContentLoaded', function() {
    console.log('Page loaded');
    
    // Can access browser APIs
    localStorage.setItem('visited', 'true');
    
    // Can manipulate DOM
    document.querySelector('.button').addEventListener('click', function() {
      alert('Clicked!');
    });
  });
</script>
```

### Best Practices

1. **Keep it simple**: Complex logic should be in controllers
2. **Use for calculations**: Perfect for totals, averages, filtering
3. **Avoid heavy operations**: Don't do database queries or API calls
4. **Use meaningful variable names**: Make code self-documenting
5. **Comment when necessary**: Explain complex calculations

**Good:**
```html
<script:odac>
  const discountedPrice = product.price * (1 - product.discount / 100);
  const savings = product.price - discountedPrice;
</script:odac>
```

**Avoid:**
```html
<script:odac>
  // Don't do this - should be in controller
  const users = await Odac.Mysql.query('SELECT * FROM users');
  const apiData = await fetch('https://api.example.com/data');
</script:odac>
```

### Common Use Cases

- ✅ Calculate totals and subtotals
- ✅ Filter and sort arrays
- ✅ Format dates and numbers
- ✅ Group and aggregate data
- ✅ Create temporary display variables
- ✅ Simple conditional logic
- ❌ Database queries (use controller)
- ❌ API calls (use controller)
- ❌ File operations (use controller)
- ❌ Heavy computations (use controller)
