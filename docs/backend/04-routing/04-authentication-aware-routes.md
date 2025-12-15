## 🔐 Authentication-Aware Routes

These methods let you define routes that require authentication. Only logged-in users can access these routes.

#### `auth.page(path, controller)`
Defines a page route that requires authentication.

```javascript
// Only authenticated users can access the dashboard
Candy.Route.auth.page('/dashboard', 'dashboard.index');
```

#### `auth.page(path, viewConfig)`
Defines a controller-less page route that requires authentication.

```javascript
// Only authenticated users can see this view
Candy.Route.auth.page('/profile', {
  skeleton: 'main',
  head: 'profile.head',
  content: 'profile',
  script: 'profile'
});
```

#### `auth.get(path, controller, options)`
Defines a GET route that requires authentication.

```javascript
// API endpoint for authenticated users only
Candy.Route.auth.get('/api/user/profile', 'api.user.profile');
```

#### `auth.post(path, controller, options)`
Defines a POST route that requires authentication.

```javascript
// Only authenticated users can update their profile
Candy.Route.auth.post('/api/user/update', 'api.user.update');
```

#### CSRF Token Protection
By default, all POST and GET routes have CSRF token protection enabled. You can disable it with the `token` option:

```javascript
// Disable CSRF token check for this route
Candy.Route.auth.post('/api/webhook', 'api.webhook', {token: false});
```
