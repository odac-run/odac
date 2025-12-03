# AJAX Navigation

CandyPack framework includes a built-in AJAX navigation system that enables smooth, single-page application (SPA) style navigation without full page reloads.

## Features

- **Zero Configuration**: Works automatically with all internal links (`/`)
- **Smooth Transitions**: Load only specific page sections without full page reload
- **History API Integration**: Browser back/forward buttons work seamlessly
- **Automatic Token Management**: CSRF tokens are handled automatically
- **Progressive Enhancement**: Falls back to normal navigation if JavaScript fails
- **Flexible Element Loading**: Choose which page sections to update
- **Page-Specific Callbacks**: Run custom code when specific pages load

## Quick Start

### Minimal Setup (Recommended)

Just enable navigation - it automatically handles all internal links:

```javascript
Candy.action({
  navigate: 'main'  // Update <main> element on navigation
})
```

That's it! All links starting with `/` will now load via AJAX.

### Medium Setup

Add a callback for post-navigation actions:

```javascript
Candy.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      console.log('Navigated to:', page)
      updateActiveNav()
    }
  }
})
```

### Advanced Setup

Full control over navigation behavior:

```javascript
Candy.action({
  navigate: {
    links: 'a[href^="/"]',  // Which links to intercept
    update: {                // Which elements to update
      content: 'main',
      header: 'header'
    },
    on: function(page, variables) {
      console.log('Page:', page)
      console.log('Data:', variables)
    }
  }
})
```

### HTML Markup

No special attributes needed! Just use normal links:

```html
<nav>
  <a href="/">Home</a>
  <a href="/about">About</a>
  <a href="/docs">Docs</a>
</nav>
```

All internal links (starting with `/`) are automatically handled.

## Server-Side Setup

### Skeleton Structure (Required)

For AJAX navigation to work properly, you must define a skeleton template that contains placeholders for the sections you want to update.

**Important Rules:**
- Placeholders must be **UPPERCASE** (e.g., `{{ HEADER }}`, `{{ CONTENT }}`)
- Each placeholder must be **wrapped in HTML tags** (e.g., `<header>{{ HEADER }}</header>`)
- HTML tags provide boundaries for AJAX to identify and update specific sections

Example `skeleton/main.html`:

```html
<!DOCTYPE html>
<html>
<head>
  <title>My Site</title>
  <script src="/assets/js/candy.js"></script>
  <script src="/assets/js/app.js"></script>
</head>
<body>
  <header>
    {{ HEADER }}
  </header>
  
  <main>
    {{ CONTENT }}
  </main>
  
  <footer>
    {{ FOOTER }}
  </footer>
</body>
</html>
```

**Key Points:**
- Placeholder names in skeleton are UPPERCASE: `{{ HEADER }}`, `{{ CONTENT }}`
- Controller keys are lowercase: `header`, `content`, `footer`
- Frontend selectors target the HTML tags: `'header'`, `'main'`, `'footer'`

### Controller Setup

Controllers automatically support AJAX loading. Use `Candy.View.skeleton()` to specify which skeleton to use:

```javascript
module.exports = function (Candy) {
  // Define the skeleton template
  Candy.View.skeleton('main')
  
  // Set view parts - lowercase keys map to UPPERCASE placeholders
  Candy.View.set({
    header: 'main',      // Loads view/header/main.html into {{ HEADER }}
    content: 'about',    // Loads view/content/about.html into {{ CONTENT }}
    footer: 'main'       // Loads view/footer/main.html into {{ FOOTER }}
  })
  
  // Optional: Send variables to frontend (AJAX only)
  Candy.set({
    pageTitle: 'About',
    data: {foo: 'bar'}
  }, true) // true = include in AJAX responses
}
```

**Mapping:**
- Controller key `header` → Skeleton placeholder `{{ HEADER }}`
- Controller key `content` → Skeleton placeholder `{{ CONTENT }}`
- Controller key `footer` → Skeleton placeholder `{{ FOOTER }}`

## How It Works

### Normal Page Load

1. User visits `/about`
2. Server renders full HTML with skeleton + all view parts
3. Browser displays complete page

### AJAX Page Load

1. User clicks `<a href="/about">`
2. JavaScript intercepts click and sends AJAX request with:
   - Header: `X-Candy: ajaxload`
   - Header: `X-Candy-Load: content,header` (requested sections)
3. Server detects AJAX request and returns only requested sections as JSON:
   ```json
   {
     "output": {
       "content": "<div>About page content...</div>",
       "header": "<nav>...</nav>"
     },
     "variables": {
       "pageTitle": "About",
       "data": {"foo": "bar"}
     }
   }
   ```
4. JavaScript updates specified DOM elements with fade animation
5. Browser URL updates via History API
6. Page-specific callbacks execute

