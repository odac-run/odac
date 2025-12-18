# AJAX Navigation Configuration

Learn how to configure and customize AJAX navigation in your Odac application.

## Prerequisites

Before configuring AJAX navigation, ensure your skeleton template is properly set up. See the [Quick Start guide](01-quick-start.md#skeleton-structure-required) for skeleton setup instructions.

**Key requirements:**
- Skeleton placeholders must be UPPERCASE and wrapped in HTML tags (e.g., `<main>{{ CONTENT }}</main>`)
- Controller keys are lowercase (e.g., `content: 'home'`)
- Frontend selectors target the HTML tags (e.g., `content: 'main'` targets `<main>` tag)

**Mapping example:**
- Skeleton: `<main>{{ CONTENT }}</main>`
- Controller: `content: 'home'`
- Frontend: `update: { content: 'main' }`

## Configuration Options

### Basic Configuration

The simplest way to enable AJAX navigation:

```javascript
Odac.action({
  navigate: 'main'  // Update <main> element
})
```

### Full Configuration

All available options:

```javascript
Odac.action({
  navigate: {
    // Which links to intercept
    links: 'a[href^="/"]',  // Default: all internal links
    
    // Which elements to update
    update: {
      content: 'main',
      header: 'header',
      sidebar: '#sidebar'
    },
    
    // Callback after navigation
    on: function(page, variables) {
      console.log('Navigated to:', page)
      console.log('Server data:', variables)
    }
  }
})
```

## Link Selectors

### Default Behavior

By default, all internal links (starting with `/`) are intercepted:

```javascript
navigate: true  // Uses 'a[href^="/"]'
```

### Custom Selectors

Target specific links:

```javascript
navigate: {
  links: 'nav a',  // Only navigation links
  update: 'main'
}
```

```javascript
navigate: {
  links: 'a.ajax-link',  // Only links with class
  update: 'main'
}
```

### Excluding Links

Exclude specific links from AJAX navigation:

```html
<!-- Using data attribute -->
<a href="/download.pdf" data-navigate="false">Download</a>

<!-- Using CSS class -->
<a href="/logout" class="no-navigate">Logout</a>
```

Both methods work automatically - no additional configuration needed!

## Element Updates

### Single Element

Update one element:

```javascript
navigate: 'main'  // Shorthand
// or
navigate: {
  update: 'main'
}
```

### Multiple Elements

Update multiple elements simultaneously:

```javascript
navigate: {
  update: {
    content: 'main',
    header: 'header',
    sidebar: '#sidebar',
    breadcrumb: '.breadcrumb'
  }
}
```

The keys (content, header, etc.) must match the view parts defined in your controller:

```javascript
// In controller
odac.View.set({
  header: 'main',
  content: 'about',
  sidebar: 'main'
})
```

## Callbacks

### Navigation Callback

Execute code after each navigation:

```javascript
navigate: {
  update: 'main',
  on: function(page, variables) {
    // page: current page name (e.g., 'about')
    // variables: data from server
    
    // Update page title
    document.title = variables.title || page
    
    // Track analytics
    if (window.gtag) {
      gtag('event', 'page_view', {
        page_path: window.location.pathname
      })
    }
    
    // Update active navigation
    updateActiveNav()
  }
}
```

### Page-Specific Actions

Run code for specific pages:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: (page, vars) => console.log('Navigated to:', page)
  },
  
  page: {
    index: function(variables) {
      console.log('Home page loaded')
    },
    about: function(variables) {
      console.log('About page:', variables.title)
    }
  }
})
```

## Disabling Navigation

### Disable Completely

```javascript
Odac.action({
  navigate: false  // No AJAX navigation
})
```

### Disable for Specific Links

```html
<!-- Data attribute -->
<a href="/download" data-navigate="false">Download</a>

<!-- CSS class -->
<a href="/logout" class="no-navigate">Logout</a>

<!-- External links (automatic) -->
<a href="https://example.com">External</a>
```

## Server-Side Configuration

### Sending Variables

In your controller, send data to the client:

```javascript
module.exports = function(Odac) {
  // Set variables for AJAX responses
  odac.set({
    title: 'About Page',
    user: {name: 'John', role: 'admin'},
    stats: {views: 1234}
  }, true)  // true = include in AJAX
  
  odac.View.skeleton('main')
  odac.View.set({
    header: 'main',
    content: 'about',
    footer: 'main'
  })
}
```

### Accessing Variables

Access server variables in your callback:

```javascript
navigate: {
  update: 'main',
  on: function(page, variables) {
    console.log(variables.title)      // 'About Page'
    console.log(variables.user.name)  // 'John'
    console.log(variables.stats.views) // 1234
  }
}
```

## Advanced Examples

### Loading Indicator

Show a loading spinner during navigation:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: (page, vars) => {
      hideLoadingSpinner()
    }
  },
  
  click: {
    'a[href^="/"]': function(e) {
      if (!this.hasAttribute('data-navigate') && 
          !this.classList.contains('no-navigate')) {
        showLoadingSpinner()
      }
    }
  }
})

function showLoadingSpinner() {
  document.getElementById('spinner').style.display = 'block'
}

function hideLoadingSpinner() {
  document.getElementById('spinner').style.display = 'none'
}
```

### Progress Bar

Animated progress bar:

```javascript
let progressBar = document.getElementById('progress')

Odac.action({
  navigate: {
    update: 'main',
    on: () => {
      progressBar.style.width = '100%'
      setTimeout(() => {
        progressBar.style.width = '0%'
      }, 300)
    }
  },
  
  click: {
    'a[href^="/"]': function() {
      if (!this.hasAttribute('data-navigate')) {
        progressBar.style.width = '30%'
      }
    }
  }
})
```

### Conditional Navigation

Confirm before navigating:

```javascript
Odac.action({
  navigate: {
    update: 'main'
  },
  
  click: {
    'a[href^="/"]': function(e) {
      if (hasUnsavedChanges() && 
          !this.hasAttribute('data-navigate')) {
        if (!confirm('You have unsaved changes. Continue?')) {
          e.preventDefault()
          e.stopPropagation()
        }
      }
    }
  }
})
```

## Best Practices

1. **Keep it Simple**: Start with minimal configuration
2. **Progressive Enhancement**: Ensure links work without JavaScript
3. **Loading States**: Always show loading indicators
4. **Error Handling**: Handle failed requests gracefully
5. **SEO**: Ensure content is accessible to search engines
6. **Performance**: Only update necessary elements

## Troubleshooting

### Links Not Working

- Check that `navigate` is enabled
- Verify link selector matches your links
- Check browser console for errors

### Elements Not Updating

- Ensure element selectors are correct
- Verify view parts are defined in controller
- Check that skeleton has correct placeholders

### Variables Not Available

- Confirm `Odac.set(data, true)` has `true` parameter
- Check variables are set before `View.print()`

## Next Steps

- Learn about [Form Handling](../03-forms/01-form-handling.md)
- Explore [API Requests](../04-api-requests/01-get-post.md)
- Check [Advanced Usage](03-advanced-usage.md)
