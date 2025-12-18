# Advanced AJAX Navigation

Advanced techniques and patterns for AJAX navigation in Odac.

**Note:** All examples assume you have properly configured your skeleton template and controller. See the [Quick Start guide](01-quick-start.md#skeleton-structure-required) for setup instructions.

## Programmatic Navigation

### Using odac.load()

Navigate programmatically from your code:

```javascript
// Basic usage
odac.load('/about')

// With callback
odac.load('/about', function(page, variables) {
  console.log('Loaded:', page)
  console.log('Data:', variables)
})

// Without updating history
odac.load('/about', callback, false)
```

### Use Cases

**Redirect after form submission:**
```javascript
odac.form('#my-form', function(data) {
  if (data.result.success) {
    odac.load('/success')
  }
})
```

**Conditional navigation:**
```javascript
if (user.isLoggedIn) {
  odac.load('/dashboard')
} else {
  odac.load('/login')
}
```

**Timed navigation:**
```javascript
setTimeout(() => {
  odac.load('/next-page')
}, 3000)
```

## Multi-Section Updates

### Updating Multiple Areas

Update different parts of your page simultaneously:

```javascript
Odac.action({
  navigate: {
    update: {
      content: 'main',
      sidebar: '#sidebar',
      breadcrumb: '.breadcrumb',
      notifications: '#notifications'
    }
  }
})
```

### Controller Setup

Define all parts in your controller:

```javascript
module.exports = function(Odac) {
  odac.View.skeleton('main')
  odac.View.set({
    header: 'main',
    content: 'dashboard',
    sidebar: 'dashboard',
    breadcrumb: 'dashboard',
    footer: 'main'
  })
}
```

## State Management

### Preserving State

Maintain application state across navigations:

```javascript
let appState = {
  user: null,
  cart: [],
  filters: {}
}

Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Update state from server
      if (variables.user) {
        appState.user = variables.user
      }
      
      // Restore UI state
      restoreFilters(appState.filters)
    }
  }
})
```

### Session Storage

Persist state across page reloads:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Save to session storage
      sessionStorage.setItem('lastPage', page)
      sessionStorage.setItem('pageData', JSON.stringify(variables))
    }
  },
  
  load: function() {
    // Restore on app load
    const lastPage = sessionStorage.getItem('lastPage')
    if (lastPage) {
      console.log('Last visited:', lastPage)
    }
  }
})
```

## Animation & Transitions

### Custom Transitions

Add custom animations:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      const main = document.querySelector('main')
      
      // Fade in animation
      main.style.opacity = '0'
      setTimeout(() => {
        main.style.transition = 'opacity 0.3s'
        main.style.opacity = '1'
      }, 10)
    }
  }
})
```

### Page Transitions

Smooth page transitions:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      const main = document.querySelector('main')
      main.classList.add('page-enter')
      
      setTimeout(() => {
        main.classList.remove('page-enter')
        main.classList.add('page-enter-active')
      }, 10)
    }
  }
})
```

```css
main {
  transition: transform 0.3s, opacity 0.3s;
}

main.page-enter {
  opacity: 0;
  transform: translateY(20px);
}

main.page-enter-active {
  opacity: 1;
  transform: translateY(0);
}
```

## Scroll Management

### Scroll to Top

Automatically scroll to top on navigation:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      window.scrollTo({
        top: 0,
        behavior: 'smooth'
      })
    }
  }
})
```

### Preserve Scroll Position

Remember scroll position:

```javascript
let scrollPositions = {}

Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Save current scroll position
      const currentPath = window.location.pathname
      scrollPositions[currentPath] = window.scrollY
      
      // Restore scroll position for this page
      const savedPosition = scrollPositions[page]
      if (savedPosition !== undefined) {
        setTimeout(() => {
          window.scrollTo(0, savedPosition)
        }, 100)
      } else {
        window.scrollTo(0, 0)
      }
    }
  }
})
```

### Scroll to Element

Scroll to specific element after navigation:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Check for hash in URL
      const hash = window.location.hash
      if (hash) {
        setTimeout(() => {
          const element = document.querySelector(hash)
          if (element) {
            element.scrollIntoView({behavior: 'smooth'})
          }
        }, 100)
      }
    }
  }
})
```

## Error Handling

### Handling Failed Requests

Gracefully handle navigation errors:

```javascript
// Override odac.load to add error handling
const originalLoad = odac.load.bind(Odac)

