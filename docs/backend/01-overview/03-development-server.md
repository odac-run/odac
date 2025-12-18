## 🚀 Development Server

Odac provides a built-in development server that allows you to test your website locally without running the full Odac server infrastructure.

### Quick Start

Navigate to your website directory and run one of these commands:

```bash
# Using npm script
npm start

# Using Odac directly
odac framework run
```

Both commands will start a local development server on port `1071` by default, and your website will be accessible at `http://127.0.0.1:1071`.

### Custom Port

You can specify a custom port by adding it as an argument:

```bash
# Using npm script with custom port
npm start 8080

# Using Odac directly with custom port
odac framework run 8080
```

This will start the server on your specified port (e.g., `http://127.0.0.1:8080`).

### Development vs Production

The development server (`npm start`) is designed for:

- **Local testing** and development only
- **Quick iteration** without server setup
- **Debugging** your application logic
- **Testing on localhost** (127.0.0.1)

**Important**: The development server does NOT provide DNS, SSL, or other production services.

For production deployment with full Odac server features, create your website using:

```bash
odac web create
```

This registers your website with the Odac server and provides:

- **Automatic SSL** certificate management
- **DNS handling** for your domain
- **Multi-domain hosting** capabilities
- **Process monitoring** and auto-restart
- **Production optimizations** and security

### Package.json Scripts

When you create a new website, Odac automatically generates a `package.json` with these useful scripts:

```json
{
  "scripts": {
    "start": "odac framework run",
    "test": "echo \"Error: no test specified\" && exit 1"
  }
}
```

- `npm start` - Starts the development server for local testing
- `npm test` - Placeholder for your test suite

### Tips

- The development server automatically detects changes in your code
- Use `Ctrl+C` to stop the development server
- The server will show helpful error messages in the console
- All Odac framework features are available in development mode