**Key Points:**
- The `output` keys in the JSON response match the lowercase keys from `Candy.View.set()` in your controller
- These keys correspond to UPPERCASE placeholders in your skeleton (e.g., `content` → `{{ CONTENT }}`)
- Only the sections specified in `navigate.update` are sent and updated
- Frontend selectors target the HTML tags wrapping the placeholders

## API Reference

### Candy.action({ navigate: ... })

Initialize AJAX navigation using the action system.

#### Minimal Usage
```javascript
Candy.action({
  navigate: 'main'  // Just specify element to update
})
```
- **Default selector**: `'a[href^="/"]'` (all internal links)
- **Default element**: Updates specified element as 'content'

#### Medium Usage
```javascript
Candy.action({
  navigate: {
    update: 'main',           // Element to update
    on: function(page, vars) { // Callback after navigation
      console.log('Navigated to:', page)
    }
  }
})
```

#### Advanced Usage
```javascript
Candy.action({
  navigate: {
    links: 'a[href^="/"]',    // Which links to intercept
    update: {                  // Multiple elements to update
      content: 'main',
      header: 'header',
      sidebar: '#sidebar'
    },
    on: function(page, variables) {
      // Called after each navigation
      // page: current page name (e.g., 'about')
      // variables: data from server
    }
  }
})
```

#### Boolean Usage
```javascript
Candy.action({
  navigate: true   // Enable with all defaults
})

// Or disable completely
Candy.action({
  navigate: false  // Disable AJAX navigation
})
```
- **Selector**: `'a[href^="/"]'`
- **Update**: `{content: 'main'}`

### Excluding Specific Links

You can exclude specific links from AJAX navigation using either:

**1. Data Attribute:**
```html
<a href="/download" data-navigate="false">Download PDF</a>
<a href="/external" data-navigate="false">External Link</a>
```

**2. CSS Class:**
```html
<a href="/download" class="no-navigate">Download PDF</a>
<a href="/logout" class="no-navigate">Logout</a>
```

Both methods work automatically - no additional configuration needed!

#### Configuration Options

**`links`** or **`selector`** (string, optional)
- CSS selector for links to intercept
- Default: `'a[href^="/"]'` (all internal links)
- Examples: `'a.ajax-link'`, `'nav a'`, `'a[data-ajax]'`

**`update`** or **`elements`** (string | object, optional)
- String: Single element selector (becomes `{content: selector}`)
- Object: Multiple elements to update
- Default: `{content: 'main'}`
- Examples:
  ```javascript
  update: 'main'  // Single element
  update: {       // Multiple elements
    content: 'main',
    header: 'header',
    sidebar: '#sidebar'
  }
  ```

**`on`** or **`callback`** (function, optional)
- Called after each successful navigation
- Parameters:
  - `page` (string): Current page name
  - `variables` (object): Server-side data
- Example:
  ```javascript
  on: function(page, variables) {
    console.log('Page:', page)
    updateAnalytics(page)
  }
  ```

### Candy.loader(selector, elements, callback)

Low-level method for direct initialization (not recommended for new code).

**Parameters:** Same as navigate configuration, but as separate arguments.

### Candy.load(url, callback, push)

Programmatically load a page via AJAX.

**Parameters:**
- `url` (string): URL to load
- `callback` (function): Optional callback after load
- `push` (boolean): Whether to update browser history (default: true)

**Example:**
```javascript
Candy.load('/about', function(page, variables) {
  console.log('Loaded:', page)
})
```

## Page-Specific Actions

Run code when specific pages load. The page identifier is based on the controller name or view name:

```javascript
Candy.action({
  page: {
    // Runs when controller/page/index.js is used
    index: function(variables) {
      console.log('Home page loaded')
    },
    
    // Runs when controller/page/about.js is used
    about: function(variables) {
      console.log('About page loaded')
      console.log('Server data:', variables)
    },
    
    // Runs when view object has {content: 'dashboard'}
    dashboard: function(variables) {
      console.log('Dashboard loaded')
    }
  }
})
```

**Page Identifier Rules:**
- **With controller**: Uses controller filename (e.g., `user.js` → `'user'`)
- **With view object**: Uses `content` or `all` value (e.g., `{content: 'dashboard'}` → `'dashboard'`)
- Accessible via `Candy.page()` or `document.documentElement.dataset.candyPage`

## Server Variables

Send data from server to client in AJAX responses:

```javascript
// In controller
Candy.set({
  user: {name: 'John', role: 'admin'},
  stats: {views: 1234}
}, true) // true = include in AJAX
```

Access in client:

```javascript
Candy.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      console.log(variables.user.name) // 'John'
      console.log(variables.stats.views) // 1234
    }
  }
})
```

## Best Practices

1. **Progressive Enhancement**: Always ensure links work without JavaScript
2. **Loading States**: Show loading indicators during transitions
3. **Error Handling**: Provide fallback for failed AJAX requests
4. **SEO**: Ensure content is accessible to search engines
5. **Performance**: Only load necessary page sections

## Example: Complete Setup

