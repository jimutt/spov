# The sandbox

Each session and job runs in a [Modal](https://modal.com) sandbox of its own:
a container with your repositories cloned under `/workspace`, OpenCode running
in it, and a credential proxy beside it. This guide covers what is installed,
how to add to it, how credentials are kept out of it, and what it costs.

## What is installed

Each sandbox has `git`, `ripgrep`, the pinned `opencode`, and the forge CLIs:
`tea` for Forgejo and Gitea, `gh` for GitHub and `glab` for GitLab. They are
set up for the credential spov cloned with, but hold a placeholder rather than
the token (below).

It also has a general-purpose toolchain on Debian 13: build tools, Python with
`uv`, Node LTS with corepack (pnpm, yarn), Bun and Go, at pinned versions,
plus `jq`, `sqlite3`, `fd`, `zip` and `openssh-client`. A repository's own
version files win inside it: `.nvmrc`, `.python-version`, `.tool-versions` and
`mise.toml` through [mise](https://mise.jdx.dev), and `go.mod` through Go. A
version that is not installed is downloaded on first use, which needs egress.
The generated `/workspace/AGENTS.md` tells the agent what is installed.

Docker is not available inside a sandbox.

## Add-ons

More can be added per project with `sandbox.addons`, a list of `name` or
`name@version`:

```yaml
sandbox:
  addons: [rust, postgres@16, node@22]
  apt: [libvips-dev]          # extra Debian packages
```

- The add-ons are `dotnet`, `java`, `php`, `playwright`, `postgres`, `redis`,
  `ruby` and `rust`. A bare name gets spov's tested version; a partial
  version such as `java@21` follows new releases: the server looks up the
  newest match and boots the image built for it, and while a new patch
  builds, sessions keep using the last build.
- Postgres, PHP and Redis come from their upstream repositories and take any
  version they publish: `postgres@14`, `php@8.1`, `redis@7.2` or
  `redis@7.2.16`. Postgres and Redis are started when the sandbox boots.
- The same list pins a default language into the image, such as `node@22`,
  `python@3.12`, `go@1.24`, `bun@1.3` or `uv@0.9`, so it is not downloaded in
  every session; a repository's own version file still wins.

Add-ons or `apt` packages make the session's image a variant of the base,
which the server builds on its first launch (seconds to a few minutes, once)
unless `modal.build_on_demand` is false, in which case
`spov serve image build --addons ... --apt ...` builds it. The server deletes
images nobody has used for a while. Versions and defaults are in
[configuration.md](configuration.md#add-ons).

A shared (hosted) server limits how much one person can build; a local server
does not, since it has one user on their own Modal account. Each person has one image build
running at a time: a second launch of theirs waits for the first build to end.
A request may start at most five new images in any 24 hours, failed builds
included, and a launch past that is refused with a 429 that says when the next
one is allowed. Booting an image that already exists, whoever built it, never
counts and never waits, and neither does an operator's `spov serve image build`.

## Environment variables

Ordinary settings go in `sandbox.env`, in your user file or a project's
`.spov.yaml`, or on one launch with `--env KEY=VALUE`:

```yaml
sandbox:
  env:
    NODE_ENV: development
    OPENCODE_LOG_LEVEL: info
```

The two files merge by name, and `--env` on `spov fly` or `spov jobs create`
sets one more over them. Quote a value YAML would read as a number or `true`.
They reach every process in the sandbox, sessions and jobs alike, and are not
secret: spov stores them with the session and shows them on its page. A token
or a key goes in a [routed credential](credentials.md#private-registries-and-other-services)
instead. Names spov relies on, such as `PATH`, `GITHUB_TOKEN` or
`OPENCODE_CONFIG`, are refused;
[configuration.md](configuration.md#environment-variables-in-the-sandbox)
lists them.

## Credentials stay outside the sandbox

Forge tokens and model provider keys do not enter the sandbox's main
container, where the agent, its shell and the CLIs run. A credential proxy
runs beside it in a sidecar container and holds them, and adds each only to
the requests it allows: git on the session's repositories, and a fixed set of
REST calls on them, with the merge endpoints, repository settings and similar
refused for a stored token. So the clone URL and `git remote -v` show no
token, and the agent cannot read one. [forges.md](forges.md) lists what the
proxy allows.

**The agent can still use the credentials it is given.** It needs the git
token to push, and it has a shell. What limits a prompt-injected agent is the
credential's scope and the proxy's rules, not the sandbox. That is why a
GitHub App token is narrowed per session, why a Forgejo token should be
created for specific repositories, and why you should protect your default
branch on the forge: a push reaches every branch the forge itself does not
protect. A job's agent also runs under a permission allowlist and is steered
away from the credential files, but that is a guardrail, not a boundary.

The proxy is part of every release. A `spov` built from source without it
puts the tokens in the sandbox, where the agent can read them, and warns; a
hosted server refuses to start without it unless `SPOV_TOKENS_IN_SANDBOX=1`.
See [development.md](development.md#building-from-source).

## Network access

Sandboxes can reach the open internet unless `sandbox.egress` narrows it:

| `egress` | What the sandbox can reach |
|---|---|
| `open` (default, for jobs too) | Anywhere |
| `install` | Everything `agent` allows, plus the package registries and toolchain downloads a first-use install needs |
| `agent` | GitHub and the session's other forges; Anthropic, OpenAI, Gemini, OpenRouter and OpenCode's APIs; OpenCode's web search (`mcp.exa.ai`) |

`install` and `agent` allow TLS only, so a plain-HTTP registry or `git://`
fails. Another model provider (Groq, xAI, DeepSeek, Mistral) needs its API host
in `sandbox.egress_extra_domains`. `sandbox.egress_extra_domains` adds hosts to either (not to `open`), and
a routed credential's hosts are added for it.

## Cost

A Modal sandbox bills for its whole wall-clock life, not for the work it does.
spov therefore:

- terminates a session's sandbox when the TUI exits, unless you pass
  `--keep`;
- ends a job's sandbox when the job finishes, and bounds its unattended time;
- caps `timeout` at 12 hours and defaults it to 4;
- sets an idle timeout (30 minutes by default) as the safety net for an
  interactive session nobody is attached to. An attached TUI or `--web`
  browser keeps a sandbox alive, because the CLI's proxy pings it. The
  dashboard does not. A job is not idled out: its idle timeout equals its
  sandbox timeout, so it ends when it delivers, reaches its time limit or is
  cancelled;
- shows each sandbox's age in `spov ls`, and an estimated cost at launch and
  in the dashboard.

If a launch fails partway, spov terminates whatever it created. If that fails
too, it says so and tells you how to stop the sandbox by hand.
