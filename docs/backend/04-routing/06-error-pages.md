## 🚨 Error Pages

Odac framework provides built-in error handling with customizable error pages for different HTTP status codes. You can define custom error pages to provide a better user experience when things go wrong.

#### `error(statusCode, controller)`
Maps HTTP error status codes to custom controller handlers. This allows you to create branded error pages instead of showing generic browser error messages.

-   `statusCode`: The HTTP status code to handle (e.g., `404`, `500`).
-   `controller`: The name of the controller file to handle this error.

```javascript
// Custom 404 Not Found page
Odac.Route.error(404, 'errors/not-found');

// Custom 500 Internal Server Error page
Odac.Route.error(500, 'errors/server-error');

// Custom 403 Forbidden page
Odac.Route.error(403, 'errors/forbidden');
```

#### Common Error Status Codes

- **404 Not Found**: Page or resource doesn't exist
- **403 Forbidden**: Access denied to the resource
- **500 Internal Server Error**: Server-side error occurred
- **401 Unauthorized**: Authentication required
- **400 Bad Request**: Invalid request format

#### Error Controller Example

Create error controllers in your `controller/errors/` directory:

```javascript
// controller/errors/not-found.js
module.exports = function() {
    Odac.response.status(404);
    return Odac.view('errors/404', {
        title: 'Page Not Found',
        message: 'The page you are looking for could not be found.'
    });
};
```

```javascript
// controller/errors/server-error.js
module.exports = function() {
    Odac.response.status(500);
    return Odac.view('errors/500', {
        title: 'Server Error',
        message: 'Something went wrong on our end. Please try again later.'
    });
};
```

#### Error Views

Create corresponding view templates in your `view/errors/` directory:

```html
<!-- view/errors/404.html -->
<!DOCTYPE html>
<html>
<head>
    <title>{{title}}</title>
</head>
<body>
    <h1>404 - Page Not Found</h1>
    <p>{{message}}</p>
    <a href="/">Go back to homepage</a>
</body>
</html>
```

#### Default Error Handling

If no custom error page is defined, Odac will fall back to its built-in error responses. It's recommended to at least define custom 404 and 500 error pages for a professional user experience.

#### Error Context

Error controllers receive the same Odac context as regular controllers, allowing you to:

- Access request information (`Odac.request`)
- Use database connections (`Odac.db`)
- Render views with data (`Odac.view`)
- Redirect users (`Odac.redirect`)
- Log error details for debugging

```javascript
// controller/errors/server-error.js
module.exports = function() {
    // Log the error for debugging
    console.error('Server error occurred:', Odac.request.url);
    
    Odac.response.status(500);
    return Odac.view('errors/500', {
        title: 'Oops! Something went wrong',
        supportEmail: 'support@yoursite.com'
    });
};
```