### Minimal Example
```javascript
// Just enable AJAX navigation
Candy.action({
  navigate: 'main'
})
```

### Real-World Example
```javascript
// app.js - Everything in one Candy.action() call
Candy.action({
  // AJAX Navigation - automatically handles all internal links
  navigate: {
    update: 'main',
    on: function(page, variables) {
      Candy.fn.updateActiveNav(window.location.pathname)
      console.log('Navigated to:', page)
    }
  },
  
  // Custom functions (accessible as Candy.fn.functionName)
  function: {
    updateActiveNav: function(url) {
      document.querySelectorAll('nav a').forEach(link => {
        link.classList.toggle('active', link.getAttribute('href') === url)
      })
    }
  },
  
  // App initialization
  load: function() {
    console.log('App initialized')
    Candy.fn.updateActiveNav(window.location.pathname)
  },
  
  // Page-specific code
  page: {
    index: function(variables) {
      // Home page specific code
      Candy.form('#contact-form', function(data) {
        if (data.result.success) {
          alert('Message sent!')
        }
      })
    },
    
    about: function(variables) {
      // About page specific code
      console.log('About page:', variables.pageTitle)
    }
  },
  
  // Event handlers
  click: {
    '#refresh-btn': function() {
      Candy.load(window.location.pathname)
    }
  }
})
```

### Advanced Multi-Section Example
```javascript
Candy.action({
  navigate: {
    update: {
      content: 'main',
      sidebar: '#sidebar',
      breadcrumb: '.breadcrumb'
    },
    on: function(page, vars) {
      // Update page title
      document.title = vars.title || page
      
      // Track analytics
      if (window.gtag) {
        gtag('config', 'GA_ID', {page_path: window.location.pathname})
      }
    }
  }
})
```

## Disabling Navigation

### Disable Completely
```javascript
Candy.action({
  navigate: false  // Disable AJAX navigation entirely
})
```

### Disable for Specific Links

**Method 1: Data Attribute**
```html
<a href="/download.pdf" data-navigate="false">Download PDF</a>
<a href="/api/export" data-navigate="false">Export Data</a>
```

**Method 2: CSS Class**
```html
<a href="/logout" class="no-navigate">Logout</a>
<a href="/admin" class="no-navigate">Admin Panel</a>
```

**Common Use Cases:**
- File downloads
- External links
- Logout/login actions
- Admin panels
- API endpoints
- Forms with file uploads

## Best Practices

### 1. Use Minimal Configuration
```javascript
// Simple and effective
Candy.action({
  navigate: 'main'
})
```

### 2. Exclude Special Links
```html
<!-- Downloads -->
<a href="/files/report.pdf" data-navigate="false">Download Report</a>

<!-- External -->
<a href="https://example.com" target="_blank">External Site</a>

<!-- Actions -->
<a href="/logout" class="no-navigate">Logout</a>
```

### 3. Handle Loading States
```javascript
Candy.action({
  navigate: {
    update: 'main',
    on: (page, vars) => {
      hideLoadingSpinner()
      updatePageTitle(vars.title)
    }
  },
  
  click: {
    'a[href^="/"]': function() {
      showLoadingSpinner()
    }
  }
})
```

## Troubleshooting

### Links not loading via AJAX

- Check browser console for errors
- Verify navigate is enabled in `Candy.action()`
- Ensure links start with `/` for internal navigation

### Specific links should not use AJAX

- Add `data-navigate="false"` attribute
- Or add `no-navigate` class
- Or customize selector to exclude them

### Elements not updating

This is usually caused by mismatched keys between your skeleton, controller, and frontend configuration.

**Check these three places match:**

1. **Skeleton template** (`skeleton/main.html`):
   ```html
   <header>
     {{ HEADER }}
   </header>
   <main>
     {{ CONTENT }}
   </main>
   ```

2. **Controller** (`controller/page/about.js`):
   ```javascript
   Candy.View.skeleton('main')
   Candy.View.set({
     header: 'main',    // Lowercase → {{ HEADER }}
     content: 'about'   // Lowercase → {{ CONTENT }}
   })
   ```

3. **Frontend** (`public/assets/js/app.js`):
   ```javascript
   Candy.action({
     navigate: {
       update: {
         header: 'header',  // Targets <header> tag
         content: 'main'    // Targets <main> tag
       }
     }
   })
   ```

**Mapping:**
- Skeleton: `{{ HEADER }}` (uppercase) wrapped in `<header>` tag
- Controller: `header: 'main'` (lowercase key)
- Frontend: `header: 'header'` (lowercase key, CSS selector for `<header>` tag)

**Also verify:**
- Element selectors match actual DOM elements (e.g., `'main'` matches `<main>`)
- Skeleton template is defined with `Candy.View.skeleton('main')`
- View parts are defined in controller with `Candy.View.set()`

### Variables not available

- Confirm `Candy.set(data, true)` has `true` as second parameter
- Check that variables are set before `View.print()` is called
