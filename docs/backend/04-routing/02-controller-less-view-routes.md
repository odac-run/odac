## ⚡ Controller-less View Routes

For simple pages that don't require complex logic in a controller, you can render a view directly from your route file by passing a view configuration object as the second parameter.

#### `page(path, { ... })`
This defines a page and immediately tells it which view components to render.

```javascript
Candy.Route.page("/users", {
    skeleton: "dashboard",
    header: "dashboard.main",
    sidebar: "dashboard.main",
    footer: "dashboard.main",
    content: "users"
});
```
This example tells CandyPack to render the `/users` page by assembling a view from multiple parts, likely using a main `dashboard` skeleton and filling it with different content blocks.

#### `auth.page(path, { ... })`
Similar to `page()`, but requires authentication. Only authenticated users can access this route.

```javascript
// Only authenticated users can see the dashboard
Candy.Route.auth.page('/', {
    skeleton: 'main', 
    content: 'dashboard'
});
```

See [Authentication-Aware Routes](04-authentication-aware-routes.md) for more details.
