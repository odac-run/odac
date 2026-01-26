# User Preferences & Project Rules

## Communication & workflow
- **Proactive Warning:** If the user requests a change that could compromise system stability, security, or critical recovery mechanisms (like watchdogs), EXPLICITLY warn them about the potential side effects before proceeding or alongside the implementation.

## Code Style
- **Lifecycle Naming:** Use `start()` and `stop()` for service initialization and termination methods. Do not use `open/close` or `init/destroy` unless dictated by a specific library, to maintain project consistency.
