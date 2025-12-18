## 🔁 Loops and Iteration

Loops allow you to repeat content for each item in an array or object. This is essential for displaying lists, tables, and collections.

### For Loop

The most common way to iterate over arrays and objects:

```html
<odac:for in="users" key="index" value="user">
  <div class="user-card">
    <h3><odac var="user.name" /></h3>
    <p><odac var="user.email" /></p>
  </div>
</odac:for>
```

**Parameters:**
- `in`: The array or object to loop through (required)
- `key`: Variable name for the index/key (optional, default: "key")
- `value`: Variable name for the value (optional, default: "value")

### Iterating Over Arrays

```javascript
// Controller
Odac.set('products', [
  { name: 'Laptop', price: 999 },
  { name: 'Mouse', price: 29 },
  { name: 'Keyboard', price: 79 }
])
```

```html
<!-- View -->
<div class="products">
  <odac:for in="products" key="index" value="product">
    <div class="product">
      <span class="number"><odac var="index + 1" />.</span>
      <h3><odac var="product.name" /></h3>
      <p>$<odac var="product.price" /></p>
    </div>
  </odac:for>
</div>
```

### Iterating Over Objects

```javascript
// Controller
Odac.set('settings', {
  theme: 'dark',
  language: 'en',
  notifications: true
})
```

```html
<!-- View -->
<table>
  <odac:for in="settings" key="settingKey" value="settingValue">
    <tr>
      <td><odac var="settingKey" /></td>
      <td><odac var="settingValue" /></td>
    </tr>
  </odac:for>
</table>
```

### While Loop

Use while loops for conditional iteration:

```html
<script:odac>
  let counter = 0;
</script:odac>

<odac:while condition="counter < 5">
  <p>Item <odac var="counter + 1" /></p>
  <script:odac>counter++;</script:odac>
</odac:while>
```

**Note:** Be careful with while loops to avoid infinite loops. The condition must eventually become false.

### Loop Control Statements

#### Break

Exit the loop early:

```html
<odac:for in="products" value="product">
  <odac:if condition="product.stock === 0">
    <p class="notice">Some products are out of stock</p>
    <odac:break />
  </odac:if>
  <div><odac var="product.name" /></div>
</odac:for>
```

#### Continue

Skip to the next iteration:

```html
<odac:for in="users" value="user">
  <odac:if condition="user.isBlocked">
    <odac:continue />
  </odac:if>
  
  <div class="user">
    <h3><odac var="user.name" /></h3>
    <p><odac var="user.email" /></p>
  </div>
</odac:for>
```

### Practical Examples

#### Product List with Numbering

```html
<div class="product-list">
  <h2>Our Products</h2>
  
  <odac:for in="products" key="i" value="product">
    <div class="product-item">
      <span class="number">#<odac var="i + 1" /></span>
      <img src="<odac var="product.image" />" alt="<odac var="product.name" />">
      <h3><odac var="product.name" /></h3>
      <p class="price">$<odac var="product.price" /></p>
      
      <odac:if condition="product.discount">
        <span class="discount">-<odac var="product.discount" />%</span>
      </odac:if>
    </div>
  </odac:for>
</div>
```

#### Table with Data

```html
<table class="users-table">
  <thead>
    <tr>
      <th>#</th>
      <th>Name</th>
      <th>Email</th>
      <th>Role</th>
      <th>Status</th>
    </tr>
  </thead>
  <tbody>
    <odac:for in="users" key="index" value="user">
      <tr>
        <td><odac var="index + 1" /></td>
        <td><odac var="user.name" /></td>
        <td><odac var="user.email" /></td>
        <td><odac var="user.role" /></td>
        <td>
          <odac:if condition="user.isActive">
            <span class="badge success">Active</span>
          <odac:else>
            <span class="badge danger">Inactive</span>
          </odac:if>
        </td>
      </tr>
    </odac:for>
  </tbody>
</table>
```

#### Nested Loops

