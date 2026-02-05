---
trigger: always_on
---

# User Preferences & Project Rules

## Communication & workflow
- **Proactive Warning:** If the user requests a change that could compromise system stability, security, or critical recovery mechanisms (like watchdogs), EXPLICITLY warn them about the potential side effects before proceeding or alongside the implementation.

## Code Style
- **Lifecycle Naming:** Use `start()` and `stop()` for service initialization and termination methods. Do not use `open/close` or `init/destroy` unless dictated by a specific library, to maintain project consistency.

- **Mandatory Lint Verification:** After ANY code modification, you MUST run the linter (`npm run lint` or `npx eslint <file>`) and fix ALL errors until the command returns success (exit code 0). Do not assume the code is correct; verify it.
- **Non-Blocking I/O:** Always use asynchronous, non-blocking methods for File System operations (e.g., `fs/promises` or `fs.readFile` with callback) instead of synchronous ones (`fs.readFileSync`, `fs.existsSync`) in server-side code to prevent Event Loop blocking, unless purely for startup initialization.
- **Hierarchical Logging:** Sub-modules must use hierarchical log initialization, e.g., `Odac.core('Log').init('Parent', 'Child')`, to ensure clear log tracing.

## Architectural Principles (Non-Negotiable)
- **Root Cause over Patching:** Never implement local "quick fixes" for systemic requirements (logging, auth, validation). Modify the Core foundation (`core/`) instead of patching individual files.
- **Centralization:** Features cutting across multiple modules must be implemented centrally. If a helper function is needed in two places, move it to a shared Utility or Core Class immediately.
- **Enterprise Mindset:** Solutions must be scalable and modular. Ask: "Will this implementation hold up if the codebase grows 10x?" Avoid temporary hacks.
- **No Hardcoding / Environment Agnostic:** NEVER hardcode local paths, environment-specific directories (e.g., `/Users/...` or `/root/...`), or sensitive data. All configuration MUST come from Environment Variables or the `Config` provider. Temporary hardcoded fixes are strictly forbidden.
- **Service Locators:** Always use the ODAC service locator patterns (`Odac.server('Name')`, `Odac.core('Name')`) to access cross-module singletons instead of using local `require` paths. This ensures access to initialized instances and prevents relative path errors.


## Project Context
- **Environment:** The system operates within a containerized environment (Docker/K8s). All code must be container-aware (handle PID 1 signals, respect read-only filesystems, use env vars for config).
- **Storage Architecture:** ODAC relies on **Host Bind Mounts** (not Named Volumes) for application data. This is mandatory for the Native Builder to function, as it needs to resolve and mount the exact Host Path of the source code.
    - **Production Standard:** Data must be stored in `/var/odac`.
    - **Development:** Can use local directory (`.`), but must be aware of Host Path resolution (`ODAC_HOST_ROOT`).
    - **Privileged Mode:** Builder containers may require `Privileged: true` if the project resides in restricted directories (like `/root`).

## Security
- **Log Sanitation:** Never log raw configuration objects or environment variables. Always sanitize sensitive fields (password, token, key, secret, auth, env) before logging.