odac.load = function(url, callback, push) {
  try {
    originalLoad(url, function(page, variables) {
      if (callback) callback(page, variables)
    }, push)
  } catch (error) {
    console.error('Navigation failed:', error)
    // Fallback to normal navigation
    window.location.href = url
  }
}
```

### Retry Logic

Retry failed requests:

```javascript
function loadWithRetry(url, maxRetries = 3) {
  let attempts = 0
  
  function attempt() {
    attempts++
    odac.load(url, 
      (page, vars) => {
        console.log('Success after', attempts, 'attempts')
      },
      true
    )
  }
  
  // Initial attempt
  attempt()
  
  // Retry on error (implement error detection)
  // This is a simplified example
}
```

## Performance Optimization

### Debouncing Navigation

Prevent rapid navigation:

```javascript
let navigationTimeout

Odac.action({
  click: {
    'a[href^="/"]': function(e) {
      clearTimeout(navigationTimeout)
      
      navigationTimeout = setTimeout(() => {
        // Navigation happens here
      }, 100)
    }
  }
})
```

### Prefetching

Prefetch pages on hover:

```javascript
let prefetchCache = {}

Odac.action({
  mouseover: {
    'a[href^="/"]': function() {
      const url = this.getAttribute('href')
      
      if (!prefetchCache[url]) {
        // Prefetch the page
        fetch(url, {
          headers: {
            'X-Odac': 'prefetch'
          }
        }).then(response => response.text())
          .then(html => {
            prefetchCache[url] = html
          })
      }
    }
  }
})
```

## Integration with Other Libraries

### With Analytics

Track page views:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Google Analytics
      if (window.gtag) {
        gtag('config', 'GA_ID', {
          page_path: window.location.pathname,
          page_title: variables.title || page
        })
      }
      
      // Plausible
      if (window.plausible) {
        plausible('pageview')
      }
    }
  }
})
```

### With State Management

Redux/Vuex integration:

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: function(page, variables) {
      // Dispatch to Redux
      store.dispatch({
        type: 'PAGE_CHANGED',
        payload: {page, variables}
      })
    }
  }
})
```

## Testing

### Unit Testing

Test navigation logic:

```javascript
describe('Navigation', () => {
  it('should update active nav on page change', () => {
    Odac.action({
      navigate: {
        update: 'main',
        on: (page) => {
          updateActiveNav(page)
        }
      }
    })
    
    // Simulate navigation
    odac.load('/about')
    
    // Assert
    expect(document.querySelector('.nav-link.active').href)
      .toContain('/about')
  })
})
```

## Best Practices

1. **Error Handling**: Always handle failed requests
2. **Loading States**: Show visual feedback during navigation
3. **Accessibility**: Announce page changes to screen readers
4. **Performance**: Minimize DOM updates
5. **SEO**: Ensure content is crawlable
6. **Progressive Enhancement**: Work without JavaScript

## Common Patterns

### Dashboard Navigation

```javascript
Odac.action({
  navigate: {
    links: '.sidebar a, .breadcrumb a',
    update: {
      content: '#dashboard-content',
      breadcrumb: '.breadcrumb',
      stats: '#stats-widget'
    },
    on: (page, vars) => {
      updateSidebar(page)
      updateStats(vars.stats)
    }
  }
})
```

### E-commerce

```javascript
Odac.action({
  navigate: {
    update: {
      content: 'main',
      cart: '#cart-widget'
    },
    on: (page, vars) => {
      updateCartCount(vars.cartItems)
      trackPageView(page)
    }
  }
})
```

### Blog

```javascript
Odac.action({
  navigate: {
    update: 'main',
    on: (page, vars) => {
      if (page === 'post') {
        initComments()
        highlightCode()
      }
    }
  }
})
```

## Next Steps

- Learn about [Form Handling](../03-forms/01-form-handling.md)
- Explore [API Requests](../04-api-requests/01-get-post.md)
- Check [odac.js Overview](../01-overview/01-introduction.md)
