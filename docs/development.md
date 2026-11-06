# Developing spov

## Building from source

Most people install a release (see the [README](../README.md#get-started)).
To build spov yourself you need Go 1.26 or newer; the dashboard is built with
Bun:

```sh
scripts/install-bun.sh       # if bun is not installed
scripts/build-web.sh         # builds the dashboard into the binary
scripts/build-proxy.sh       # builds the credential proxy into the binary
go build -o ~/.local/bin/spov ./cmd/spov
```

Run both build scripts again whenever you rebuild. A plain `go build` without
`build-web.sh` works too, but embeds a placeholder page instead of the
dashboard, and `spov serve` says so when it starts. Without `build-proxy.sh`
the binary has no credential proxy: a local server then launches sandboxes
with their tokens and keys inside the main container and warns about it, and a
hosted server refuses to start ([The sandbox](sandbox.md#credentials-stay-outside-the-sandbox)).
`spov version --embedded` says which of the two a binary carries.

A source build does not install OpenCode. `install.sh --with-opencode` does,
or OpenCode's own installer (`curl -fsSL https://opencode.ai/v2/install | bash`);
see [The OpenCode client](sessions.md#the-opencode-client).

## Checks

```sh
./scripts/check.sh                     # the commit gate: Go, web and coverage checks
git config core.hooksPath .githooks    # run it before every push

cd web && bun run dev                  # the dashboard against a separate spov serve
```

`check.sh` runs what CI runs: gofmt, vet, the comment-block limit, the
integration typecheck, the API client drift gate, the web checks,
golangci-lint, `go test -race` and the coverage gate.

## Integration tests

Behind the `integration` build tag. The Modal ones create real, billable
sandboxes and terminate what they create:

```sh
go test -tags integration ./internal/sandbox/ -run Integration -v
```

## The codebase

Decisions live in `decisions/` and are linked from the code with
`@decision(DL-NNN)` annotations. If you are about to change annotated code,
read the decision first. [`AGENTS.md`](../AGENTS.md) is the map of the
codebase, the rules for each area, and the tests that guard them. Releasing is
in [releasing.md](releasing.md).
