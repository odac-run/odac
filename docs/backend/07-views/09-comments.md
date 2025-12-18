## 💬 Comments in Views

Odac supports two types of comments in view files: backend comments (not rendered) and regular HTML comments (rendered).

### Backend Comments (Not Rendered)

Backend comments are removed during template rendering and never appear in the HTML output sent to the browser.

#### Single-Line Backend Comments

```html
<!--candy This is a backend comment -->
<p>This will be rendered</p>
```

#### Multi-Line Backend Comments

```html
<!--candy
  This is a multi-line backend comment
  It can span multiple lines
  None of this will appear in the output
candy-->

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
<!--candy TODO: Add pagination here -->
<!--candy FIXME: This needs optimization -->
<!--candy NOTE: This section is for admin users only -->
```

**Sensitive Information:**
```html
<!--candy 
  Database query returns: id, name, email, password_hash
  We only display: name, email
candy-->

<candy:for in="users" value="user">
  <p><candy var="user.name" /> - <candy var="user.email" /></p>
</candy:for>
```

**Debugging Information:**
```html
<!--candy Debug: user object structure -->
<!--candy { id: 1, name: "John", role: "admin" } -->

<candy:if condition="user.role === 'admin'">
  <div class="admin-panel">Admin content</div>
</candy:if>
```

**Temporary Code:**
```html
<!--candy
  Old implementation - keeping for reference
  <div class="old-layout">
    <candy:for in="items" value="item">
      <p><candy var="item.name" /></p>
    </candy:for>
  </div>
candy-->

<div class="new-layout">
  <candy:for in="items" value="item">
    <div class="item-card">
      <h3><candy var="item.name" /></h3>
    </div>
  </candy:for>
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
<!--candy
  This section displays products based on user role:
  - Admin: sees all products including inactive
  - Regular user: sees only active products
  - Guest: sees only featured products
candy-->

<script:candy>
  let visibleProducts;
  
  if (Candy.Auth.check()) {
    const user = Candy.Auth.user();
    if (user.role === 'admin') {
      visibleProducts = products;
    } else {
      visibleProducts = products.filter(p => p.isActive);
    }
  } else {
    visibleProducts = products.filter(p => p.featured);
  }
</script:candy>

<candy:for in="visibleProducts" value="product">
  <div class="product">
    <h3><candy var="product.name" /></h3>
  </div>
</candy:for>
```

#### Marking Sections for Developers

```html
<div class="dashboard">
  <!--candy START: User Statistics Section -->
  <div class="stats">
    <h2>Statistics</h2>
    <p>Total Users: <candy var="stats.totalUsers" /></p>
    <p>Active Users: <candy var="stats.activeUsers" /></p>
  </div>
  <!--candy END: User Statistics Section -->
  
  <!--candy START: Recent Activity Section -->
  <div class="activity">
    <h2>Recent Activity</h2>
    <candy:for in="activities" value="activity">
      <p><candy var="activity.description" /></p>
    </candy:for>
  </div>
  <!--candy END: Recent Activity Section -->
</div>
```

#### Explaining Template Variables

```html
<!--candy
  Available variables from controller:
  - user: Current user object { id, name, email, role }
  - posts: Array of post objects
  - categories: Array of category objects
  - settings: Site settings object
candy-->

<div class="profile">
  <h1><candy var="user.name" /></h1>
  <p><candy var="user.email" /></p>
</div>
```

#### Temporary Disabling Code

```html
<div class="products">
  <candy:for in="products" value="product">
    <div class="product-card">
      <h3><candy var="product.name" /></h3>
      <p>$<candy var="product.price" /></p>
      
      <!--candy Temporarily disabled - waiting for API
      <div class="reviews">
        <candy var="product.averageRating" /> stars
      </div>
      candy-->
    </div>
  </candy:for>
</div>
```

#### Version History

```html
<!--candy
  Version History:
  v1.0 - Initial implementation
  v1.1 - Added sorting functionality
  v1.2 - Added filtering by category
  v2.0 - Complete redesign with new layout
candy-->

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
<!--candy This query is cached for 5 minutes -->
<candy:for in="products" value="product">
  <div><candy var="product.name" /></div>
</candy:for>
```

**Avoid:**
```html
<!--candy Loop through products -->
<candy:for in="products" value="product">
  <!--candy Display product name -->
  <div><candy var="product.name" /></div>
</candy:for>
```

### Security Considerations

**Never expose sensitive information in HTML comments:**

```html
<!-- BAD: Visible in browser source -->
<!-- Database password: secret123 -->
<!-- API key: abc123xyz -->

<!--candy GOOD: Not visible in output -->
<!--candy Database password: secret123 -->
<!--candy API key: abc123xyz -->
```

**Be careful with user data:**

```html
<!-- BAD: Exposes user data -->
<!-- User ID: 12345, Email: user@example.com -->

<!--candy GOOD: Hidden from output -->
<!--candy User ID: 12345, Email: user@example.com -->
```

### Comment Syntax Summary

| Type | Syntax | Rendered | Use Case |
|------|--------|----------|----------|
| Backend Single-Line | `<!--candy comment -->` | No | Development notes, TODOs |
| Backend Multi-Line | `<!--candy ... candy-->` | No | Detailed explanations, disabled code |
| HTML Comment | `<!-- comment -->` | Yes | Section markers, browser hacks |
