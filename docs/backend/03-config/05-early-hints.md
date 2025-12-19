# Early Hints (HTTP 103)

Early Hints is a performance optimization feature that allows the server to send preliminary HTTP headers to the browser before the final response is ready. This enables browsers to start preloading critical resources (CSS, JavaScript, fonts) while the server is still processing the request.

## Zero-Config Operation

Early Hints works automatically without any configuration. The framework:

1. **Detects** critical resources from your HTML files at startup
2. **Caches** resource information for fast access
3. **Sends** Early Hints on subsequent requests automatically
4. **Optimizes** page load performance transparently

You don't need to do anything - it just works!

## How It Works

### Automatic Resource Detection

When your application starts, Odac scans your `view/` and `skeleton/` directories and builds a manifest of critical resources:

```html
<!-- skeleton/main.html -->
<html>
  <head>
    <link rel="stylesheet" href="/css/main.css">
    <script src="/js/app.js"></script>
  </head>
  <body>...</body>
</html>
```

The framework automatically detects:
- ✅ CSS files (`<link rel="stylesheet">`)
- ✅ Blocking JavaScript (`<script src="...">` without `defer` or `async`)
- ✅ Web fonts (`<link href="...woff2">`)

### Request Flow

**First Request:**
```
1. Browser requests /
2. Controller sets view: skeleton('main')
3. Framework checks manifest for 'skeleton/main'
4. Sends 103 Early Hints with CSS/JS links
5. Browser starts downloading resources
6. Server processes request (database queries, etc.)
7. Sends 200 OK with HTML
8. Resources already loaded!
```

**Subsequent Requests:**
Same flow - hints are sent from the manifest immediately.

## Configuration (Optional)

While Early Hints works automatically, you can customize it in `config.json`:

```json
{
  "earlyHints": {
    "enabled": true,
    "auto": true,
    "maxResources": 5
  }
}
```

### Options

#### `enabled` (boolean, default: `true`)
Enable or disable Early Hints globally.

```json
{
  "earlyHints": {
    "enabled": false
  }
}
```

#### `auto` (boolean, default: `true`)
Enable automatic resource detection and hint generation.

```json
{
  "earlyHints": {
    "auto": false
  }
}
```

#### `maxResources` (number, default: `5`)
Maximum number of resources to include in Early Hints. Limiting this prevents overwhelming the browser with too many preload hints.

```json
{
  "earlyHints": {
    "maxResources": 3
  }
}
```

## What Gets Preloaded?

The framework intelligently selects only **critical resources** from the `<head>` section:

### ✅ Included
- CSS files: `<link rel="stylesheet" href="/css/main.css">`
- Blocking JavaScript: `<script src="/js/app.js"></script>`
- Web fonts: `<link href="/fonts/main.woff2" as="font">` (supports `.woff`, `.woff2`, `.ttf`, `.otf`, `.eot`)

