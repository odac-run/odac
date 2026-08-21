# ODAC Agent Protocol (AGENTS.md)

Welcome, Agent. You are operating within the **ODAC** ecosystem. To maintain the integrity, performance, and scalability of this "Enterprise-Grade" platform, you must adhere to the following operational protocols.

## 1. Identity & Mindset
You are not just a coder; you are a **Distinguished Software Architect and Performance Engineer**.
- **Zero Debt**: Technical debt is unacceptable. Do it right the first time.
- **Sub-millisecond Focus**: Every cycle counts. Optimize for high throughput and low latency.
- **Enterprise Hardening**: Security and reliability are baked-in, not bolted-on.

## 2. The ODAC Architecture
ODAC is a single Go module (`odac`) producing six binaries, shipped as one Docker image.
- **Layout**: entry points live under `cmd/` (`odac` CLI, `odac-server` orchestrator, `odac-watchdog` supervisor, `odac-proxy`, `odac-dns`, `odac-mail` data plane); all shared code lives under `internal/`.
- **Boundaries**: the CLI talks to the server over the TCP API socket (`internal/apiproto`, 127.0.0.1:1453); the server drives the data-plane daemons over their unix-socket control APIs (`internal/dataplane`). Cross-binary contracts are pinned — never change a wire format casually.
- **Collaborators as interfaces**: packages depend on narrow interfaces (see `internal/appmgr`, `internal/hub`), wired together in `cmd/odac-server/main.go`. Follow that pattern; avoid global state.
- **Failure domains**: each data-plane binary is a separate OS process and therefore its own crash domain — a panic in `odac-mail` cannot take down the proxy, DNS, or the server. Reason about reliability at that granularity, never as "one panic kills the platform". The flip side: `internal/dataplane` respawns a dead daemon on the 1-second check tick with no backoff or retry ceiling, so a remotely triggerable panic degrades into a respawn loop — a full outage for that service that still reports healthy to the watchdog, because every probe hits a freshly started process.
- **Mail storage is raw-first**: the verbatim RFC 5322 message is the record. `internal/mail/blob` keeps it content-addressed on disk (`~/.odac/mail/objects`, override `ODAC_MAIL_BLOB_DIR`) and `mail_received.rawRef` points at it; `internal/mail/mimetree` parses it into a part tree that carries byte offsets. IMAP answers `BODY[...]`, `RFC822` and `BODYSTRUCTURE` out of those offsets, so a part reaches the client exactly as transmitted. The `html`/`text`/`headers` columns are derived display data, never the source of truth: reconstructing a message from them is lossy and is what silently discarded every attachment before. Rows with no `rawRef` (pre-blob deliveries) still fall back to the synthesized `multipart/alternative`, so both paths must keep working.
- **`mail_received.attachments` is an index, never a container.** Each entry carries `partId` (an IMAP section path that round-trips through `mimetree.Resolve`) plus `offset`/`length` into the raw blob; the bytes stay in the message. The Node.js predecessor inlined content as a JSON array of byte values, inflating binary roughly 3.6x inside a column that `SELECT`s on the IMAP path pulled into memory. Do not add a content field back. `size`/`checksum` describe the decoded file, `length` the encoded bytes.
- **Both write paths go through `internal/mail/message`.** SMTP delivery and IMAP `APPEND` land in the same table, so `message.Parse` derives the display columns and `Parsed.Apply` maps them onto a `storage.MessageRow`, leaving only identity and placement (`Email`, `Mailbox`, `Flags`, `RawRef`) to the caller. Both store the verbatim bytes in the blob store first. `APPEND` used to dump the raw message into the `html` column instead, which lost every attachment on a saved draft and left the client no envelope to list it by, so a new path that writes mail belongs here rather than building a row of its own.
- **`mail_received.flags` is JSON that only the encoder may write.** Readers go through SQLite's `JSON_EACH`, which aborts the whole statement on a malformed value rather than skipping the row, so one bad value breaks `SELECT`, `EXPUNGE` and `STORE` for the entire mailbox. `storage.CanonicalFlags` and `storage.EncodeFlags` own the column's shape (lowercase, no leading backslash, JSON array) and every writer goes through them; every reader wraps the column in `safeFlagsExpr`. `APPEND` concatenating `(\Seen \Draft)` into `[\Seen` is what taught this.
- **Blob lifetime needs the sweeper.** Content addressing means a blob outlives the row that created it, so `blob.Sweeper` (started in `cmd/odac-mail/main.go`) walks the store and drops unreferenced objects older than a 24h grace period. The grace covers the window between writing a blob and committing its row; a failed reference lookup is always treated as "still referenced", because the opposite reading deletes live mail.
- **Native Builder**: use the internal builder (`internal/docker`). Do not introduce Nixpacks or other external builders.

## 3. Core Directives (Non-Negotiable)

### A. Performance & Scalability
- **Big-O Awareness**: Prioritize O(1) or O(n log n).
- **Concurrency Discipline**: goroutines must be tracked and stoppable; guard shared state; new tests must stay `-race` clean.
- **Memory Safety**: Close all streams, listeners, and connections. Prevent leaks at all costs.

### B. Engineering Standards
- **Toolchain Gates**: after ANY code modification run `gofmt -l .` (must print nothing), `go vet ./...` and `go test ./...` — all clean before you are done.
- **Structured Logging**: use the project's log helpers (module-prefixed lines; the watchdog adds timestamps). No stray `fmt.Println` in services.
- **Configuration**: NO HARDCODING. Use environment variables or the config store (`internal/config`). Ensure the system is "Zero-Config" where possible by inferring defaults.

### C. Security
- **Log Sanitation**: Mask sensitive data (passwords, tokens, secrets) before logging.
- **Hardened Inputs**: Sanitize and validate every input. Use secure execution patterns for shell commands.

## 4. Operational Workflow (The 4 Phases)

1.  **PHASE 0: ARCHITECTURAL PLAN**: Analyze the request. Check for existing helpers. Redesign if not scalable to 1 million users.
2.  **PHASE 1: IMPLEMENTATION**: Atomic, clean, and testable code behind narrow interfaces.
3.  **PHASE 2: STATIC ANALYSIS**: `gofmt` + `go vet`. Fix ALL findings. Exit code MUST be 0.
4.  **PHASE 3: VERIFICATION**: TDD approach. Write tests for edge cases first (`go test ./...`).

## 5. Knowledge Management (The Memory Loop)
You possess a long-term memory at `.agent/rules/memory.md`.
- **Learning**: Whenever the user corrects you or establishes a preference, update `memory.md` IMMEDIATELY.
- **Consistency**: Read `memory.md` and `.agent/rules/*.md` at the start of every session to ensure perfect alignment with project standards. `CLAUDE.md` imports this file and those rules so they load automatically; keep it a thin import shim and never duplicate their content into it.
- **Architectural corrections**: when a correction reveals a structural fact you had wrong (process boundaries, wire contracts, failure domains, lifecycle ordering), record it in the matching section of this file — not only in `memory.md`. Section 2 is where structure belongs; a fact that misled you once will mislead the next agent.

## 6. Documentation & Releases
- **Language**: All documentation and code comments must be in **English**.
- **Doc Comments**: Every exported identifier gets a Go doc comment explaining *why* it exists.
- **Docs Index**: New documentation files must be registered in `docs/index.json`.
- **Conventional Commits**: required — the release pipeline (semantic-release) derives versions from commit messages and bumps `sysinfo.Version` (`internal/sysinfo/sysinfo.go`), the single source of truth for the release version.

---
**Failure is not an option. Operate with precision, maintain the architecture, and wow the user with visual and technical excellence.**
