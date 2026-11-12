# Interactive sessions

A session is an OpenCode agent in a sandbox of its own, with the
repositories you chose cloned in, that you work with yourself: in the
terminal TUI, in OpenCode's web UI, or from the dashboard. This guide covers
everything you can do with one. For handing a goal off to run on its own,
see [Autonomous jobs](jobs.md).

## First-time setup

```sh
spov init
```

`spov init` checks, in order, what a first launch needs on this machine, and
offers to set up each thing that is missing:

| Step | Ready when | What init offers |
|---|---|---|
| Modal | Its credentials are found and Modal accepts them | Run `modal setup` in your terminal |
| Git platform | A configured platform has a working credential | GitHub through `gh`: run `gh auth login` if needed, add the `platforms` entry to your user config, and use gh's login |
| Model | A provider key is stored or exported, or OpenCode is signed in | Paste an API key, or sign in to OpenCode Go or Zen |
| OpenCode client | A client the pinned version accepts is installed | Nothing: it prints the install command. `spov fly --web` works without one |
| Sandbox image | The image for the pinned OpenCode version is published | Build it, which takes a few minutes |

It asks before every change, and a no skips that step. If a spov server from
an earlier command is still running idle on this machine, init stops it after
a change, so the next command starts one that sees what you set up. It is safe to run
again: a step that is done is left alone. It never stores a Modal token or
copies gh's token, and it adds to your config file only by creating it or
appending a `platforms` block; a file that already has a `platforms` list is
yours, and init prints the lines to add instead.

`spov init --check` changes nothing and exits 1 when a required step is
missing, as a run without a terminal does. `--json` prints the same as one
document: `{ready, steps: [{name, state, required, detail, fix}]}`, with the
step names `modal`, `git`, `model`, `client` and `image`.

Init sets up GitHub on github.com only. For Forgejo, Gitea, GitLab, GitHub
Enterprise or a GitHub App use `spov credentials add`
([Credentials](credentials.md)); init then reports the platform as ready.
With a hosted server configured, init only checks that you are signed in to it
and that your local client matches it.

## Starting one

```sh
cd your-repo
spov fly
```

