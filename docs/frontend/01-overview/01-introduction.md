# Frontend Javascript Framework: candy.js

`candy.js` is a lightweight frontend JavaScript framework designed to simplify interactions with the backend, handle forms, and manage page-specific logic within the CandyPack ecosystem. It provides a set of tools for event handling, AJAX requests, and more, all accessible through the global `Candy` object.

## The Global `Candy` Object

After including `candy.js` in your page, you will have access to a global `Candy` object. This object is the main entry point for all the features of the framework.

## Core Concepts

### Actions

Actions are the fundamental building block of `candy.js`. An action is a collection of event handlers and lifecycle callbacks that define the behavior of a page or a component.

### Pages

`candy.js` has a concept of "pages", which allows you to scope your JavaScript to a specific page. The current page identifier is determined by the backend based on:
- **Controller name** when using controller files (e.g., `'user'` for `controller/page/user.js`)
- **View name** when using view objects (e.g., `'dashboard'` from `{content: 'dashboard'}`)
- This identifier is accessible via `Candy.page()` and stored in `data-candy-page` attribute on the `<html>` element.

### Lifecycle Events

`candy.js` provides several lifecycle events that you can hook into:
-   `start`: Fired once when the script is initialized.
-   `load`: Fired on every page load, after the DOM is ready.
-   `page`: Fired on a specific page, after the DOM is ready.
-   `interval`: Fired repeatedly at a specified interval.

## Event Handling with `Candy.action()`

The `Candy.action()` method is the most important method in the framework. It allows you to register event handlers and lifecycle callbacks.

```javascript
Candy.action({
    // Fired once on DOMContentLoaded
    start: function() {
        console.log('Candy.js started!');
    },

    // Fired on every page load
    load: function() {
        console.log('Page loaded!');
    },

    // Fired only on the 'home' page
    page: {
        home: function() {
            console.log('Welcome to the home page!');
        }
    },

    // Fired every 2 seconds on the 'dashboard' page
    interval: {
        myInterval: {
            interval: 2000,
            page: 'dashboard',
            function: function() {
                console.log('Dashboard is refreshing...');
            }
        }
    },

    // Event handlers
    click: {
        '#my-button': function() {
            alert('Button clicked!');
        }
    },

    // You can also define functions and reference them
    fn: {
        myFunction: function() {
            alert('This is my function!');
        }
    },

    // And then use them in your event handlers
    mouseover: {
        '#my-element': 'fn.myFunction'
    }
});
```

## Working with Forms using `Candy.form()`

`Candy.form()` simplifies AJAX form submissions. It handles serialization, validation feedback, success messages, and file uploads automatically.

```javascript
// Basic usage
Candy.form('#my-form', function(data) {
    // This callback is executed on success
    console.log('Form submitted successfully!', data);
});

// With options
Candy.form({
    form: '#my-form',
    messages: ['success', 'error'], // Show both success and error messages
    loading: function(percent) {
        console.log('Upload progress:', percent + '%');
    }
}, function(data) {
    // Success callback
    if (data.result.success) {
        window.location.href = '/thank-you';
    }
});
```
To display validation errors, you can add elements with the `candy-form-error` attribute to your form. The value of the attribute should be the `name` of the input field.

```html
<input type="text" name="email">
<span candy-form-error="email"></span>
```

## Making AJAX requests with `Candy.get()`

For simple GET requests, you can use the `Candy.get()` method.

```javascript
Candy.get('/api/users', function(data) {
    console.log('Users:', data);
});
```

## Managing CSRF tokens with `Candy.token()`

`candy.js` automatically manages CSRF tokens for you. The `Candy.token()` method will return a valid token for your requests. The `Candy.form()` and `Candy.get()` methods use this automatically, so you usually don't need to call it yourself.

## Other Utility Functions

-   **`Candy.client()`**: Returns a unique client identifier from a cookie.
-   **`Candy.data()`**: Returns data from the `candy_data` cookie, which is set by the backend. This data includes the current page and the initial CSRF token.
-   **`Candy.page()`**: Returns the identifier of the current page. This is the controller name (e.g., `'user'`) or view name (e.g., `'dashboard'`) set by the backend. Use this to conditionally run code for specific pages.
-   **`Candy.storage()`**: A wrapper for `localStorage`.
    ```javascript
    // Set a value
    Candy.storage('my-key', 'my-value');

    // Get a value
    let value = Candy.storage('my-key');

    // Remove a value
    Candy.storage('my-key', null);
    ```
