## 📦 API and Data Routes

#### `get(path, controller, options)`
Defines a route that responds to `GET` requests. This is ideal for API endpoints that return data (like JSON).

-   `options`: By default, Odac protects routes from CSRF attacks by checking for a token. For a public API or stateless endpoint, you must disable this by passing `{ token: false }`. If you don't, the server will expect a token and will not return a response if one isn't provided.

```javascript
// An API endpoint at GET /api/users/123
// We disable the token check as this is a public API.
Odac.Route.get('/api/users/{id}', 'api/users.get', { token: false });
```

#### `post(path, controller, options)`
Defines a route that responds to `POST` requests, typically used for form submissions. The `{ token: false }` option works here as well, but should be used with caution as POST routes are primary targets for CSRF attacks.

```javascript
// A form that posts data to /login
Odac.Route.post('/login', 'auth.login');
```
