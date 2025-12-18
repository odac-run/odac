## 📁 View System Overview

Odac's view system creates dynamic HTML pages by combining skeleton (layout) and view (content) files. This system provides a modular structure by keeping page layout and content separate.

### Directory Structure

Your project uses two main directories:

- `skeleton/` - Main page skeletons (layout files)
- `view/` - Page contents and components

### Skeleton Files

Skeleton files define the overall structure of your page. They contain the basic HTML structure including head and body, and host placeholders for content.

Example: `skeleton/main.html`

```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>My Website</title>
    <meta name="description" content="Welcome to my website">
    <link rel="stylesheet" href="/assets/css/style.css">
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

**Important Rules for Placeholders:**

1. **Each placeholder must be wrapped in HTML tags** - This allows AJAX to identify and update specific sections
2. **Never place placeholders directly next to each other** - Bad: `{{ HEADER }}{{ CONTENT }}`, Good: `<header>{{ HEADER }}</header><main>{{ CONTENT }}</main>`
3. **Placeholders are uppercase** - `{{ HEADER }}`, `{{ CONTENT }}`, `{{ FOOTER }}`
4. **Use semantic HTML tags** - `<header>`, `<main>`, `<footer>`, `<aside>`, `<nav>`, etc.

**Why wrap in tags?**
When using AJAX navigation, the system needs to identify which part of the page to update. HTML tags provide clear boundaries for each section.

**Note:** Skeleton files currently support only view part placeholders (uppercase). For dynamic content like page titles, use a view part for the `<head>` section or set them in individual view files.

### View Files

View files contain the content that will be placed into the placeholders within the skeleton. They are organized under the `view/` directory.

Example directory structure:

```
view/
├── header/
│   ├── main.html
│   └── dashboard.html
├── content/
│   ├── home.html
│   └── about.html
└── footer/
    └── main.html
```


