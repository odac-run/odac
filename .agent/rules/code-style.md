---
trigger: always_on
---

- **Formatting & Vet:** After making changes, always run `gofmt` (no diffs allowed) and `go vet ./...`; resolve any findings.
- **Testing:** After making significant changes, always run the tests (`go test ./...`) to ensure no regressions. Resolve any errors found.
- **Comments:** Minimize code comments; use them only when absolutely necessary to explain complex logic. Always write code comments in **English**.
- **Imports:** Use standard `import` blocks at the top of the file, grouped stdlib / external / `odac/...` (goimports order).
