## 🔀 Conditional Rendering

Conditionals allow you to show or hide content based on conditions. This is essential for dynamic user interfaces.

### Basic If Statement

```html
<odac:if condition="user.isAdmin">
  <p>Welcome to the admin panel!</p>
</candy:if>
```

The content inside the tag is only rendered if the condition is true.

### If-Else Structure

```html
<odac:if condition="user.isLoggedIn">
  <p>Welcome back, <odac var="user.name" />!</p>
<odac:else>
  <p>Please log in to continue.</p>
</candy:if>
```

### If-ElseIf-Else Structure

```html
<odac:if condition="user.role === 'admin'">
  <div class="admin-panel">
    <p>You have full admin privileges</p>
  </div>
<odac:elseif condition="user.role === 'moderator'">
  <div class="moderator-panel">
    <p>You have moderator privileges</p>
  </div>
<odac:elseif condition="user.role === 'editor'">
  <div class="editor-panel">
    <p>You have editor privileges</p>
  </div>
<odac:else>
  <div class="user-panel">
    <p>You have regular user privileges</p>
  </div>
</candy:if>
```

### Condition Syntax

Conditions use standard JavaScript expressions:

```html
<!-- Equality -->
<odac:if condition="status === 'active'">Active</candy:if>

<!-- Comparison -->
<odac:if condition="age >= 18">Adult</candy:if>
<odac:if condition="price < 100">Affordable</candy:if>

<!-- Logical operators -->
<odac:if condition="user.isVerified && user.isPremium">
  Premium Verified User
</candy:if>

<odac:if condition="role === 'admin' || role === 'moderator'">
  Staff Member
</candy:if>

<!-- Negation -->
<odac:if condition="!user.isBanned">
  Welcome!
</candy:if>

<!-- Existence check -->
<odac:if condition="user">
  User exists
</candy:if>

<!-- Array/String length -->
<odac:if condition="items.length > 0">
  You have items
</candy:if>

<!-- Method calls -->
<odac:if condition="Odac.Auth.check()">
  Logged in
</candy:if>
```

### Practical Examples

#### User Authentication Status

```html
<nav>
  <odac:if condition="Odac.Auth.check()">
    <a href="/profile">Profile</a>
    <a href="/settings">Settings</a>
    <a href="/logout">Logout</a>
  <odac:else>
    <a href="/login">Login</a>
    <a href="/register">Register</a>
  </candy:if>
</nav>
```

#### Product Stock Status

```html
<div class="product">
  <h3><odac var="product.name" /></h3>
  <p class="price">$<odac var="product.price" /></p>
  
  <odac:if condition="product.stock > 10">
    <span class="badge in-stock">In Stock</span>
    <button>Add to Cart</button>
  <odac:elseif condition="product.stock > 0">
    <span class="badge low-stock">Only <odac var="product.stock" /> left!</span>
    <button>Add to Cart</button>
  <odac:else>
    <span class="badge out-of-stock">Out of Stock</span>
    <button disabled>Notify Me</button>
  </candy:if>
</div>
```

#### User Role-Based Content

```html
<div class="dashboard">
  <h1>Dashboard</h1>
  
  <odac:if condition="user.role === 'admin'">
    <div class="admin-section">
      <h2>Admin Tools</h2>
      <a href="/admin/users">Manage Users</a>
      <a href="/admin/settings">System Settings</a>
      <a href="/admin/logs">View Logs</a>
    </div>
  </candy:if>
  
  <odac:if condition="user.role === 'admin' || user.role === 'moderator'">
    <div class="moderation-section">
      <h2>Moderation</h2>
      <a href="/moderate/posts">Review Posts</a>
      <a href="/moderate/reports">Handle Reports</a>
    </div>
  </candy:if>
  
  <div class="user-section">
    <h2>Your Content</h2>
    <a href="/my-posts">My Posts</a>
    <a href="/my-profile">My Profile</a>
  </div>
</div>
```

#### Form Validation Messages

```html
<form>
  <div class="form-group">
    <label>Email</label>
    <input type="email" name="email" value="<odac var="email" />">
    
    <odac:if condition="errors && errors.email">
      <span class="error"><odac var="errors.email" /></span>
    </candy:if>
  </div>
  
  <div class="form-group">
    <label>Password</label>
    <input type="password" name="password">
    
    <odac:if condition="errors && errors.password">
      <span class="error"><odac var="errors.password" /></span>
    </candy:if>
  </div>
  
  <odac:if condition="success">
    <div class="success-message">
      <odac var="success" />
    </div>
  </candy:if>
  
  <button type="submit">Submit</button>
</form>
```

#### Conditional CSS Classes

```html
<div class="user-card <odac:if condition="user.isPremium">premium</candy:if> <odac:if condition="user.isVerified">verified</candy:if>">
  <h3><odac var="user.name" /></h3>
  
  <odac:if condition="user.isPremium">
    <span class="badge">⭐ Premium</span>
  </candy:if>
  
  <odac:if condition="user.isVerified">
    <span class="badge">✓ Verified</span>
  </candy:if>
</div>
```

#### Nested Conditions

```html
<odac:if condition="user">
  <odac:if condition="user.isActive">
    <odac:if condition="user.subscription">
      <odac:if condition="user.subscription.status === 'active'">
        <div class="premium-content">
          <h2>Premium Content</h2>
          <p>Welcome, premium member!</p>
        </div>
      <odac:elseif condition="user.subscription.status === 'expired'">
        <div class="renewal-notice">
          <p>Your subscription has expired. Please renew to continue.</p>
          <a href="/renew">Renew Now</a>
        </div>
      </candy:if>
    <odac:else>
      <div class="upgrade-notice">
        <p>Upgrade to premium for exclusive content!</p>
        <a href="/upgrade">Upgrade Now</a>
      </div>
    </candy:if>
  <odac:else>
    <div class="inactive-notice">
      <p>Your account is inactive. Please contact support.</p>
    </div>
  </candy:if>
<odac:else>
  <div class="login-notice">
    <p>Please log in to view this content.</p>
    <a href="/login">Login</a>
  </div>
</candy:if>
```

#### Conditional Attributes

```html
<!-- Disabled button -->
<button <odac:if condition="!canSubmit">disabled</candy:if>>
  Submit
</button>

<!-- Selected option -->
<select name="country">
  <option value="tr" <odac:if condition="country === 'tr'">selected</candy:if>>Turkey</option>
  <option value="us" <odac:if condition="country === 'us'">selected</candy:if>>USA</option>
  <option value="uk" <odac:if condition="country === 'uk'">selected</candy:if>>UK</option>
</select>

<!-- Checked checkbox -->
<input 
  type="checkbox" 
  name="terms" 
  <odac:if condition="termsAccepted">checked</candy:if>
>
```

### Best Practices

1. **Keep conditions simple**: Complex logic should be in the controller
2. **Use meaningful variable names**: Make conditions self-documenting
3. **Avoid deep nesting**: Refactor complex nested conditions
4. **Handle null/undefined**: Always check if objects exist before accessing properties

**Good:**
```javascript
// Controller
Odac.set('canEdit', user.isAdmin || user.id === post.authorId)
```

```html
<!-- View -->
<odac:if condition="canEdit">
  <button>Edit</button>
</candy:if>
```

**Avoid:**
```html
<!-- Too complex for a view -->
<odac:if condition="(user && user.role === 'admin') || (user && post && user.id === post.authorId && !post.isLocked)">
  <button>Edit</button>
</candy:if>
```
