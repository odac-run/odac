## 🗺️ Static Route Mapping (Optional)

Normally, all publicly served files should be in your `public` folder. However, if you need to expose a specific file from somewhere else on your server, you can use the optional `route` object in your `config.json`. This creates a direct mapping from a URL path to that file.

```json
"route": {
  "/assets/js/odac.js": "${odac}/framework/web/odac.js",
  "/css/main.css": "/path/to/your/project/assets/css/main.css"
}
```

When a user visits a URL that matches a key in the `route` object, Odac will serve the corresponding file from your filesystem.

#### Using the `${odac}` Variable

The special variable `${odac}` is a shortcut that points to the root directory where Odac is installed. This is helpful for linking to files that are part of the framework itself.

#### Absolute Paths

For your own project files, you should provide a full, absolute path to the file on your server.
