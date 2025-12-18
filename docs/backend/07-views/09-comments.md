## 💬 Comments in Views

Odac supports two types of comments in view files: backend comments (not rendered) and regular HTML comments (rendered).

### Backend Comments (Not Rendered)

Backend comments are removed during template rendering and never appear in the HTML output sent to the browser.

#### Single-Line Backend Comments

```html
<!--odac This is a backend comment -->
<p>This will be rendered</p>
```

#### Multi-Line Backend Comments

```html
<!--odac
  This is a multi-line backend comment
  It can span multiple lines
  None of this will appear in the output
odac-->

<div class="content">
  <p>This will be rendered</p>
</div>
```

### Regular HTML Comments (Rendered)

Standard HTML comments are preserved and sent to the browser:

```html
<!-- This is a regular HTML comment -->
<!-- It will appear in the browser's HTML source -->
<p>Content here</p>
```

### When to Use Each Type

#### Use Backend Comments For:

**Development Notes:**
```html
<!--odac TODO: Add pagination here -->
<!--odac FIXME: This needs optimization -->
<!--odac NOTE: This section is for admin users only -->
```

**Sensitive Information:**
```html
<!--odac 
  Database query returns: id, name, email, password_hash
  We only display: name, email
odac-->

<odac:for in="users" value="user">
  <p><odac var="user.name" /> - <odac var="user.email" /></p>
</odac:for>
```

**Debugging Information:**
```html
<!--odac Debug: user object structure -->
<!--odac { id: 1, name: "John", role: "admin" } -->

<odac:if condition="user.role === 'admin'">
  <div class="admin-panel">Admin content</div>
</odac:if>
```

**Temporary Code:**
```html
<!--odac
  Old implementation - keeping for reference
  <div class="old-layout">
    <odac:for in="items" value="item">
      <p><odac var="item.name" /></p>
    </odac:for>
  </div>
odac-->

<div class="new-layout">
  <odac:for in="items" value="item">
    <div class="item-card">
      <h3><odac var="item.name" /></h3>
    </div>
  </odac:for>
</div>
```

#### Use HTML Comments For:

**Section Markers:**
```html
<!-- Header Section -->
<header>
  <nav>...</nav>
</header>

<!-- Main Content -->
<main>
  <article>...</article>
</main>

<!-- Footer Section -->
<footer>
  <p>Copyright 2024</p>
</footer>
```

**Browser-Specific Hacks:**
```html
<!--[if IE]>
  <p>You are using Internet Explorer</p>
<![endif]-->
```

**Third-Party Integration Notes:**
```html
<!-- Google Analytics -->
<script>
  // Analytics code here
</script>

<!-- Facebook Pixel -->
<script>
  // Pixel code here
</script>
```

### Practical Examples

#### Documenting Complex Logic

```html
<!--odac
  This section displays products based on user role:
  - Admin: sees all products including inactive
  - Regular user: sees only active products
  - Guest: sees only featured products
odac-->

<script:odac>
  let visibleProducts;
  
  if (Odac.Auth.check()) {
    const user = Odac.Auth.user();
    if (user.role === 'admin') {
      visibleProducts = products;
    } else {
      visibleProducts = products.filter(p => p.isActive);
    }
  } else {
    visibleProducts = products.filter(p => p.featured);
  }
</script:odac>

<odac:for in="visibleProducts" value="product">
  <div class="product">
    <h3><odac var="product.name" /></h3>
  </div>
</odac:for>
```

#### Marking Sections for Developers

```html
<div class="dashboard">
  <!--odac START: User Statistics Section -->
  <div class="stats">
    <h2>Statistics</h2>
    <p>Total Users: <odac var="stats.totalUsers" /></p>
    <p>Active Users: <odac var="stats.activeUsers" /></p>
  </div>
  <!--odac END: User Statistics Section -->
  
  <!--odac START: Recent Activity Section -->
  <div class="activity">
    <h2>Recent Activity</h2>
    <odac:for in="activities" value="activity">
      <p><odac var="activity.description" /></p>
    </odac:for>
  </div>
  <!--odac END: Recent Activity Section -->
</div>
```

#### Explaining Template Variables

```html
<!--odac
  Available variables from controller:
  - user: Current user object { id, name, email, role }
  - posts: Array of post objects
  - categories: Array of category objects
  - settings: Site settings object
odac-->

<div class="profile">
  <h1><odac var="user.name" /></h1>
  <p><odac var="user.email" /></p>
</div>
```

#### Temporary Disabling Code

```html
<div class="products">
  <odac:for in="products" value="product">
    <div class="product-card">
      <h3><odac var="product.name" /></h3>
      <p>$<odac var="product.price" /></p>
      
      <!--odac Temporarily disabled - waiting for API
      <div class="reviews">
        <odac var="product.averageRating" /> stars
      </div>
      odac-->
    </div>
  </odac:for>
</div>
```

#### Version History

```html
<!--odac
  Version History:
  v1.0 - Initial implementation
  v1.1 - Added sorting functionality
  v1.2 - Added filtering by category
  v2.0 - Complete redesign with new layout
odac-->

<div class="product-list">
  <!-- Product list implementation -->
</div>
```

### Best Practices

1. **Use backend comments for sensitive info**: Never expose internal logic or data structures in HTML comments
2. **Keep comments concise**: Don't over-comment obvious code
3. **Update comments**: Remove or update outdated comments
4. **Use meaningful descriptions**: Make comments helpful for other developers
5. **Don't commit debug comments**: Remove debug comments before committing

**Good:**
```html
<!--odac This query is cached for 5 minutes -->
<odac:for in="products" value="product">
  <div><odac var="product.name" /></div>
</odac:for>
```

**Avoid:**
```html
<!--odac Loop through products -->
<odac:for in="products" value="product">
  <!--odac Display product name -->
  <div><odac var="product.name" /></div>
</odac:for>
```

### Security Considerations

**Never expose sensitive information in HTML comments:**

```html
<!-- BAD: Visible in browser source -->
<!-- Database password: secret123 -->
<!-- API key: abc123xyz -->

<!--odac GOOD: Not visible in output -->
<!--odac Database password: secret123 -->
<!--odac API key: abc123xyz -->
```

**Be careful with user data:**

```html
<!-- BAD: Exposes user data -->
<!-- User ID: 12345, Email: user@example.com -->

<!--odac GOOD: Hidden from output -->
<!--odac User ID: 12345, Email: user@example.com -->
```

### Comment Syntax Summary

| Type | Syntax | Rendered | Use Case |
|------|--------|----------|----------|
| Backend Single-Line | `<!--odac comment -->` | No | Development notes, TODOs |
| Backend Multi-Line | `<!--odac ... odac-->` | No | Detailed explanations, disabled code |
| HTML Comment | `<!-- comment -->` | Yes | Section markers, browser hacks |