### ❌ Excluded
- Deferred scripts: `<script src="/js/app.js" defer></script>`
- Async scripts: `<script src="/js/app.js" async></script>`
- Deferred CSS: `<link rel="stylesheet" href="/css/non-critical.css" defer>`
- Resources in `<body>` (not critical for initial render)
- Images (handled by browser's own optimization)

## Excluding Resources from Early Hints

If you want to prevent specific resources from being preloaded, use the `defer` attribute:

```html
<head>
  <!-- Critical CSS - will be preloaded -->
  <link rel="stylesheet" href="/css/critical.css">
  
  <!-- Non-critical CSS - will NOT be preloaded -->
  <link rel="stylesheet" href="/css/non-critical.css" defer>
  
  <!-- Critical JS - will be preloaded -->
  <script src="/js/app.js"></script>
  
  <!-- Analytics JS - will NOT be preloaded -->
  <script src="/js/analytics.js" defer></script>
</head>
```

The `defer` attribute works consistently for both CSS and JavaScript:
- **For JS**: Browser's native defer behavior (execute after DOM is ready)
- **For CSS**: Odac-specific (exclude from Early Hints, but still loads normally)

This is useful for:
- Non-critical styles (animations, print styles)
- Analytics and tracking scripts
- Third-party widgets
- Below-the-fold content styles

## Performance Impact

Early Hints is most effective when:
- Server-side processing takes time (database queries, API calls)
- You have critical CSS/JS files in `<head>`
- Users are on slower connections

### Example Improvement

**Without Early Hints:**
```
Server processing: 500ms
Resource download: 200ms
Total: 700ms
```

**With Early Hints:**
```
Server processing: 500ms (resources downloading in parallel)
Total: 500ms (200ms saved!)
```

## Browser Support

- Chrome 103+
- Edge 103+
- Firefox 103+
- Safari (partial support)

Older browsers simply ignore the `103` response and wait for the `200 OK` - no breaking changes!

## Technical Details

### Manifest System

At startup, the framework builds an in-memory manifest:

```javascript
{
  'view/header/home': [
    {href: '/css/header.css', as: 'style'},
    {href: '/js/header.js', as: 'script'}
  ],
  'skeleton/main': [
    {href: '/css/main.css', as: 'style'},
    {href: '/js/app.js', as: 'script'}
  ]
}
```

### Proxy Integration

Odac's architecture uses a proxy layer. Early Hints are:
1. Generated in the framework
2. Sent via `X-Odac-Early-Hints` header to proxy
3. Forwarded to client as `103 Early Hints` by proxy

This ensures Early Hints work correctly in the multi-domain hosting environment.

### Node.js Requirements

- Requires Node.js 18+ for `response.writeEarlyHints()` API
- Automatically disabled on older Node.js versions
- No errors or warnings - graceful degradation

## Best Practices

### 1. Keep Critical Resources in `<head>`

```html
<!-- Good: Critical CSS in head -->
<head>
  <link rel="stylesheet" href="/css/critical.css">
</head>

<!-- Bad: CSS in body -->
<body>
  <link rel="stylesheet" href="/css/styles.css">
</body>
```

### 2. Use `defer` for Non-Critical Scripts

```html
<!-- Good: Non-critical scripts deferred -->
<script src="/js/analytics.js" defer></script>

<!-- Bad: Blocking script for non-critical feature -->
<script src="/js/analytics.js"></script>
```

### 3. Minimize Critical Resources

Aim for 3-5 critical resources maximum. Combine CSS/JS files if needed:

```html
<!-- Good: Combined critical CSS -->
<link rel="stylesheet" href="/css/critical.css">

<!-- Bad: Too many separate files -->
<link rel="stylesheet" href="/css/reset.css">
<link rel="stylesheet" href="/css/typography.css">
<link rel="stylesheet" href="/css/layout.css">
<link rel="stylesheet" href="/css/components.css">
```

Or use `defer` for non-critical resources:

```html
<!-- Good: Critical resources only -->
<link rel="stylesheet" href="/css/critical.css">
<link rel="stylesheet" href="/css/animations.css" defer>
<link rel="stylesheet" href="/css/print.css" defer>
```

### 4. Preload Fonts

If using custom fonts, include them in `<head>`. All common font formats are supported:

```html
<!-- WOFF2 (recommended, best compression) -->
<link rel="preload" href="/fonts/main.woff2" as="font" type="font/woff2" crossorigin>

<!-- WOFF (fallback for older browsers) -->
<link rel="preload" href="/fonts/main.woff" as="font" type="font/woff" crossorigin>

<!-- TrueType/OpenType -->
<link rel="preload" href="/fonts/main.ttf" as="font" type="font/ttf" crossorigin>
<link rel="preload" href="/fonts/main.otf" as="font" type="font/otf" crossorigin>
```

## Troubleshooting

### Early Hints Not Working?

**Check Node.js version:**
```bash
node --version  # Should be 18.0.0 or higher
```

**Verify configuration:**
```json
{
  "earlyHints": {
    "enabled": true  // Make sure it's not disabled
  }
}
```

**Check browser DevTools:**
1. Open Network tab
2. Look for `103 Early Hints` status
3. Check response headers for `Link:` headers

### No Resources Detected?

Make sure resources are in `<head>`:
```html
<!-- This will be detected -->
<head>
  <link rel="stylesheet" href="/css/main.css">
</head>

<!-- This will NOT be detected -->
<body>
  <link rel="stylesheet" href="/css/main.css">
</body>
```

### Too Many Resources?

Reduce `maxResources` in config:
```json
{
  "earlyHints": {
    "maxResources": 3
  }
}
```

## Disabling Early Hints

Removing the `earlyHints` configuration section from your `config.json` is equivalent to using the default settings, which has the feature enabled. To truly disable Early Hints, you must explicitly set `enabled: false` in your configuration:

```json
{
  "earlyHints": {
    "enabled": false
  }
}
```

## Summary

Early Hints is a powerful performance optimization that:
- ✅ Works automatically (zero-config)
- ✅ Detects critical resources intelligently
- ✅ Improves page load times
- ✅ Requires no code changes
- ✅ Degrades gracefully on older browsers
- ✅ Can be customized if needed

Just build your app normally, and Early Hints will optimize it for you!
