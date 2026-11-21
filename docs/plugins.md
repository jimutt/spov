# OpenCode plugins

spov runs OpenCode 2, and an OpenCode 2 plugin works in a spov sandbox the way
it works on your own machine: OpenCode installs it from npm and loads it when
the session's first conversation starts. This page says where a plugin can
come from, what it needs from the sandbox, and what does not work yet.

> **OpenCode 2 only.** OpenCode 2 has a new plugin API, and plugins written
> for OpenCode 1 do not run on it until they are ported. A plugin's README or
> its npm dependencies say which it supports: an OpenCode 2 plugin depends on
> `@opencode/plugin`, an OpenCode 1 plugin only on `@opencode-ai/plugin`.
> OpenCode 2's config key is `plugins` (plural); 1.x guides say `plugin`.

## Two ways to add a plugin

### Your own list: `sandbox.plugins`

List npm packages in your config, and every interactive session gets them,
wherever it starts and however many repositories it has:

```yaml
# ~/.config/spov/config.yaml, or a project's .spov.yaml
sandbox:
  plugins:
    - envsitter-guard@0.1.1
    - opencode-rules@2.0.0
```

- **`--plugin <spec>`** on `spov fly` replaces the list for one launch; repeat
  it for several.
- **A sandbox template** can carry a list too (the **OpenCode plugins** field
  on the Templates page, or `plugins` in the template's JSON). A launch from a
  template takes its list unless the launch names its own, as with the
  template's other settings. In the browser, a template is how a session gets
  your plugins.
- **Each entry is an npm package spec:** `name` or `@scope/name`, optionally
  `@version`, a tag such as `@latest`, or a semver range such as `@^2.0.0`. A
  URL, a git repository or a path is refused, so a plugin only ever comes from
  the npm registry. At most 16.
- spov writes the list into the sandbox's own OpenCode config
  (`~/.config/opencode/opencode.json`), where `opencode plugin add` would put
  it on your machine. The session's page lists them under **Workspace**.

Pin a version (`name@1.2.3`) when you want every sandbox to run the same code:
a bare name or a tag installs whatever is newest when the sandbox starts.

### The repository's own `opencode.json`

A repository that configures OpenCode for its contributors keeps working in
spov: its `opencode.json` (with `plugins`) and its `.opencode/` directory
(local plugin files, rules, commands and agents) are read by OpenCode when the
session runs inside that repository. A session runs inside its repository
when it has exactly one: a synced `spov fly` in a checkout, `spov fly --repo
owner/name`, or a browser launch of one repository.

A session with several repositories starts in `/workspace`, the folder that
holds them, which is no repository: OpenCode reads none of their
`opencode.json` files there, as it would not if you started it in the parent
folder of two checkouts on your machine. `landing: <directory>` in your config
starts such a session in one of them, and then only that repository's
configuration loads. Plugins in `sandbox.plugins` load either way.

When both are present, OpenCode merges them: the session gets your plugins and
the repository's.

## What a plugin needs from the sandbox

- **The npm registry, to install.** Under the `open` egress preset (the
  default) and `install`, it is reachable. Under `agent`, spov adds
  `registry.npmjs.org` when the launch has `sandbox.plugins`, and the launch
  says so. A repository's own plugins are read only after the clone, so spov
  cannot see them at launch: under `agent`, add the registry yourself with
  `sandbox.egress_extra_domains: [registry.npmjs.org]`. Without it the
  session runs without those plugins, and nothing else says so.
- **Its own hosts.** A plugin that calls a service of its own (a telemetry
  endpoint, a model download, an API) needs that host reachable: add it to
  `sandbox.egress_extra_domains` under `install` or `agent`.
- **Its own credentials.** Never put a plugin's API key in `sandbox.env`,
  which is shown with the session. Store it as a
  [routed credential](credentials.md#private-registries-and-other-services)
  for the service's host, with the header the plugin sends it in: the
  credential proxy sets it on the plugin's requests to that host and replaces
  whatever value the plugin sent, so the plugin's own setting can hold any
  placeholder.
- **Time and space, once per sandbox.** A plugin installs when the first
  conversation starts, which delays the first answer by the install's time
  (seconds for most, more for a plugin with large native dependencies). A
  resumed session keeps what was installed, so a resume does not install
  again.

## In the transcript

A plugin that adds text to the conversation (rules, reminders, recalled
memories) does it with a message OpenCode marks as synthetic. The session's
page draws such a message folded, as **Added by OpenCode or a plugin**, and
never as yours, the agent's or spov's. A plugin's tools are drawn like any
other tool call.

## What does not work yet

- **Jobs.** `spov jobs create` adds no plugins: a job's permission policy
  allows only the actions its kind needs, and a plugin's tools are not on
  that list. A sync job runs inside its repository, so that repository's own
  `opencode.json` still loads there.
- **The TUI's side of a plugin.** A plugin can also extend OpenCode's terminal
  UI. `spov attach` uses the `opencode` on your machine, so that part comes
  from the plugins installed there, not from the sandbox.
- **A server-wide list.** There is no list a hosted server applies to every
  session; use a template.

## Tried in spov

These were run in real spov sandboxes on OpenCode 2.3.0:

| Plugin | What it does | Result |
|---|---|---|
| `envsitter-guard` | Blocks reading `.env` files, adds safe `envsitter_*` tools | Its 15 tools reached the agent; `read` and a shell `cat` of `.env` were refused |
| `opencode-rules` | Injects Markdown rules from `.opencode/rules/` into the prompt | The repository's rule was injected and followed |
| `opencode-mem` | Persistent memory with a local vector store | Installed (1.3 GB with its native dependencies), downloaded its embedding model through the proxy, stored memories, kept them across a resume |