Inside a git checkout, a plain `spov fly` launches that repository from your
current branch, waits for OpenCode to be ready, and hands your terminal to the
TUI. When you quit, the agent's commits and uncommitted changes come back to
your checkout, and then the sandbox is terminated. See
[Working from a checkout](#working-from-a-checkout).

Elsewhere, name the repositories:

```sh
spov fly --repo your-org/app --repo your-org/lib
```

A `--repo` launch clones each repository's default branch and does not sync
back: what the agent does stays in the sandbox unless it pushes.

| Command | What it does |
|---|---|
| `spov fly` | Launch a session and attach the TUI |
| `spov attach <name\|id>` | Join a session, waiting first if it is still starting |
| `spov ls` (`list`) | List sessions, newest first: five a page (`-n`, `--page`), plus any live session older than that |
| `spov stop <name\|id>` | Stop one session and terminate its sandbox; `--all` stops every running one. Stopping a job's session cancels the job. |
| `spov resume <name\|id>` | Reopen a stopped session in a new sandbox and attach the TUI |

### `spov fly` flags

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
| `--env KEY=VALUE` | An environment variable in the sandbox, over `sandbox.env`; repeatable. Not for secrets (see [The sandbox](sandbox.md#environment-variables)) |
| `--credentials a,b`, `--no-credentials` | Which [routed credentials](credentials.md#private-registries-and-other-services) the session gets |
| `--template name`, `--no-template` | Launch from a [sandbox template](sandbox.md#sandbox-templates), or without the one `sandbox.template` names |
| `--sync`, `--no-sync` | Whether to seed the sandbox from your current branch and sync its changes back (below) |
| `--allow-version-mismatch` | Attach even if the local and sandbox OpenCode versions differ |

## Choosing repositories

spov takes the first of these that names anything:

1. `--repo` flags.
2. The `repos:` list in a project file (`.spov.yaml`).
3. The git checkout you run `spov fly` in, synced (below).
4. The `repos:` list in your user file.
5. An interactive picker of your repositories, when you are on a terminal.

Several repositories clone concurrently, up to five at a time. If any clone
fails, the launch fails rather than starting the agent without it.

### Working from a checkout

Inside a git checkout, a plain `spov fly` launches that repository from your
current branch, including commits you have not pushed, and copies the
sandbox's commits and uncommitted changes back into the checkout when you
quit.

- **Nothing is overwritten.** The work lands back on your branch when the
  checkout is as you left it, and on a `spov/<session>` branch when you have
  changed it meanwhile.
- **Only what `git status` shows.** The sync never writes or deletes a
  file your checkout ignores (`node_modules`, `.env`), and refuses a sync
  that tries. If the session changed your repository's git hooks (a
  `core.hooksPath` inside the checkout, such as `.githooks` or husky's
  directory), the work lands on `spov/<session>` for you to read first.
- **You see what landed.** After a sync, spov prints the branch the work
  landed on, the commits it added (how many, and the short range), and each
  changed (`M`), added (`A`) and deleted (`D`) path, up to 30 and then "and N
  more". A path or message the sandbox wrote is printed with any terminal
  control character shown as an escape such as `\x1b`.
- **Your checkout's hooks do not run.** Every git command spov runs in your
  checkout turns off its hooks (`core.hooksPath`) and its filesystem monitor
  (`core.fsmonitor`), and uses no pager.
- **Nothing is lost.** The sandbox is terminated only after the work is
  back; a sync that fails leaves it running and tells you how to retry.
- **The checkout must be clean and on a branch**, or the launch stops and
  says so: commit or stash first, or pass `--no-sync`. "Clean" counts
  untracked files too, a new `.spov.yaml` among them. A directory with no
  checkout of its own counts as inside any checkout above it, such as a home
  directory kept in git.
- **A checkout whose remote is on no platform you configured** is sent along
  from your machine, and its sandbox cannot push.

`sandbox.sync: false` turns this off, and `true` makes every `spov fly` sync
(as `--sync` does), failing outside a checkout. `--sync` also syncs a `--repo`
that is the checkout's. Jobs never sync.

`spov attach <name> --sync` copies a running session's work into the checkout
when you leave the TUI, without stopping it, and `spov stop <name> --sync`
copies it and then stops the session.

## Joining, leaving and stopping

`spov attach` takes `--web`, `--no-tui`, `--sync` and
`--allow-version-mismatch`. Attaching never terminates a session: quitting
leaves the sandbox up, and `spov stop` ends it.

A session that is still starting is waited on, with a line each time it moves
on. A job's session is joined once the job has opened its conversation, so
the `spov attach` that `spov jobs create` prints works straight away. Ctrl-C
stops only the wait; the session or job carries on. `--timeout 10m` bounds the
wait, and `--no-wait` fails at once instead.

An attached TUI or `--web` browser keeps a sandbox alive. Nobody attached, it
stops itself after its idle timeout (30 minutes by default). See
[Cost](sandbox.md#cost).

## Resuming a stopped session

A stopped interactive session can be reopened with `spov resume`, the session
page's Resume button, or by writing in its composer ("Resume and send"). It is
the same session, with its history, in a new sandbox, and it comes back one of
two ways:

- **From its snapshot**, when the server kept one (it does when the credential
  proxy holds the secrets): the workspace is as it was when the session
  stopped, uncommitted changes and installed packages included. Snapshots are
  kept 7 days and at most 20 per user by default (`snapshots` in the server's
  config).
- **From its transcript** otherwise: the repositories are cloned fresh and the
  conversation is restored, but uncommitted work is gone. The CLI asks first;
  `--yes` skips the question. A session synced from a checkout starts again
  from the commit it started from: run `spov resume` inside that checkout and
  it sends the commit along, unpushed or not. From the browser or elsewhere, a
  commit that was never pushed is not there, and the workspace starts on the
  default branch, which the launch notes.

The settings are as launched; `--timeout` sets a new lifetime. Leaving the TUI
stops the session again unless `--keep`. `spov attach` on a stopped session
offers to resume it. A job is not resumed.

A session that was synced from a checkout syncs back when you leave the TUI, as
`spov fly` does, if you resumed it inside that checkout: the sandbox's commits
and uncommitted changes are applied to the checkout (on `spov/<session>` when
the checkout has moved on), and then the session stops. A resume has no
clean-checkout rule, because the fallback branch makes a dirty or moved checkout
safe. Resumed from anywhere else, a synced session is left running when you
leave the TUI, since there is nowhere to put its work, and spov prints
`spov stop <name> --sync` (run it in the checkout), `spov attach <name> --sync`
and `spov stop <name>` to discard it. It bills until you do one of those, or
until it idles out. If a sync fails the session is left running too, with the
same commands.

- `--no-sync` stops the session on exit without syncing; the work stays in the
  next exit snapshot.
- `--sync` asks for the default explicitly and is refused, before anything
  starts, for a session that was not synced or outside its checkout. Use it in a
  script that must get the work back.
- `--keep`, `--no-attach` and `--json` never sync; the commands they print name
  `--sync`.

A local server keeps its session history under `~/.local/share/spov/sessions`
for 30 days after a session stops (`history.retention`);
[local-history.md](local-history.md) describes the files.

## Where a session works

Every session starts in `/workspace`, with each repository cloned into its own
directory under it. spov writes `/workspace/AGENTS.md`, which lists the
repositories, their hosts and which forge CLI belongs to each, and tells the
agent to read each repository's own `AGENTS.md`. `landing:` in config starts
OpenCode inside one repository instead, and a synced launch always starts in
its repository. What the sandbox has installed is in [The sandbox](sandbox.md).

## The OpenCode client

The terminal TUI is your local `opencode` (OpenCode 2), talking to the server
in the sandbox, so the two must match. `install.sh --with-opencode` installs the
version spov pins. spov looks for the client in
`~/.local/share/spov/bin/<version>/`, then `~/.opencode/bin/`, then `$PATH`,
and prefers the version the deployment pins. Another patch release is used
with a note, another minor release with a warning, and another major release
is refused unless you pass `--allow-version-mismatch`. `--web` needs no local
client at all.

## Shell completion

`spov completion bash|zsh|fish|powershell` prints the script, and
`spov completion <shell> --help` says where to put it. Beyond commands and
flags, it completes session names for `attach` and `stop`, job IDs for
`spov jobs show|report|cancel|resume|interrupt|wait`, and credential names for
`spov credentials remove`. It asks a server only if one is already running,
and offers nothing rather than starting one or waiting more than a second.

## Output for scripts

`spov ls`, `spov jobs` (`list`, `show`, `create`), `spov credentials` (`list`)
and `spov whoami` take `--json`. It prints the server API's own response for
that call, as [`api/openapi.yaml`](api/openapi.yaml) describes it, and nothing
else on stdout: `{"sessions": [...]}`, `{"jobs": [...]}`, one job,
`{"credentials": [...], "enabled": ...}`, or your identity. Hints and notes go
to stderr. Tables and messages are for people, and their columns and wording
can change in any release; script against `--json`.

```sh
spov jobs --json | jq -r '.jobs[] | select(.state == "blocked") | .id'
```