```html
<div class="categories">
  <odac:for in="categories" value="category">
    <div class="category">
      <h2><odac var="category.name" /></h2>
      
      <div class="products">
        <odac:for in="category.products" value="product">
          <div class="product">
            <h3><odac var="product.name" /></h3>
            <p>$<odac var="product.price" /></p>
          </div>
        </odac:for>
      </div>
    </div>
  </odac:for>
</div>
```

#### Grid Layout

```html
<div class="grid">
  <odac:for in="items" key="i" value="item">
    <div class="grid-item">
      <img src="<odac var="item.image" />" alt="<odac var="item.title" />">
      <h3><odac var="item.title" /></h3>
      <p><odac var="item.description" /></p>
      
      <!-- Add row break every 3 items -->
      <odac:if condition="(i + 1) % 3 === 0">
        <div class="row-break"></div>
      </odac:if>
    </div>
  </odac:for>
</div>
```

#### Filtering with Continue

```html
<div class="active-users">
  <h2>Active Users</h2>
  
  <odac:for in="users" value="user">
    <!-- Skip inactive users -->
    <odac:if condition="!user.isActive">
      <odac:continue />
    </odac:if>
    
    <!-- Skip blocked users -->
    <odac:if condition="user.isBlocked">
      <odac:continue />
    </odac:if>
    
    <div class="user-card">
      <h3><odac var="user.name" /></h3>
      <p><odac var="user.email" /></p>
    </div>
  </odac:for>
</div>
```

#### Empty State Handling

```html
<div class="products-section">
  <h2>Products</h2>
  
  <odac:if condition="products && products.length > 0">
    <div class="products-grid">
      <odac:for in="products" value="product">
        <div class="product-card">
          <h3><odac var="product.name" /></h3>
          <p>$<odac var="product.price" /></p>
        </div>
      </odac:for>
    </div>
  <odac:else>
    <div class="empty-state">
      <p>No products found.</p>
      <a href="/products/add">Add your first product</a>
    </div>
  </odac:if>
</div>
```

#### Alternating Row Colors

```html
<table>
  <odac:for in="items" key="i" value="item">
    <tr class="<odac:if condition="i % 2 === 0">even<odac:else>odd</odac:if>">
      <td><odac var="item.name" /></td>
      <td><odac var="item.value" /></td>
    </tr>
  </odac:for>
</table>
```

#### Limited Results with Break

```html
<div class="top-products">
  <h2>Top 5 Products</h2>
  
  <script:odac>
    let count = 0;
  </script:odac>
  
  <odac:for in="products" value="product">
    <odac:if condition="count >= 5">
      <odac:break />
    </odac:if>
    
    <div class="product">
      <h3><odac var="product.name" /></h3>
      <p>$<odac var="product.price" /></p>
    </div>
    
    <script:odac>count++;</script:odac>
  </odac:for>
</div>
```

#### Pagination with While

```html
<script:odac>
  const itemsPerPage = 10;
  const currentPage = parseInt(Odac.Request.get('page')) || 1;
  const startIndex = (currentPage - 1) * itemsPerPage;
  const endIndex = startIndex + itemsPerPage;
  let index = startIndex;
</script:odac>

<div class="items">
  <odac:while condition="index < endIndex && index < items.length">
    <div class="item">
      <odac var="items[index].name" />
    </div>
    <script:odac>index++;</script:odac>
  </odac:while>
</div>
```

### Best Practices

1. **Check for existence**: Always verify the array/object exists before looping
2. **Use meaningful names**: Choose descriptive variable names for keys and values
3. **Avoid complex logic**: Keep loop bodies simple, move complex logic to controllers
4. **Handle empty states**: Always provide feedback when there are no items
5. **Be careful with while**: Ensure while loops will eventually terminate

**Good:**
```javascript
// Controller - prepare data
Odac.set('activeUsers', users.filter(u => u.isActive))
```

```html
<!-- View - simple loop -->
<odac:for in="activeUsers" value="user">
  <div><odac var="user.name" /></div>
</odac:for>
```

**Avoid:**
```html
<!-- Too complex for a view -->
<odac:for in="users" value="user">
  <odac:if condition="user.isActive && !user.isBlocked && user.role !== 'guest'">
    <div><odac var="user.name" /></div>
  </odac:if>
</odac:for>
```
