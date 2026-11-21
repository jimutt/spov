# spov documentation

spov launches [OpenCode](https://opencode.ai) agents in on-demand
[Modal](https://modal.com) sandboxes, for interactive sessions and for jobs
that run on their own. New here? Start with the
[project README](../README.md): it covers installing spov, `spov init` and a
first session.

## Find what you need

| I want to… | Read |
|---|---|
| Start a session from my terminal, sync it with my checkout, or resume one | [Interactive sessions](sessions.md) |
| Hand off a goal and get a pull request or a report back | [Autonomous jobs](jobs.md) |
| Give a sandbox access to GitHub, Forgejo, Gitea or GitLab, or to a model provider | [Credentials](credentials.md) |
| Choose what a sandbox has installed, its network access or its variables | [The sandbox](sandbox.md) |
| Use OpenCode plugins in a sandbox | [OpenCode plugins](plugins.md) |
| Use the dashboard, or run spov for a team | [The server and the dashboard](server.md) |
| Look up a configuration key | [Configuration reference](configuration.md) |

## Using spov

- [Interactive sessions](sessions.md): `spov fly`, attaching, syncing work
  back to a checkout, resuming a stopped session, and first-time setup with
  `spov init`.
- [Autonomous jobs](jobs.md): goals, checks, the three kinds of job (pull
  request, report, sync), steering a running job and how a job is judged.
- [Credentials](credentials.md): forge tokens, model keys, the OpenCode
  sign-in and routed credentials for registries and other services.
- [The sandbox](sandbox.md): the toolchain, add-ons, environment variables,
  egress presets, sandbox templates and the credential proxy.
- [OpenCode plugins](plugins.md): adding OpenCode 2 plugins with
  `sandbox.plugins` or a repository's own `opencode.json`, and what a plugin
  needs from the sandbox.
- [Configuration reference](configuration.md): every key, in the user file,
  a project's `.spov.yaml` and `SPOV_*` variables.

## Running a server

- [The server and the dashboard](server.md): the local server the CLI starts
  for you, a hosted server for a team, signing in, the allowlist and service
  accounts.
- [Deploying a hosted server](../deploy/README.md): the container image,
  Postgres, the GitHub App and service accounts for pipelines.
- [Running a shared server safely](security.md): what a shared server
  protects, what it does not, and a checklist for before you expose one.
- [GitHub access on a hosted spov](github-access.md): whose credential a
  sandbox uses, and what the organisation's GitHub App can reach.
- [Metrics](metrics.md): the control-plane metrics `spov serve` publishes and
  the dashboard built on them.

## Reference

- [Forges](forges.md): what works on GitHub, Forgejo, Gitea and GitLab.
- [Local session history](local-history.md): where a local server keeps its
  sessions, how to read them, and how long they are kept.
- [The HTTP API](api/openapi.yaml): the OpenAPI document of the API the CLI
  and the dashboard both use.

## Contributing

- [Developing spov](development.md): building from source, the checks and the
  tests.
- [Releasing spov](releasing.md): how a release is cut.
- [`AGENTS.md`](../AGENTS.md) maps the code, and
  [`decisions/`](../decisions/INDEX.md) holds the reasons behind it.
