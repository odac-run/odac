# Project Structure & Architecture

## Directory Organization

```
├── bin/              # Executable binaries (odac)
├── cli/              # Command-line interface
│   └── src/          # CLI implementation (Cli.js, Connector.js, Monitor.js)
├── core/             # Core dependency injection system
├── server/           # Server infrastructure
│   └── src/          # Server modules (DNS, SSL, Mail, Web, etc.)
├── watchdog/         # Process monitoring system
├── docs/             # Documentation (server only)
│   ├── index.json    # Documentation navigation structure
│   └── server/       # Server documentation files
├── locale/           # Internationalization files
└── test/             # Jest test files
```

## Architecture Patterns

### Dependency Injection (Core)

- **Global Odac**: Singleton registry pattern via `global.Odac`
- **Module Loading**: Dynamic require with `core()`, `cli()`, `server()`, `watchdog()` methods
- **Singleton Management**: Automatic instantiation and caching

### File Naming Conventions

- **PascalCase**: Class files and main modules (e.g., `Odac.js`, `Server.js`)
- **camelCase**: Utility functions and instances
- **lowercase**: Entry points (`index.js`)

### Module Structure

- Each module can have an optional `init()` method for setup
- Server modules are typically singletons for infrastructure

### Documentation System

- **Index File**: `docs/index.json` contains the navigation structure for all documentation
- **Adding New Docs**: When creating new documentation files, they MUST be added to `docs/index.json`
- **Language**: All documentation content must be written in English
- **Structure**: Documentation is organized into:
  - `docs/server/` - Server infrastructure documentation (CLI, DNS, SSL, Mail)
- **File Organization**: Each section has folders with numbered prefixes (01-overview, 02-structure, etc.)
- **Navigation**: The index.json file defines the title and hierarchy shown in documentation site
