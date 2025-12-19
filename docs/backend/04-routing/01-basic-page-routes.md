## 📄 Basic Page Routes

#### `page(path, controller)`
This is the most common method. It maps a URL path to a controller that is expected to render a standard HTML page. It handles `GET` requests.

-   `path`: The URL path to match (e.g., `/about`).
-   `controller`: The name of the controller file.

```javascript
// When a user visits yoursite.com/
Odac.Route.page('/', 'index');

// When a user visits yoursite.com/contact
Odac.Route.page('/contact', 'contact-form');
```

**Page Identifier:** The controller filename becomes the page identifier in the frontend. For example, `'contact-form'` becomes accessible as `Odac.page()` returning `"contact-form"`. This allows you to run page-specific JavaScript:

```javascript
// Frontend
Odac.action({
  page: {
    'contact-form': function() {
      console.log('Contact form page loaded')
    }
  }
})
```
