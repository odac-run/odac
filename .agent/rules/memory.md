---
trigger: always_on
---

# User Preferences & Project Rules

## Communication & workflow
- **Proactive Warning:** If the user requests a change that could compromise system stability, security, or critical recovery mechanisms (like watchdogs), EXPLICITLY warn them about the potential side effects before proceeding or alongside the implementation.

## Code Style
- **Lifecycle Naming:** Use `Start()` and `Stop()` for service initialization and termination methods. Do not use `Open/Close` or `Init/Destroy` unless dictated by a specific library, to maintain project consistency.

- **Mandatory Static Verification:** After ANY code modification, you MUST run `gofmt -l .` (must print nothing) and `go vet ./...` and fix ALL findings until both succeed (exit code 0). Do not assume the code is correct; verify it.
- **Non-Blocking Hot Paths:** Never block the server's periodic check ticks or request-serving paths with slow I/O; dispatch long-running work (builds, pulls, probes) onto tracked goroutines with cancellation, keeping shared state mutex-guarded and `-race` clean.
- **Hierarchical Logging:** Sub-modules must log with their module prefix (`[Parent] ...` / `[Parent Child] ...`) via the project's log helpers, to ensure clear log tracing.

## Architectural Principles (Non-Negotiable)
- **Root Cause over Patching:** Never implement local "quick fixes" for systemic requirements (logging, auth, validation). Fix the shared `internal/` package instead of patching individual call sites.
- **Centralization:** Features cutting across multiple packages must be implemented centrally. If a helper function is needed in two places, move it to a shared `internal/` package immediately (e.g. `internal/netutil`).
- **Enterprise Mindset:** Solutions must be scalable and modular. Ask: "Will this implementation hold up if the codebase grows 10x?" Avoid temporary hacks.
- **No Hardcoding / Environment Agnostic:** NEVER hardcode local paths, environment-specific directories (e.g., `/Users/...` or `/root/...`), or sensitive data. All configuration MUST come from environment variables or the config store (`internal/config`). Temporary hardcoded fixes are strictly forbidden.
- **Narrow Interfaces:** Packages access collaborators through the narrow interfaces wired in `cmd/odac-server/main.go` (the composition root) instead of importing concrete siblings. This keeps packages testable with fakes and prevents dependency cycles.
- **Zero-Config Philosophy:** The system must infer as much configuration as possible (e.g., auto-detecting ports from Docker images). Do not ask the user for configuration unless absolutely necessary. Defaults should be intelligent and production-ready.
- **Native Builder:** Use the internal builder (`internal/docker`) for building Docker images. DO NOT use Nixpacks or external builders. The project has its own optimized build pipeline.
- **Unified Hub Commands:** In `internal/hub`, periodic background tasks and on-demand commands are unified into the command table. Tasks are identified by having an interval. Use `trigger(name)` to manually execute a command as a task (broadcasting result) or `processCommand` for individual request-response.
- **Deliberate Ordering:** Keep member ordering tidy, but NEVER re-sort wire/protocol-relevant tables — the hub command table preserves its original insertion order (protocol parity), and several payloads pin exact key order.

## Project Context
- **Environment:** The system operates within a containerized environment (Docker/K8s). All code must be container-aware (handle PID 1 signals, respect read-only filesystems, use env vars for config).
- **Storage Architecture:** ODAC relies on **Host Bind Mounts** (not Named Volumes) for application data. This is mandatory for the Native Builder to function, as it needs to resolve and mount the exact Host Path of the source code.
    - **Production Standard:** Data must be stored in `/var/lib/odac` (compose default, override via `ODAC_DATA_DIR`).
    - **Development:** Can use local directory (`.`), but must be aware of Host Path resolution (`ODAC_HOST_ROOT`).
    - **Privileged Mode:** Builder containers may require `Privileged: true` if the project resides in restricted directories (like `/root`).

## Security
- **Log Sanitation:** Never log raw configuration objects or environment variables. Always sanitize sensitive fields (password, token, key, secret, auth, env) before logging.

## Architectural Updates
- **Zero Downtime Deployments (ZDD):** All redeployments and container swaps must be executed using the Blue-Green architectural model. Never kill the active container until the new container has fully initialized, passed readiness checks (acquired an IP/Port), and the ODAC Proxy has been explicitly synced to drain traffic.
