# Technology Stack & Build System

## Tech Stack

- **Runtime**: Node.js 18+ (required)
- **Language**: JavaScript (ES6+, CommonJS modules)
- **Database**: MySQL2, SQLite3 support
- **Security**: bcrypt, node-forge, ACME client for SSL
- **Mail**: Built-in SMTP/IMAP server with DKIM signing
- **Proxy**: HTTP proxy for multi-domain hosting
- **DNS**: Native DNS server implementation

## Code Quality Tools

- **Linting**: ESLint with Prettier integration
- **Testing**: Jest with coverage reporting (no threshold enforcement)
- **Git Hooks**: Husky with pre-commit linting and test execution
- **Release**: Semantic Release with conventional commits
- **Test Check**: Automatic test execution on commit for changed files (pass/fail only)

## Common Commands

```bash
# Development
npm run lint          # Run ESLint
npm run lint:fix      # Fix linting issues automatically
npm test              # Run Jest tests with coverage
npm run test:watch    # Run tests in watch mode

# Release
npm run release       # Semantic release (automated)

# Installation
curl -sL https://odac.run/install | bash  # Quick install
npm install -g odac                        # Manual install

# Git Hooks & CI/CD
# Pre-commit automatically runs:
# 1. lint-staged (ESLint + Prettier on staged files)
# 2. Test execution for changed core/ and server/ files (test/scripts/check-coverage.js)
# Note: Coverage is tracked but doesn't block commits - focus is on test pass/fail

# GitHub Actions (on PR to main/dev):
# 1. Runs all tests with coverage
# 2. Runs linter
# 3. Tests on Node.js 18.x and 22.x
# 4. Uploads coverage to Codecov
# 5. Comments PR with coverage report
```

## Code Style

- **Prettier Config**: No semicolons, single quotes, 140 char width, 2-space tabs
- **ESLint**: Separate configs for server/framework/web/browser contexts
- **Globals**: `Odac` and `__` are global variables across the codebase
- **Module System**: CommonJS (`require`/`module.exports`) for server-side code

## Logging Standards

### Server & Core Modules
- **Log Class**: Use `Odac.core('Log', false).init('ModuleName')` for all logging in `server/` and `core/` directories
- **Usage Pattern**:

  ```javascript
  const {log, error} = Odac.core('Log', false).init('ModuleName')

  log('Info message')
  log('Message with %s placeholder', 'value')
  error('Error message', errorObject)
  ```

- **CLI Mode**: Logs are automatically suppressed in CLI mode to avoid breaking the interface
- **Location**: Log class is in `core/Log.js` (also available via `server/src/Log.js` for backward compatibility)

### Framework Modules
- **Console Logging**: Use `console.log()` and `console.error()` directly in `framework/` directory
- **Reason**: Framework runs in user's application context where Log class may not be available
- **Usage Pattern**:

  ```javascript
  console.log('[ModuleName] Info message')
  console.error('[ModuleName] Error:', error.message)
  ```

### General Rule
- **Server/Core**: Always use Log class (never `console.log`)
- **Framework**: Always use `console.log` (Log class not guaranteed to be available)
