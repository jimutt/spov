# spov

**Run coding agents in disposable cloud sandboxes, on your own repositories.**

spov starts an [OpenCode](https://opencode.ai) agent in a fresh
[Modal](https://modal.com) sandbox with only the repositories you choose,
connects your terminal to it, and brings the agent's work back to your
checkout when you are done. Or hand it a goal, and it works on its own until it
has opened a pull request or written a report.

```sh
cd your-repo
spov fly
```

> **Early days.** spov is young: everything described here works and is
> exercised against real Modal, GitHub, GitLab and Forgejo, but its interfaces
> may still change between releases.

## Why spov

- **Not on your laptop.** The agent works in a sandbox of its own, with the
  repositories it needs and nothing else: not your home directory, your SSH
  keys or your other projects. Each sandbox is thrown away afterwards.
- **Your credentials stay out of its reach.** Git tokens, model API keys and
  registry tokens are held by a proxy beside the sandbox, which adds each only
  to the requests it is meant for. The agent sees placeholders.
- **Your work comes back to you.** Inside a checkout, `spov fly` starts from
  your current branch, unpushed commits included, and copies the agent's
  commits and changes back when you quit. Nothing is overwritten.
- **Hand off whole tasks.** A job works unattended until it opens a pull
  request or writes a report, and spov checks the result itself (the pull
  request exists, your check command passes) before calling it a success.
- **No sandbox outlives its use.** Modal bills a sandbox for as long as it
  exists, so spov ends it when you quit, idles it out when nobody is attached,
  and gives every sandbox a hard time limit.
- **Terminal or browser.** The OpenCode TUI you know, or OpenCode's web UI;
  with a [server](docs/server.md), also a dashboard to start, watch and steer
  sessions and jobs.

## Get started

You need:

- Linux or macOS (on Windows, WSL) with `git`;
- a [Modal](https://modal.com) account, where the sandboxes run and are billed;
- a GitHub account, with [`gh`](https://cli.github.com) signed in on this
  machine or a GitHub App (Forgejo, Gitea and GitLab work too:
  [credentials.md](docs/credentials.md#forgejo-gitea-and-gitlab));
- a model: an API key such as `ANTHROPIC_API_KEY`, or an OpenCode Go or Zen
  subscription.

**1. Install spov**, with the OpenCode 2 version it expects:

```sh
curl -fsSL https://github.com/jimutt/spov/releases/latest/download/install.sh | sh -s -- --with-opencode
```

spov goes to `~/.local/bin` (the script tells you if that is not on your
`PATH`) and OpenCode beside spov's data in `~/.local/share/spov/bin/`. The
[script](scripts/install.sh) checks the download against the release's
checksums, and their signature when `cosign` is installed. Running it again updates spov;
`… | SPOV_VERSION=0.2.0 sh -s -- --with-opencode` pins a release. With Homebrew,
`brew install jimutt/tap/spov` installs spov alone; install OpenCode 2 with
its own installer (`curl -fsSL https://opencode.ai/v2/install | bash`).

**2. Connect Modal**:

```sh
uvx modal setup        # or: pipx run modal setup, or pip install modal && python3 -m modal setup
```

This signs you in and writes `~/.modal.toml`; `MODAL_TOKEN_ID` and
`MODAL_TOKEN_SECRET` work too. Then build the sandbox image once, which takes a
few minutes and is needed again only when spov moves to a new OpenCode
version:

```sh
spov serve image build
```

**3. Connect GitHub.** Create `~/.config/spov/config.yaml`:

```yaml
platforms:
  - name: github
    kind: github
    account: your-username
```

and reuse `gh`'s login (`gh auth login` first if it is not signed in):

```sh
spov credentials add github --from-gh
```

A GitHub App is better still: it gives each session a token for just its own
repositories ([credentials.md](docs/credentials.md#github)).

**4. Add a model.** Store an API key:

```sh
spov credentials add ANTHROPIC_API_KEY    # or OPENROUTER_API_KEY, OPENAI_API_KEY, ...
```

or sign in to OpenCode Go or Zen with `spov login opencode`.

**5. Launch a session** from a checkout of a repository on GitHub:

```sh
cd your-repo
spov fly
```

spov creates a sandbox, clones the repository at your current branch, waits
for OpenCode, and hands your terminal to its TUI; the launch shows its
progress and an estimated cost per hour. Work with the agent as you would
locally. When you quit, its commits and uncommitted changes are copied into
your checkout, and then the sandbox is terminated.

The checkout must be clean and on a branch to start (untracked files count):
commit or stash first, or pass `--no-sync` to launch without syncing. From
any other directory, `spov fly --repo owner/name` launches a repository's
default branch instead.

### When something goes wrong

- **"image not published"**: the sandbox image is not built yet; run
  `spov serve image build`.
- **The agent cannot reach a model**: store a key (step 4). A key stored
  while the local server runs is seen once it restarts, which it does after a
  minute with nothing to do.
- **An OpenCode version mismatch**: run the install command again.
- **Anything else at launch**: the local server's log is
  `~/.local/state/spov/server.log`, and `spov config show` prints the
  configuration spov is using.

## What you can do

### Work with an agent

| | |
|---|---|
| `spov fly` | Start a session, in this checkout or with `--repo owner/name` |
| `spov fly --web` | The same, in OpenCode's web UI |
| `spov ls` | Your sessions, with their state and age |
| `spov attach <name>` | Join a running session again |
| `spov stop <name>` | End one; `--sync` copies its work back first |
| `spov resume <name>` | Reopen a stopped session in a new sandbox |

[Interactive sessions](docs/sessions.md) covers every flag, choosing
repositories, resuming and scripting.

### Hand off a task

```sh
spov jobs create --repo acme/app --check './app/scripts/check.sh' \
  "Fix the flaky retry test in internal/queue and open a pull request"
```

A job runs until it delivers, and is judged on three gates: the check passes,
the pull request or report exists, and the agent did something with no
question left unanswered. You can follow it, steer it or take over at any
point. Jobs need a server with a database, which the Compose file in
[deploy/](deploy/README.md#compose) runs on your own machine; see
[Autonomous jobs](docs/jobs.md).

### Use the dashboard

A server with a database also serves a web dashboard, signed in to with
GitHub, to start, watch and steer sessions and jobs, answer the agent's
questions, and see what their sandboxes cost. See
[The server and the dashboard](docs/server.md).

## How it works

```text
you ── spov (CLI or dashboard) ── spov serve ── Modal sandbox
                                                 ├── OpenCode, your repositories in /workspace
                                                 └── credential proxy, holding the tokens
```

`spov serve` holds the credentials, creates the sandboxes and supervises the
jobs; the CLI and the dashboard are its clients. On your own machine there is
nothing to set up: the CLI starts a server for itself when it needs one. A
team runs one server with Postgres for everyone, which also gives jobs and
the dashboard ([deploy/README.md](deploy/README.md)).

Each sandbox has a general-purpose toolchain (Python, Node, Bun, Go, build
tools) and takes add-ons such as Rust, Java or Postgres. What is installed,
how credentials are kept out of it, network access and cost are in
[The sandbox](docs/sandbox.md).

## Configure it

spov reads `~/.config/spov/config.yaml`, then a project's `.spov.yaml`, then
`SPOV_*` environment variables. A project file says what its repository
needs:

```yaml
# .spov.yaml
sandbox:
  addons: [rust, postgres@16]    # beyond the default toolchain
  env:
    RUST_LOG: debug
```

Commit it before your next `spov fly`, since a synced launch needs a clean
checkout. `spov config show` prints the merged result and where each value
came from. Secrets never live in these files; `spov credentials add` stores
them.
[configuration.md](docs/configuration.md) documents every key.

## Documentation

- [Interactive sessions](docs/sessions.md): starting, syncing, resuming, and
  `--json` output for scripts
- [Autonomous jobs](docs/jobs.md): goals, checks, steering and how a job is
  judged
- [Credentials](docs/credentials.md): GitHub, Forgejo, Gitea and GitLab, model
  providers, private registries
- [The sandbox](docs/sandbox.md): toolchain, add-ons, environment variables,
  security, network, cost
- [The server and the dashboard](docs/server.md): local or hosted, signing in,
  the web dashboard
- [Configuration reference](docs/configuration.md): every key
- Reference: [forges](docs/forges.md), [GitHub access](docs/github-access.md),
  [local history](docs/local-history.md), [metrics](docs/metrics.md),
  [the HTTP API](docs/api/openapi.yaml)
- Running a server: [deploy/README.md](deploy/README.md), and what to decide
  before you share one: [Running a shared server safely](docs/security.md)
- Reporting a vulnerability: [SECURITY.md](SECURITY.md)

## Contributing

<!-- Building from source lives in docs/development.md. @decision(DL-099) -->

Building from source, the checks and the integration tests are in
[docs/development.md](docs/development.md). [`AGENTS.md`](AGENTS.md) maps the
codebase.

## Licence

MIT. See [`LICENSE`](LICENSE).
