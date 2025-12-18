## 🎨 Rendering Views

In Odac, you use the `Odac.View` object to render views. There are two main approaches:

### 1. Combining Skeleton and View Parts

The most common usage is to select a skeleton and place view parts into it.

```javascript
module.exports = function (Odac) {
  Odac.View
    .skeleton('main')           // Use skeleton/main.html
    .set('header', 'main')      // Place view/header/main.html into {{ HEADER }}
    .set('content', 'home')     // Place view/content/home.html into {{ CONTENT }}
    .set('footer', 'main')      // Place view/footer/main.html into {{ FOOTER }}
}
```

### 2. Bulk Setting with Object

You can set all view parts at once:

```javascript
module.exports = function (Odac) {
  Odac.View.set({
    skeleton: 'main',
    header: 'main',
    content: 'home',
    footer: 'main'
  })
}
```

### 3. Subdirectories with Dot Notation

View files can be organized in subdirectories. You can access them using dot notation:

```javascript
Odac.View.set({
  skeleton: 'dashboard',
  header: 'dashboard.main',      // view/header/dashboard/main.html
  sidebar: 'dashboard.menu',     // view/sidebar/dashboard/menu.html
  content: 'user.profile'        // view/content/user/profile.html
})
```

### 4. Direct View Rendering from Routes

You can render views directly from route files without using a controller:

```javascript
// route/www.js
Odac.Route.page('/about').view({
  skeleton: 'main',
  header: 'main',
  content: 'about',
  footer: 'main'
})
```

### 5. All Feature

If you're using the same directory structure for all placeholders, you can use the `all()` method:

```javascript
Odac.View
  .skeleton('main')
  .all('home')  // view/home/header.html, view/home/content.html, view/home/footer.html
```

In this case, placeholders like `{{ HEADER }}`, `{{ CONTENT }}`, `{{ FOOTER }}` in the skeleton are automatically matched with `view/home/header.html`, `view/home/content.html`, `view/home/footer.html` files.

### Setting Dynamic Page Titles and Meta Tags

Since skeleton files only support view part placeholders, you have two approaches for dynamic titles:

#### Approach 1: Include Head as a View Part

Create a separate view part for the `<head>` section:

**Skeleton (skeleton/main.html):**
```html
<!DOCTYPE html>
<html lang="en">
<div id="head">
    {{ HEAD }}
</div>
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

**Note:** Each placeholder is wrapped in an HTML tag so AJAX can identify and update specific sections.

**Head View (view/head/main.html):**
```html
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{ Odac.pageTitle }}</title>
    <meta name="description" content="{{ Odac.pageDescription }}">
    <link rel="stylesheet" href="/assets/css/style.css">
</head>
```

**Controller:**
```javascript
module.exports = async function (Odac) {
  const productId = Odac.Request.get('id')
  const product = await Odac.Mysql.table('products')
    .where('id', productId)
    .first()
  
  // Set dynamic title and description
  Odac.pageTitle = product ? `${product.name} - My Store` : 'Product Not Found'
  Odac.pageDescription = product ? product.short_description : ''
  
  Odac.product = product
  
  Odac.View.set({
    skeleton: 'main',
    head: 'main',        // Include dynamic head
    header: 'main',
    content: 'product.detail',
    footer: 'main'
  })
}
```

#### Approach 2: Set Title in Content View

Include the title tag in your content view:

**Skeleton (skeleton/simple.html):**
```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <link rel="stylesheet" href="/assets/css/style.css">
</head>
<body>
    {{ CONTENT }}
</body>
</html>
```

**Content View (view/content/product.html):**
```html
<title>{{ Odac.product.name }} - My Store</title>

<div class="product">
    <h1>{{ Odac.product.name }}</h1>
    <p>{{ Odac.product.description }}</p>
</div>
```

**Note:** This approach is less clean but works for simple cases.

### Important Notes

- View files must have the `.html` extension
- Skeleton files should be in the `skeleton/` directory, view files in the `view/` directory
- Placeholders for view parts are written in uppercase: `{{ HEADER }}`, `{{ CONTENT }}`, etc.
- View part names are specified in lowercase: `header`, `content`, etc.
- Variables in skeleton/views are accessed via `Odac` object: `{{ Odac.variableName }}`
- You don't need to use `return` from the controller, `Odac.View.set()` automatically initiates the rendering process
