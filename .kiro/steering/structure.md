# Project Structure & Architecture

## Directory Organization

```
├── bin/              # Executable binaries (odac)
├── cli/              # Command-line interface
│   └── src/          # CLI implementation (Cli.js, Connector.js, Monitor.js)
├── core/             # Core dependency injection system
├── framework/        # Web framework implementation
│   ├── src/          # Framework modules (Auth, Route, Server, etc.)
│   └── web/          # Client-side JavaScript (odac.js)
├── server/           # Server infrastructure
│   └── src/          # Server modules (DNS, SSL, Mail, Web, etc.)
├── watchdog/         # Process monitoring system
├── web/              # Template website (copied when creating new sites)
│   ├── controller/   # Template controllers
│   ├── package.json  # Template package.json with {{domain}} placeholders
│   └── config.json   # Template configuration
├── docs/             # Documentation (backend, frontend & server)
│   ├── index.json    # Documentation navigation structure
│   ├── backend/      # Backend documentation files
│   ├── frontend/     # Frontend documentation files
│   └── server/       # Server documentation files
├── locale/           # Internationalization files
└── test/             # Jest test files
```

## Architecture Patterns

### Dependency Injection (Core)

- **Global Odac**: Singleton registry pattern via `global.Odac`
- **Module Loading**: Dynamic require with `core()`, `cli()`, `server()`, `watchdog()` methods
- **Singleton Management**: Automatic instantiation and caching

### Framework Pattern

- **Request Lifecycle**: Each request gets a fresh Odac instance with req/res context
- **Controller Pattern**: Simple function exports in `controller/` directories
- **Helper Functions**: Global shortcuts (`__()`, `abort()`, `return()`, etc.)

### File Naming Conventions

- **PascalCase**: Class files and main modules (e.g., `Odac.js`, `Server.js`)
- **camelCase**: Utility functions and instances
- **lowercase**: Entry points (`index.js`)

### Module Structure

- Each module can have an optional `init()` method for setup
- Framework modules receive Odac instance for request context
- Server modules are typically singletons for infrastructure

### Web Template System

- **Template Directory**: `web/` serves as the template for new websites
- **Automatic Copying**: When creating a new site, entire `web/` directory is copied
- **Template Variables**: Files can contain placeholders like `{{domain}}` and `{{domain_original}}`
- **Post-Processing**: After copying, template variables are replaced with actual values
- **Template Files**:
  - `package.json` - Contains domain placeholders for name and description
  - `config.json` - Basic routing configuration
  - `controller/` - Example controller implementations

### Documentation System

- **Index File**: `docs/index.json` contains the navigation structure for all documentation
- **Adding New Docs**: When creating new documentation files, they MUST be added to `docs/index.json`
- **Language**: All documentation content must be written in English
- **Structure**: Documentation is organized into THREE sections only:
  - `docs/server/` - Server infrastructure documentation (CLI, DNS, SSL, Mail)
  - `docs/backend/` - Backend framework documentation (Controllers, Routing, Auth, Database)
  - `docs/frontend/` - Frontend documentation (odac.js, AJAX navigation, Forms)
- **IMPORTANT**: There is NO `docs/framework/` directory - backend docs go in `docs/backend/`
- **File Organization**: Each section has folders with numbered prefixes (01-overview, 02-structure, etc.)
- **Navigation**: The index.json file defines the title and hierarchy shown in documentation site
