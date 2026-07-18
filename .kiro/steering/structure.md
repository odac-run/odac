# Project Structure & Architecture

## Directory Organization

```
├── cmd/                  # Binary entry points (one dir per binary)
│   ├── odac/             # CLI (command table, monitor TUI, boot-on-demand)
│   ├── odac-server/      # Orchestrator daemon (wires all internal services)
│   ├── odac-watchdog/    # Supervisor (restarts the server, log capture)
│   ├── odac-proxy/       # HTTP/HTTPS/HTTP3 reverse proxy data plane
│   ├── odac-dns/         # DNS server data plane
│   └── odac-mail/        # SMTP/IMAP mail server data plane
├── internal/             # All shared code (config, api, appmgr, docker, hub,
│                         # domains, updater, dataplane, proxy, dns, mail, ...)
│   └── lang/locale/      # Embedded i18n catalogs (go:embed)
├── bin/                  # Build output only (gitignored)
├── docs/                 # Documentation
│   ├── index.json        # Documentation navigation structure
│   └── server/           # Server documentation files
└── test/                 # Cross-binary e2e/integration harness
```

## Architecture Patterns

### Single Module, Six Binaries

- **Module**: one Go module named `odac`; every binary builds with `go build ./cmd/...`
- **Wiring**: `cmd/odac-server/main.go` constructs the services and connects them
  through narrow interfaces (dependency injection at the composition root)
- **Boundaries**: CLI ↔ server over the TCP API socket (`internal/apiproto`);
  server ↔ data-plane daemons over unix-socket control APIs (`internal/dataplane`)

### File Naming Conventions

- **Go conventions**: lowercase package names, `snake_case` file names where
  multi-word (e.g. `log_filter.go`), `_test.go` for tests
- **Entry points**: each `cmd/<binary>/main.go`

### Module Structure

- Services expose `Init`/`Start`/`Stop`-style lifecycles managed by the server
- Packages depend on interfaces, not concrete siblings — fakes in tests

### Documentation System

- **Index File**: `docs/index.json` contains the navigation structure for all documentation
- **Adding New Docs**: When creating new documentation files, they MUST be added to `docs/index.json`
- **Language**: All documentation content must be written in English
- **Structure**: Documentation is organized into:
  - `docs/server/` - Server infrastructure documentation (CLI, DNS, SSL, Mail)
- **File Organization**: Each section has folders with numbered prefixes (01-overview, 02-structure, etc.)
- **Navigation**: The index.json file defines the title and hierarchy shown in documentation site
