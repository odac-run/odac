---
trigger: always_on
---

# User Preferences & Project Rules

## Communication & workflow
- **Proactive Warning:** If the user requests a change that could compromise system stability, security, or critical recovery mechanisms (like watchdogs), EXPLICITLY warn them about the potential side effects before proceeding or alongside the implementation.

## Code Style
- **Lifecycle Naming:** Use `start()` and `stop()` for service initialization and termination methods. Do not use `open/close` or `init/destroy` unless dictated by a specific library, to maintain project consistency.

- **Mandatory Lint Verification:** After ANY code modification, you MUST run the linter (`npm run lint` or `npx eslint <file>`) and fix ALL errors until the command returns success (exit code 0). Do not assume the code is correct; verify it.

## Architectural Principles (Non-Negotiable)
- **Root Cause over Patching:** Never implement local "quick fixes" for systemic requirements (logging, auth, validation). Modify the Core foundation (`core/`) instead of patching individual files.
- **Centralization:** Features cutting across multiple modules must be implemented centrally. If a helper function is needed in two places, move it to a shared Utility or Core Class immediately.
- **Enterprise Mindset:** Solutions must be scalable and modular. Ask: "Will this implementation hold up if the codebase grows 10x?" Avoid temporary hacks.


## Project Context
- **Environment:** The system operates within a containerized environment (Docker/K8s). All code must be container-aware (handle PID 1 signals, respect read-only filesystems, use env vars for config).
- **Architecture:** ODAC is a hybrid system (Node.js Core + Go Proxy). It acts as a zero-dependency solution, excluding external DBs (Redis, Postgres), proxies (Nginx, Traefik), and DNS.

## Security
- **Log Sanitation:** Never log raw configuration objects or environment variables. Always sanitize sensitive fields (password, token, key, secret, auth, env) before logging.