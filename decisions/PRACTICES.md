# Development Practices

Lean by design. Add sections only when a real convention emerges.

## Testing

- The gate is **87%**, and the absolute number is not the point: judge a change by measuring the same profile before and after it, never by either figure alone. Coverage is a floor, not the goal. Tests must exercise real behaviour; a test that only proves a mock was called is not worth writing.
- Prefer, in order: pure-function unit tests → tests against fakes at package boundaries (`sandbox.Provider`, `git.Platform`, credential store) → integration tests behind build tags/env flags that hit real Modal, Forgejo or GitHub.
- Keep fakes small, hand-written and shared (`internal/<pkg>/fake.go`); avoid generated mock frameworks.
- Test error paths and boundaries (timeouts, missing config, version mismatch, empty password) — they are where this tool fails in practice.
- Use Go table-driven tests and `t.Run` subtests; `go test -race ./...` must pass.

## Code Style

- Standard Go: `gofmt`, `go vet`, `golangci-lint` with defaults. Wrap errors with `%w` and context.
- Secrets never appear in logs, CLI arguments, or error messages.

## Architecture

- External systems (Modal, Forgejo, GitHub, keyring, OpenCode HTTP API) sit behind small internal interfaces; SDK types do not leak past their package.
- Annotate code implementing a decision with `@decision(DL-NNN)`.
- A comment says what the code does and anything local and non-obvious; the rationale, measurements and history live in the decision record. Do not restate a record in a comment or in AGENTS.md. If a comment needs a paragraph of why, add it to the record instead. `scripts/comment-blocks.py` fails on a block over 20 lines; `--top 20` lists the longest.
