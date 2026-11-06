# spov

spov runs [OpenCode 2](https://opencode.ai) agents in disposable
[Modal](https://modal.com) sandboxes, one sandbox per session, with only the
repositories you chose cloned in. You can work with a session yourself, from
your terminal or a browser, or hand a goal to an autonomous **job** that works
on its own and ends with a pull request or a report.

`spov serve` is the control plane: it holds the credentials, creates the
sandboxes and supervises the jobs. The `spov` CLI and the web dashboard are both
clients of it.

> Pre-release. Everything described here works and is exercised against real
> Modal, Forgejo (Codefloe), GitHub and GitLab, but interfaces still change.

## Why

Running an agent against your repositories on your laptop means it shares your
machine. spov gives each session its own sandbox with only the repositories you
chose, and terminates it when you are done. A sandbox bills for its whole
wall-clock life, so spov is built around ending them: sessions end when you
quit, jobs end when they deliver or reach their time limit, and every sandbox
has a hard timeout.

## Two ways to run it

| | **Local** | **With a server and a database** |
|---|---|---|
| Setup | None: the CLI starts its own server | `spov serve` with Postgres; see [`deploy/README.md`](deploy/README.md) |
| Interactive sessions (`spov fly`) | Yes | Yes |
| Autonomous jobs (`spov jobs create`) | No | Yes |
| Web dashboard | No | Yes, with GitHub sign-in |
| Credentials | On your machine (keyring or a 0600 file) | Per user, encrypted on the server |
| Users | You | Several, with an allowlist and quotas |

The server with a database can run on your own machine too. It is what the
dashboard and jobs need, not a remote host.

## Install

spov needs Go 1.26 or newer. The dashboard is built with Bun:

```sh
scripts/install-bun.sh       # if bun is not installed
scripts/build-web.sh         # builds the dashboard into the binary
scripts/build-proxy.sh       # builds the credential proxy into the binary
go build -o ~/.local/bin/spov ./cmd/spov
```

A plain `go build` without `build-web.sh` works too, but embeds a placeholder
page instead of the dashboard, and `spov serve` says so when it starts. Without
`build-proxy.sh` the binary has no credential proxy: a local server then
launches sandboxes with their tokens and keys inside the main container and
warns about it, and a hosted server refuses to start (see
[Inside the sandbox](#inside-the-sandbox)).

You also need:

- **Modal credentials**: `modal setup`, or `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET`.
- **`opencode`** (OpenCode 2), for the terminal TUI:
  ```sh
  curl -fsSL https://opencode.ai/v2/install | bash
  ```
  spov looks in `~/.local/share/spov/bin/<version>/`, then `~/.opencode/bin/`,
  then `$PATH`, and prefers the version the deployment pins. Another 2.0.x
  release is used with a note, and another 2.x release with a warning. Another
  major release is refused unless you pass `--allow-version-mismatch`.
- **A model provider key**, e.g. `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY`, or
  an OpenCode Go or Zen account (see [Model provider keys](#model-provider-keys)).
- **A git platform credential** (see [Credentials](#credentials)).

Shell completion is optional. `spov completion bash|zsh|fish|powershell`
prints the script, and `spov completion <shell> --help` says where to put it.
Beyond commands and flags, it completes session names for `attach` and
`stop`, job IDs for `spov jobs show|report|cancel|resume|interrupt`, and credential
names for `spov credentials remove`. It asks a server only if one is already
running, and offers nothing rather than starting one or waiting more than a
second.

## Quick start

Configure your platform once, in `~/.config/spov/config.yaml`:

```yaml
platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: your-username
```

Store a token for it and a model key, then launch from any directory:

```sh
spov credentials add codefloe
spov credentials add ANTHROPIC_API_KEY

spov fly --repo your-username/your-repo
```

That creates a sandbox, clones the repository, waits for OpenCode to be
healthy, and hands your terminal to the TUI. When you quit, the sandbox is
terminated.

## Interactive sessions

| Command | What it does |
|---|---|
| `spov fly` | Launch a session and attach the TUI |
| `spov attach <name\|id>` | Join a session, waiting first if it is still starting |
| `spov ls` (`list`) | List sessions, newest first: five a page (`-n`, `--page`), plus any live session older than that |
| `spov stop <name\|id>` | Stop one session and terminate its sandbox; `--all` stops every running one. Stopping a job's session cancels the job. |
| `spov resume <name\|id>` | Reopen a stopped session in a new sandbox and attach the TUI (hosted servers) |

`spov fly` flags:

| Flag | Effect |
|---|---|
| `--repo owner/name` | Repository to clone; repeatable. Use `platform:owner/name` when several platforms are configured. Replaces the configured list. |
| `--model provider/model` | Model to start on, e.g. `anthropic/claude-sonnet-5`. Changeable in the TUI. |
| `--web` | Open OpenCode's own web UI in a browser instead of the terminal TUI |
| `--keep` | Leave the sandbox running when the TUI exits |
| `--no-attach` | Launch and print how to reach it, without starting the TUI |
| `--name` | Name the session (a generated name otherwise) |
| `--plain` | Report progress as plain lines instead of the full-screen launch view |
| `--git-name`, `--git-email` | Identity for commits made in the sandbox; defaults to your local `git config` |
| `--git-access repos\|all` | Whether the sandbox's git credentials reach only this session's repositories (the default) or every repository they can |
| `--sync`, `--no-sync` | Seed the sandbox from your current clean local branch and sync its changes back when the session closes (one repository; `sandbox.sync` makes it the default) |
| `--allow-version-mismatch` | Attach even if the local and sandbox OpenCode versions differ |

`spov attach` takes `--web`, `--no-tui`, `--sync` and `--allow-version-mismatch`.
Attaching never terminates a session: quitting leaves the sandbox up, and
`spov stop` ends it (`spov stop --sync` syncs back first). A sync that fails
leaves the sandbox running, so the work is not lost.

### Resuming a stopped session

On a hosted server, a stopped interactive session can be reopened with
`spov resume`, the session page's Resume button, or by writing in its
composer ("Resume and send"). It is the same session, with its history, in
a new sandbox, and it comes back one of two ways:

- **From its snapshot**, when the server under the credential proxy kept
  one: the workspace is as it was when the session stopped, uncommitted
  changes and installed packages included. Snapshots are kept 7 days and
  at most 20 per user by default (`snapshots` in the server's config).
- **From its transcript** otherwise: the repositories are cloned fresh and
  the conversation is restored, but uncommitted work is gone. The CLI asks
  first; `--yes` skips the question.

The settings are as launched; `--timeout` sets a new lifetime. Leaving the
TUI stops the session again unless `--keep`. `spov attach` on a stopped
session offers to resume it. A job is not resumed.

A session that is still starting is waited on, with a line each time it
moves on. A job's session is joined once the job has opened its
conversation, so the `spov attach` that `spov jobs create` prints works
straight away. Ctrl-C stops only the wait; the session or job carries on.
`--timeout 10m` bounds the wait, and `--no-wait` fails at once instead.

### Choosing repositories

spov takes the first of these that names anything:

1. `--repo` flags.
2. The `repos:` list in project config.
3. An interactive picker of your repositories, when you are on a terminal.

Several repositories clone concurrently, up to five at a time. If any clone
fails, the launch fails rather than starting the agent without it.

### Where a session works

Every session starts in `/workspace`, with each repository cloned into its own
directory under it. spov writes `/workspace/AGENTS.md`, which lists the
repositories, their hosts and which forge CLI belongs to each, and tells the
agent to read each repository's own `AGENTS.md`. `landing:` in config, or
`--landing` on a job, starts OpenCode inside one repository instead.

## Autonomous jobs

A job needs a server with a database. You give it a goal, and spov launches a
sandbox, gives OpenCode the goal, and keeps it working until it declares the work
done. It then judges the result.

```sh
spov jobs create --repo acme/app --check './app/scripts/check.sh' \
  "Fix the flaky retry test in internal/queue and open a pull request"

spov jobs create --kind report --repo acme/app --goal-file audit.md
```

| Flag | Effect |
|---|---|
| `--kind pr\|report` | What the job delivers: pull requests (merge requests on GitLab; the default), recorded one by one in the repositories you selected, or Markdown reports, one file each in `/workspace/out/` |
| `--check <command>` | A command that verifies the work, run at `/workspace`. Its exit status is one of the job's gates. |
| `--max-duration 45m` | How long the job may run unattended. When unset: `sandbox.job_max_duration` in your config, then your default from Settings, then 1 h. Never more than your limit. |
| `--model provider/model` | Model to run on; your account's default when unset |
| `--repo`, `--landing`, `--git-access`, `--goal-file` | As for sessions, plus reading the goal from a file. A job takes its git access from `sandbox.job_git_access` and your Settings, never from the interactive default. |

The job outlives your terminal. `spov jobs` (or `spov jobs list`) lists jobs
newest first, five a page (`-n`, `--page`), plus any unfinished job older than
that; `spov jobs show <id>` shows
one in full with what it delivered, `spov jobs report <id> [<path>]` prints a
report (`--save <dir>` writes it with its images), `spov jobs cancel <id>` ends one, `spov jobs interrupt <id>` stops
its current turn and takes it over, and `spov jobs resume <id>` hands a
steered or blocked one back to its loop. `spov attach <session>` joins the
job's conversation, waiting while it is queued and starting.

**How a job runs.** It moves through `queued`, `provisioning`, `running` and
`delivering` to `succeeded` or `failed` (or `cancelled`). Before provisioning, the server can run
one short model call that assesses the goal and may propose a check; a check you
gave always wins. Two states wait on a person:

- `blocked`: the agent is waiting on a permission question it cannot answer
  itself.
- `steered`: someone sent the job a message or interrupted it. The agent reads
  the message and the job waits until someone resumes it.

**Its time limit.** A job has one time allowance. It counts while the job runs
or is blocked, pauses while someone steers it, and a resume continues with what
is left. No number of turns ends a job. A run that keeps repeating a call its
policy denies is stopped as a safeguard. The job's sandbox lives long enough
for its clock, its launch and delivery, and an hour of steering.

**How it is judged.** When the agent declares the work done, or the time limit
runs out, spov stops the run and checks three gates:

1. **Check**: the check command exits 0.
2. **Deliverable**: each pull request the agent recorded is open or merged on
   its forge, or each report in `/workspace/out/` was collected. Some but not
   all is a partial success; none verified is a failure, whatever the check
   said. If you ask the agent to drop a pull request it recorded, it can
   withdraw it, and a withdrawn one is left out of the count; a withdrawal
   counts only after you have written to the job.
3. **Conduct**: the agent actually did something, with no permission request
   left unanswered.

The job succeeds only if the gates allow it, and a partial success says what
is missing or what the agent left unfinished. The agent saying it is finished
is never evidence. Something spov could not check is `unverified`, not
`failed`.

**Limits.** Each user has a limit on concurrent jobs, separate from the one on
sessions. See [`deploy/README.md`](deploy/README.md) for quotas.

## The web dashboard

A server with a database also serves a dashboard, signed in to with GitHub.
[`deploy/README.md`](deploy/README.md) covers the OAuth app and the allowlist.

- **Sessions**: every session, live, with its state, its job if it has one, its
  age and an estimated cost.
- **Jobs**: every job, filtered by All, Active, Needs you (blocked or steered) and
  Finished, with each job's outcome.
- **Start a session** and **Start a job**: from a form each, with a picker of
  your repositories. The job form makes the same job as `spov jobs create`.
- **A session's page** is also its job's page:
  - the live transcript;
  - the job's state, time limit, verdict gate by gate, and its pull request or
    report;
  - a message box to steer it, a model switch, and answers to the agent's
    permission questions;
  - **Interrupt** to stop the current turn, **Resume** to hand a paused job back,
    **Cancel job** and **End session**. The last two act only on a second press.
  - what credential the sandbox was given for each platform.
- **Usage**: sessions, sandbox hours and estimated spend this month.
- **Tokens**: your CLI tokens, which you can revoke.
- **Settings**: your stored credentials and OpenCode connection, default model,
  git access, job time limit and git identity. An admin also stores the
  organisation's GitHub App here (see [GitHub](#github)).

The browser never talks to a sandbox directly. The page shows what the server
has recorded, and an open tab does not keep a sandbox alive.

## The server

**Local.** By default there is nothing to set up. The CLI starts a server for
itself the first time it needs one (`spov serve --local`, on an ephemeral
loopback port), finds it again through `~/.local/state/spov/server.json`, and the
server exits after a minute with nothing to do. Its log is
`~/.local/state/spov/server.log`, which is where to look if a launch fails early.
After an upgrade the old server is stopped and replaced, so two builds never
share one.

**Hosted.** A server is used instead of the local one when it is named: the
`--server` flag, then `SPOV_SERVER`, then `server.url` in config. Sign in with
`spov login`. spov prints a code to enter in a browser and
stores the token that comes back. `spov whoami` shows who the server sees, and
`spov logout` revokes the token.

Running a server, with its database, browser login, allowlist, quotas,
credentials and health checks, is covered in
[`deploy/README.md`](deploy/README.md).

## Output for scripts

`spov ls`, `spov jobs` (`list`, `show`, `create`), `spov credentials` (`list`)
and `spov whoami` take `--json`. It prints the server API's own response for
that call, as [`docs/api/openapi.yaml`](docs/api/openapi.yaml) describes it,
and nothing else on stdout: `{"sessions": [...]}`, `{"jobs": [...]}`, one job,
`{"credentials": [...], "enabled": ...}`, or your identity. Hints and notes go
to stderr. Tables and messages are for people, and their columns and wording
can change in any release; script against `--json`.

```sh
spov jobs --json | jq -r '.jobs[] | select(.state == "blocked") | .id'
```

## Configuration

Three layers, each overriding the one before:

1. **User**: `~/.config/spov/config.yaml` (or `$XDG_CONFIG_HOME/spov/config.yaml`).
2. **Project**: `.spov.yaml` in the current directory or its nearest ancestor.
   It sets what a repository needs (`repos`, `landing`, `sandbox`), but never
   `platforms` or `server.url`, which decide where your tokens are sent; spov
   refuses a project file that sets either.
3. **Environment**: `SPOV_*` variables, e.g. `SPOV_SANDBOX_TIMEOUT=2h`.

`spov config show` prints the merged result and where each value came from.
Secrets never live in these files.

A typical file is below. **[docs/configuration.md](docs/configuration.md)**
documents every key, with a sample that sets them all.

```yaml
version: 1

platforms:
  - name: codefloe
    kind: forgejo              # github | forgejo | gitea | gitlab (docs/forges.md)
    base_url: https://codefloe.com
    account: alice

repos:
  - platform: codefloe
    owner: alice
    name: widgets
  - platform: codefloe
    owner: alice
    name: gadgets
    dir: tools                 # optional: directory under /workspace

landing: widgets               # optional: start in this repository's directory

sandbox:
  cpu: 0.5
  memory_mib: 2048
  timeout: 4h                  # hard cap 12h
  idle_timeout: 30m            # an interactive session's; a job's equals its timeout
  egress: open                 # open | install | agent
  # addons: [rust, postgres@16, node@22]  # see "Inside the sandbox"; name or name@version
  # apt: [libvips-dev]         # extra Debian packages
  # image: my-custom-image     # an image spov did not build; no addons or apt

opencode:
  version: 2.0.22
  port: 4096

modal:
  app: spov
  # environment: dev
  # build_on_demand: true      # a server builds an add-on variant on first use

git:                           # identity for commits made in the sandbox;
  # name: Your Name            # defaults to your local `git config`
  # email: you@example.com

server:
  # url: https://spov.example.com
```

Only scalar values can be set through the environment. A later layer
**replaces** a list rather than merging it, so a project that declares
`platforms:` should repeat every platform its repositories use.

A server also reads `quotas:`, `metrics:` and `prep:`. `prep:` configures the
model call that prepares a job's goal. See
[docs/configuration.md](docs/configuration.md),
[docs/metrics.md](docs/metrics.md) and [`deploy/README.md`](deploy/README.md).

## Credentials

**Local.** Secrets are stored in your OS keyring where there is one, and
otherwise in `~/.config/spov/credentials.json` at mode 0600, unencrypted, with a
warning the first time. On WSL there is usually no keyring.
`spov credentials add <name>` checks a credential against the platform before
storing it, and prompts without echo so it stays out of your shell history.
`spov credentials` lists what is stored and `spov credentials remove <name>`
removes one. A name is a platform from config (or `github`, or
`forgejo:<host>`, `gitea:<host>` or `gitlab:<host>`), or a provider's variable such as `ANTHROPIC_API_KEY`.

**Hosted.** Each user's credentials are stored on the server, encrypted, and
used for that user's sessions and jobs. Store them from the dashboard's Settings
page, or from the CLI with the same commands: when the CLI is pointed at a hosted
server, `spov credentials add github` (a token), `spov credentials add
forgejo:<host>` (or `gitea:<host>`, `gitlab:<host>`) or `spov credentials add <VAR>` store on the server.
`spov credentials` lists them. No command returns a stored credential.

A deployment can also hold a shared credential for a platform, the
organisation's. A launch uses your own credential first and the organisation's
only when you have none (a launch from the CLI can prefer the shared one with
`prefer: shared` on the platform). See [docs/github-access.md](docs/github-access.md).

### Model provider keys

**OpenCode Go and Zen.** `spov login opencode` (or **Connect OpenCode** on the
Settings page) signs in to your OpenCode Console account in the browser. The
server keeps the sign-in, refreshes it when a session needs it, and gives it to
each session through the credential proxy, so the sandbox never holds it. Models
are then `opencode-go/<model>` for the Go subscription and `opencode/<model>` for
Zen. This sign-in is spov's own: it does not reuse, and cannot sign out, the one
your local OpenCode holds. A console API key stored with
`spov credentials add OPENCODE_API_KEY` works too. `spov credentials` says
whether you are connected, and `spov logout opencode` forgets the sign-in.

**API keys.** spov hands `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` and the other
provider keys it recognises to the session's credential proxy, which adds each to
requests for its provider. OpenCode in the sandbox holds a placeholder and
chooses among the providers. With a local server an exported key just works. To
avoid exporting one in every shell, store it with
`spov credentials add ANTHROPIC_API_KEY`; an exported key wins over a stored
one. A local server reads its keys when it starts, so restart it after adding
one. A hosted server reads your stored keys at each launch.

### Forgejo and Gitea

Create a token under **Settings → Applications → Generate New Token**, with
**Specific repositories** listing the repositories sandboxes should reach, and
`repository` set to *Read and write*.

The session gets that token as it is. spov cannot mint narrower ones, because
Forgejo only creates tokens against your account password. spov also cannot see
a token's scope, so the narrowing is yours to set. The session page says when a
sandbox holds a stored token.

Gitea and GitLab tokens are stored the same way. What to create for each, and
what the proxy lets a stored token do, is in [docs/forges.md](docs/forges.md).

### GitHub

GitHub works best through a **GitHub App**. spov holds the App's private key and
mints each session an installation token narrowed to just that session's
repositories. The key itself never enters a sandbox.

On a hosted server there is one App, the organisation's, and everyone on the
deployment uses it. An admin stores it under **Organisation** in Settings, where
spov checks it before keeping it, or the deployment's environment sets it
(`SPOV_GITHUB_APP_ID` and `SPOV_GITHUB_APP_PRIVATE_KEY`), which wins. Users do
not store an App; they can store a token of their own (below). A local server
takes only an App, which you store yourself. [docs/github-access.md](docs/github-access.md)
covers the rest.

Create the App under **Settings → Developer settings → GitHub Apps → New GitHub
App**:

- **Repository permissions:** `Contents` read and write, `Metadata` read-only,
  and `Pull requests` and `Issues` read and write if the agent should open
  pull requests or work with issues.
- Uncheck **Webhook → Active**.
- Generate a private key, then install the App on the repositories sandboxes
  should reach.

Then store it. On a hosted server an admin enters the App ID and key in
Settings. On a local server, give spov the path to the key rather than its
contents:

```sh
spov credentials add github --app-id YOUR_CLIENT_ID \
  --key-file ~/Downloads/your-app.private-key.pem \
  --installation-id 12345678     # optional; discovered if omitted
```

and, locally, point a platform at it (`base_url` defaults to `https://github.com`; set it
for GitHub Enterprise):

```yaml
platforms:
  - name: github
    kind: github
    account: your-username     # or the org the App is installed on
```

An installation token lasts an hour. While a session is in use, the server mints
a new one before the old one expires and hands it to the sandbox, so git and
`gh` keep working for as long as the session does. A session nobody is using is
not renewed, because writing into a sandbox would keep it alive. When its person
comes back, the token is renewed within a minute. If a renewal fails, the
session page says so.

A stored GitHub personal access token (`spov credentials add github` on a
hosted server) is used as it is, like a Forgejo token. A fine-grained token can
read every public repository, so the repository pickers mark those as possibly
read-only: the token can push only to repositories it was given. Whenever a
launch uses a stored token, the forge is asked first whether the token can see
each repository, and the launch is refused before any sandbox exists if it
cannot.

## Inside the sandbox

Each sandbox has `git`, `ripgrep`, the pinned `opencode`, and the forge CLIs:
`tea` for Forgejo and Gitea, `gh` for GitHub and `glab` for GitLab. They are set
up for the credential spov cloned with, but hold a placeholder rather than the
token (see below).

It also has a general-purpose toolchain on Debian 13: build tools, Python with
`uv`, Node LTS with corepack (pnpm, yarn), Bun and Go, at pinned versions, plus
`jq`, `sqlite3`, `fd`, `zip` and `openssh-client`. A repository's own version
files win inside it: `.nvmrc`, `.python-version`, `.tool-versions` and
`mise.toml` through [mise](https://mise.jdx.dev), and `go.mod` through Go. A
version that is not installed is downloaded on first use, which needs egress.
The generated `/workspace/AGENTS.md` tells the agent what is installed.

More can be added per project with `sandbox.addons`, a list of `name` or
`name@version`: `dotnet`, `java`, `php`, `playwright`, `postgres`, `redis`,
`ruby` and `rust`. A bare name gets spov's tested version; a partial version
such as `java@21` follows new releases: the server looks up the newest match and
boots the image built for it, and while a new patch builds, sessions keep using
the last build. Postgres, PHP
and Redis come from their upstream repositories and take any version they
publish: `postgres@14`, `php@8.1`, `redis@7.2` or `redis@7.2.16`. Postgres and
Redis are started when the sandbox boots. The same list pins a default
language into the image, such as `node@22`, `python@3.12` or `go@1.24`, so it is
not downloaded in every session; a repository's own version file still wins.
`sandbox.apt` adds Debian packages. Either makes the session's image a variant
of the base, which the server builds on its first launch (seconds to a few
minutes, once) unless `modal.build_on_demand` is false, in which case
`spov serve image build --addons ... --apt ...` builds it. The server deletes
images nobody has used for a while (see [Configuration](docs/configuration.md#add-ons)).
Docker is not available inside a sandbox.

Forge tokens and model provider keys do not enter the sandbox's main container,
where the agent, its shell and the CLIs run. A credential proxy runs beside it
in a sidecar container and holds them, and adds each only to the requests it
allows: git on the session's repositories, and a fixed set of REST calls on
them, with the merge endpoints, repository settings and similar refused for a
stored token. So the clone URL and `git remote -v` show no token, and the agent
cannot read one. [docs/forges.md](docs/forges.md) lists what the proxy allows.
The proxy is built in by `scripts/build-proxy.sh`. A local `spov` built without
it puts the tokens in the sandbox, where the agent can read them, and warns; a
hosted server refuses to start without it unless `SPOV_TOKENS_IN_SANDBOX=1`.

**The agent can still use the credentials it is given.** It needs the git token
to push, and it has a shell. What limits a prompt-injected agent is the
credential's scope and the proxy's rules, not the sandbox. That is why a GitHub
App token is narrowed per session, why a Forgejo token should be created for
specific repositories, and why you should protect your default branch on the
forge: a push reaches every branch the forge itself does not protect. A job's
agent also runs under a permission allowlist and is steered away from the
credential files, but that is a guardrail, not a boundary. Sandboxes can reach
the open internet unless `sandbox.egress` narrows it.

## Cost

A Modal sandbox bills for its whole wall-clock life, not for the work it does.
spov therefore:

- terminates a session's sandbox when the TUI exits, unless you pass `--keep`;
- ends a job's sandbox when the job finishes, and bounds its unattended time;
- caps `timeout` at 12 hours and defaults it to 4;
- sets an idle timeout (30 minutes by default) as the safety net for an
  interactive session nobody is attached to. An attached TUI or `--web` browser
  keeps a sandbox alive, because the CLI's proxy pings it. The dashboard does
  not. A job is not idled out: its idle timeout equals its sandbox timeout, so
  it ends when it delivers, reaches its time limit or is cancelled;
- shows each sandbox's age in `spov ls`, and an estimated cost at launch and in
  the dashboard.

If a launch fails partway, spov terminates whatever it created. If that fails
too, it says so and tells you how to stop the sandbox by hand.

## Development

```sh
./scripts/check.sh                     # the commit gate: Go, web and coverage checks
git config core.hooksPath .githooks    # run it before every push

cd web && bun run dev                  # the dashboard against a separate spov serve

# Integration tests create real, billable Modal sandboxes:
go test -tags integration ./internal/sandbox/ -run Integration -v
```

Decisions live in `decisions/` and are linked from the code with
`@decision(DL-NNN)` annotations. If you are about to change annotated code, read
the decision first. [`AGENTS.md`](AGENTS.md) is the map of the codebase, the
rules for each area, and the tests that guard them.

## Licence

MIT. See [`LICENSE`](LICENSE).
