### 🛠️ Fixes & Improvements

- Update GitHub Actions to their latest versions and add QEMU for multi-platform Docker image builds.



---

Powered by [⚡ ODAC](https://odac.run)

### 🛠️ Fixes & Improvements

- Updater fix



---

Powered by [⚡ ODAC](https://odac.run)

### 🛠️ Fixes & Improvements

- Cli Apps count



---

Powered by [⚡ ODAC](https://odac.run)

### ⚙️ Engine Tuning

- improve email validation regex to better handle domain name structures.

### 🛠️ Fixes & Improvements

- automatic restart for containers with stale API credentials.
- **mail:** improve email validation and switch IMAP encoding to Base64
- **server:** prevent startup crash by enforcing strict apps config validation



---

Powered by [⚡ ODAC](https://odac.run)

### ⚙️ Engine Tuning

- replace Service module with App module

### ✨ What's New

- add support for installing 3rd party services
- prioritize npm start script for website execution
- **proxy:** Add Zst, Brotli and Gzip compression with sync.Pool optimization
- **proxy:** enforce automatic https redirection with ip exceptions
- **proxy:** harden security headers and improve ip masking
- Remote app creation and build system overhaul
- replace Node.js proxy with high-performance Go implement…
- Robust Zero-Downtime Update System, Detached Proxy Management & Stability Improvements
- **server:** enhance Docker container integration and stability
- Transition to WebSocket architecture, Proxy optimizations, and Container stats
- **web:** add automatic build step during website startup

### 🛠️ Fixes & Improvements

- Cli Texts
- handle ECONNRESET on API sockets to prevent crash
- Prevent server crashes from uncaught network errors and optimize HTTPS connection reuse with a persistent agent.
- prevent zombie proxy processes and improve lifecycle management
- **proxy:** resolve build issues and cleanup unused imports
- **proxy:** skip compression for WebSocket and SSE streams



---

Powered by [⚡ ODAC](https://odac.run)

### 🛠️ Fixes & Improvements

- remove conditional checks for Docker build steps



---

Powered by [⚡ Odac](https://odac.run)

### ⚙️ Engine Tuning

- 'for' and 'list' directive argument handling
- Hub polling
- Proxy logic
- rebrand from CandyPack to Odac
- remove deprecated 'client' module from monitor list
- remove framework and move to separate repository
- View rendering to support async operations

### ⚡️ Performance Upgrades

- optimize Docker image with multi-stage build
- **server:** optimize TLS handshake with certificate caching and faster ciphers
- **server:** optimize TLS handshake with certificate caching and faster ciphers

### ✨ What's New

- Add <candy:form> system with automatic validation and DB insert
- Add cloud integration
- Add DDoS Protection Firewall
- add Docker Hub release automation
- add Docker support for containerized deployment
- add environment variable support for Docker volumes
- Add skeleton-aware navigation and auto-navigation support
- Add support for controller classes and update docs
- **framework:** add Early Hints (HTTP 103) support with zero-config
- **Framework:** Add middleware support
- WebSocket Support

### 📚 Documentation

- Add Candy.Var utility documentation
- clarify difference between candy get and var tags
- split template syntax into separate detailed pages

### 🛠️ Fixes & Improvements

- add GitHub Actions permissions for semantic-release
- Add input validation to Auth.check method
- Add memory-safe timers and auto-cleanup for streaming
- add missing dependencies for server components
- Add WebSocket cleanup
- Adjust route reload timing and cache invalidation
- Enable object stringification in MySQL connection
- escape backslash characters in View.js regex replacements
- Escape backticks earlier in view content processing
- Escape backticks in cached view templates
- File type check in Route controller path logic
- Handle null and undefined in Var.html()
- Improve Docker compatibility
- Preserve template literals in <script:candy> blocks
- Prevent replace error when candy get value is undefined
- Refactor route controller lookup and page handling
- Support view config object in authPage third parameter


### 💥 BREAKING CHANGES

- rebrand from CandyPack to Odac (#88)


---

Powered by [⚡ Odac](https://odac.run)

### ⚙️ Engine Tuning

- Modular Config
- New default web template

### ✨ What's New

- add <odac:login> component for zero-config registration
- add <odac:register> component for zero-config registration
- Environment variable support
- **Framework:** Support multiple validation checks per field
- HTTP2 & Server-Sent Events (SSE) Support
- Modernize view template syntax with <odac> tags
- No-Code AJAX Navigation

### 📚 Documentation

- Add examples for complex MySQL where conditions
- Expand and update view system documentation
- Revamp database docs: connection and queries

### 🛠️ Fixes & Improvements

- Added config module to odac debug
- Improve config force save and migration verification



---

Powered by [🍭 Odac](https://odac.run)

### ⚙️ Engine Tuning

- Refactor SSL certificate generation and error handling

### ✨ What's New

- Development Server Mode
- **Framework:** Custom Cron Jobs

### 📚 Documentation

- Simplify and standardize documentation titles

### 🛠️ Fixes & Improvements

- Add CAA record support and default Let's Encrypt CAA
- Enhance system DNS config to use public resolvers
- Handle systemd-resolved conflict on DNS port 53
- Improve external IP detection with multiple fallbacks
- Log stderr output to log buffer with timestamp
- Make husky prepare script non-failing
- Skip rate limiting for localhost in DNS requests



---

Powered by [🍭 Odac](https://odac.run)

### 🛠️ Fixes & Improvements

- Server Startup



---

Powered by [🍭 Odac](https://odac.run)

### Refactor

- Improve IMAP and SMTP authentication and TLS handling

### ⚙️ Engine Tuning

- Improve error handling for HTTP/HTTPS server startup
- Refactor documentation files

### ✨ What's New

- Added 'odac service delete' command.
- Added 'odac subdomain delete' command.
- Added CLI prefix arguments support
- CLI Mouse Support
- **cli:** Add Progress-Based Output

### 🛠️ Fixes & Improvements

- Fix module instantiation and nullish services handling
- Limit log and error buffer sizes in Web and Watchdog

---

Powered by [🍭 Odac](https://odac.run)

**v**

### ✨ What's New

- Added 'odac web delete' command.
- Added German, Spanish, French, Portuguese, Russian, and Chinese language support for CLI

### 🛠️ Fixes & Improvements

- DNS Server Improvements
- Fixed website creation problem
- **Framework:** Add error handling for config file parsing
- IMAP Server Improvements
- Refactor website creation and config handling
- **Server:** Logs fixed
- SMTP Server Improvements
- Web Server Improvements

---

Powered by [🍭 Odac](https://odac.run)

**v**

## [0.5.0](https://github.com/Odac/Odac/compare/v0.4.1...v0.5.0) (2025-09-02)

- add view helper to page and authPage methods [#20](https://https://github.com/Odac/Odac/pull/20) ([](https://github.com/Odac/Odac/commit/863eaf35950831dd1166b6b2d0a50f48c19af508)), closes [#20](https://github.com/Odac/Odac/issues/20)
- merge branch 'main' into dev-1 ([](https://github.com/Odac/Odac/commit/5c96b1e0e5e0a3a86172ef6ad047cdf6f0c5c029))
- merge pull request #14 from Odac/doc/update-contribution-guidelines ([](https://github.com/Odac/Odac/commit/eb92bdf66537c5a973629b99762424e31d8fa3a1)), closes [#14](https://github.com/Odac/Odac/issues/14)
- merge pull request #16 from Odac/feat/custom-semantic-release-notes ([](https://github.com/Odac/Odac/commit/3ea998e24c27e03878258ac0de8563223dced2f8)), closes [#16](https://github.com/Odac/Odac/issues/16)
- merge pull request #21 from Odac/dev-1 ([](https://github.com/Odac/Odac/commit/ae87fd8d50a226d854a8b85345558d7d5b7e81f9)), closes [#21](https://github.com/Odac/Odac/issues/21)
- merge pull request #23 from Odac/dev ([](https://github.com/Odac/Odac/commit/b28a237ea070bc66f9ea3dbc2efa967e4d73ab27)), closes [#23](https://github.com/Odac/Odac/issues/23)
- merge pull request #25 from Odac/dev ([](https://github.com/Odac/Odac/commit/b9cb9127d64c80d03c407052124b40da86b7caec)), closes [#25](https://github.com/Odac/Odac/issues/25)
- refactor server restart and initialization logic ([](https://github.com/Odac/Odac/commit/2e7a23fce515b6a6e6fdfb58e91810d558001c66))

### ⚡️ Performance Upgrades

- core Process Module ([](https://github.com/Odac/Odac/commit/59ece15756bfb80a3ce8a430d60106ec8b9cea7e))

### ✨ What's New

- **release:** customize semantic-release notes generation ([](https://github.com/Odac/Odac/commit/70ba94ef010c10609cce69d2dd7fc32eb8ddd157))
- **server:** new Logging Module ([](https://github.com/Odac/Odac/commit/70699006a36dbcce1f8de3ad549ee6000cd6e3a1))
- synchronize main with dev branch ([](https://github.com/Odac/Odac/commit/654ecd33e9296f3753b95f11568f9d289f0d5a23))

### 🎨 Style

- add PR and author attribution to release notes [#17](https://https://github.com/Odac/Odac/pull/17) ([](https://github.com/Odac/Odac/commit/8ef3e77edb3fc3f6de4582bacf8c3088f5941b76)), closes [#17](https://github.com/Odac/Odac/issues/17) [#1234](https://github.com/Odac/Odac/issues/1234) [#1234](https://github.com/Odac/Odac/issues/1234)

### 📚 Documentation

- add AGENTS.md and update contribution guidelines ([](https://github.com/Odac/Odac/commit/2ef7dcb125eb9b341ecf209cd6f87dc6e0873389))
- framework Docs ([](https://github.com/Odac/Odac/commit/cd7a1cec6ebecc5748bae3ccd0dd934ddf77c3fa))
- server documentation ([](https://github.com/Odac/Odac/commit/27eb89a0f9c5d17c0364112743f3a032d2999195))
- update AGENTS.md with detailed developer guide [#18](https://https://github.com/Odac/Odac/pull/18) ([](https://github.com/Odac/Odac/commit/65a05649ea61ac1d29ae73e3f27dabe0d82425cd)), closes [#18](https://github.com/Odac/Odac/issues/18)

### 🛠️ Fixes & Improvements

- add missing conventional-changelog-conventionalcommits dependency [#22](https://https://github.com/Odac/Odac/pull/22) ([](https://github.com/Odac/Odac/commit/8f8a64fe3a81a5c9015e62755013d141571e5f0d)), closes [#22](https://github.com/Odac/Odac/issues/22)
- **CLI:** fixed CLI Commands ([](https://github.com/Odac/Odac/commit/44c8ef7a2aca9ebdf82317b20dc3c39728cf2676))
- **Framework:** no Controller View ([](https://github.com/Odac/Odac/commit/ae73ae793be0e3b6b6d95eaeb75ba981f0eb49f5))
- rebooting ([](https://github.com/Odac/Odac/commit/9ab17df2a39621be0429cc3ffd52f0326dc8c64a))
- resolve semantic-release configuration and dependency errors [#24](https://https://github.com/Odac/Odac/pull/24) ([](https://github.com/Odac/Odac/commit/b6359b4687f1f15cbb1847e82d49a998e74f5dde)), closes [#24](https://github.com/Odac/Odac/issues/24)

## [0.4.1](https://github.com/Odac/Odac/compare/v0.4.0...v0.4.1) (2025-08-31)

### Bug Fixes

- **server:** Refactor server restart and initialization logic ([#15](https://github.com/Odac/Odac/issues/15)) ([6cc688e](https://github.com/Odac/Odac/commit/6cc688ed95212fa73d022e3f2d8e773a17fe299e))

# [0.4.0](https://github.com/Odac/Odac/compare/v0.3.1...v0.4.0) (2025-08-28)

### Bug Fixes

- Correct license and supported version in docs ([1af458e](https://github.com/Odac/Odac/commit/1af458ead8a1a577e8e4c6c45d8cae4ee1432d5c))
- Update auto-generated PR text to English ([27bab61](https://github.com/Odac/Odac/commit/27bab61461d65737f95e89737bda06753de3bfc5))

### Features

- Add Jest for testing ([4e51bbb](https://github.com/Odac/Odac/commit/4e51bbb1295d44b14f64e661019ac7a526fa96bb))
- Add Jest for unit testing and code coverage ([1bfe78e](https://github.com/Odac/Odac/commit/1bfe78eeabc0af1178ba9bc678ab8a2eaa7d9bf8))
- add semantic-release ([7a88e9e](https://github.com/Odac/Odac/commit/7a88e9eb3cee12d6d316c776e7d88e8de6b26c55))
- add semantic-release with npm publishing ([5c2568d](https://github.com/Odac/Odac/commit/5c2568dac9ca6284718e087ce67a519a21bfe1c5))
- Complete project refactor and feature enhancement ([d3bcc19](https://github.com/Odac/Odac/commit/d3bcc1995af8f0548a0bbd8c0396db6775d4b5cf))
