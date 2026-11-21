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

[`AGENTS.md`](../AGENTS.md) is the map of the codebase, the rules for each
area, and the tests that guard them. Releasing is in
[releasing.md](releasing.md).

## Decision-Linked Development (DLD)

spov is developed with Decision-Linked Development: every design choice that
matters is a short decision record, and the code that carries it points back
to it. The record holds the why (the context, what was decided, the rationale,
the measurements and the consequences), so the code's comments say only what
the code does.

**Where things are:**

- `decisions/records/<namespace>/DL-NNN.md`: one record each, Markdown with YAML
  frontmatter (`id`, `title`, `status`, `amends`, `supersedes`, `namespace`,
  `tags`, `references`). The namespaces are `cli`, `sandbox`, `opencode`,
  `git`, `server`, `jobs` and `web` ([`dld.config.yaml`](../dld.config.yaml)).
- [`decisions/INDEX.md`](../decisions/INDEX.md): every record by ID, title,
  status and tags, generated.
- [`decisions/PRACTICES.md`](../decisions/PRACTICES.md): the project's testing,
  style and architecture conventions, which every change follows.
- `@decision(DL-NNN)` in a comment marks the code a record governs, at the
  function, type or constant level, one ID per annotation.

**How a record is used:**

- Before changing annotated code, read the record it names; `/dld-lookup
  DL-NNN` (below) or the file itself.
- A change that goes against a record needs a new record that **amends** it
  (changes part of it, which stays `accepted`) or **supersedes** it (replaces
  it, which becomes `superseded`). The prose of a record already on `main` is
  not edited (`decision_edits` is `block` by default); its frontmatter, such
  as `references` or `status`, may be.
- A record is `proposed` until its code lands, then `accepted`. A record that
  is not on `main` yet is a draft and may be rewritten freely.
- A new decision gets the next free ID, its own record, and annotations on the
  code that implements it, before the change is merged.

**Tooling.** The DLD skills under `.claude/skills/` run the workflow from an
agent (Claude Code picks them up as slash commands): `/dld-lookup` (by ID, tag
or code path), `/dld-decide` (record one decision), `/dld-plan` (break a
feature into several), `/dld-implement` (implement proposed decisions, annotate
and accept them), `/dld-adjust`, `/dld-audit` (drift between records and code)
and `/dld-status`. Their CLI also works by hand, with Node.js 22 or newer:

```sh
node .claude/skills/dld-common/scripts/dld.mjs next-id                  # the next free DL-NNN
node .claude/skills/dld-common/scripts/dld.mjs verify-annotations DL-175 # each ID annotated in the code
node .claude/skills/dld-common/scripts/dld.mjs regenerate-index         # rewrite decisions/INDEX.md
```

`scripts/comment-blocks.py`, run by `check.sh`, fails on a comment block over
20 lines: a comment that needs a paragraph of why means the record is missing
it.

**The first records are a consolidation.** spov was developed in another
repository before it moved here, and the decisions made there were merged into
the first batch of records on the move. That is why the records up to DL-069
all carry the same midnight timestamp (`2026-11-04T00:00:00Z`) rather than the
moment each was made, and why many of them are long: one record often holds
what were several decisions, with their later amendments and measurements
folded in. From DL-070 on, each record is one decision, timestamped when it
was recorded, and a change to an accepted one is a new record that amends or
supersedes it.
