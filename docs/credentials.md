# Credentials

A session needs two credentials, plus any routed credentials you add:

- **A git platform credential**, so the sandbox can clone and push: GitHub,
  Forgejo, Gitea or GitLab.
- **A model provider**: an API key such as `ANTHROPIC_API_KEY`, or an OpenCode
  Go or Zen sign-in.
- **Routed credentials**, optionally: tokens for private registries and other
  services the agent needs.

None of them enters the sandbox where the agent runs. The credential proxy, in
a sidecar container beside it, holds them and adds each only to the requests
it allows; the agent sees placeholders. See
[The sandbox](sandbox.md#credentials-stay-outside-the-sandbox).

## Where they are stored

**Local.** Secrets are stored in your OS keyring where there is one, and
otherwise in `~/.config/spov/credentials.json` at mode 0600, unencrypted, with
a warning the first time. On WSL there is usually no keyring.

`spov credentials add <name>` checks a platform credential against the
platform before storing it (a provider key is stored as given), and prompts
without echo so it stays out of your shell history.
`spov credentials` lists what is stored and `spov credentials remove <name>`
removes one. A name is a platform from config (or `github`, or
`forgejo:<host>`, `gitea:<host>` or `gitlab:<host>`), or a provider's variable
such as `ANTHROPIC_API_KEY`.

**Hosted.** Each user's credentials are stored on the server, encrypted, and
used for that user's sessions and jobs. Store them from the dashboard's
Settings page, or from the CLI with the same commands: when the CLI is pointed
at a hosted server, `spov credentials add github` (a token),
`spov credentials add forgejo:<host>` (or `gitea:<host>`, `gitlab:<host>`) or
`spov credentials add <VAR>` store on the server. `spov credentials` lists
them. No command returns a stored credential.

A deployment can also hold a shared credential for a platform, the
organisation's. A launch uses your own credential first and the
organisation's only when you have none (a launch from the CLI can prefer the
shared one with `prefer: shared` on the platform). See
[github-access.md](github-access.md).

## GitHub

Point a platform at GitHub in `~/.config/spov/config.yaml` (`base_url`
defaults to `https://github.com`; set it for GitHub Enterprise):

```yaml
platforms:
  - name: github
    kind: github
    account: your-username     # or the org the App is installed on
```

There are three ways to give spov access. A local server takes gh's login or
an App; it refuses a plain token, which only a hosted server stores.

### Reuse your gh login (local)

If `gh` is signed in on this machine (`gh auth login` if not):

```sh
spov credentials add github --from-gh
```

spov keeps only a note that the platform's credential is gh's, and asks
`gh auth token` again at each launch, so `gh auth logout` or a re-login takes
effect at once. The token reaches what gh's grant does (usually `repo`, often
`workflow`); the credential proxy narrows what a sandbox can do with it, as
for any stored token. A hosted server refuses it.

### A GitHub App (best)

spov holds the App's private key and mints each session an installation token
narrowed to just that session's repositories. The key itself never enters a
sandbox.

Create the App under **Settings → Developer settings → GitHub Apps → New
GitHub App**:

- **Repository permissions:** `Contents` read and write, `Metadata`
  read-only, and `Pull requests` and `Issues` read and write if the agent
  should open pull requests or work with issues.
- Uncheck **Webhook → Active**.
- Generate a private key, then install the App on the repositories sandboxes
  should reach.

On a local server, give spov the path to the key rather than its contents:

```sh
spov credentials add github --app-id YOUR_CLIENT_ID \
  --key-file ~/Downloads/your-app.private-key.pem \
  --installation-id 12345678     # optional; discovered if omitted
```

On a hosted server there is one App, the organisation's, and everyone on the
deployment uses it. An admin stores it under **Organisation** in Settings,
where spov checks it before keeping it, or the deployment's environment sets
it (`SPOV_GITHUB_APP_ID` and `SPOV_GITHUB_APP_PRIVATE_KEY`), which wins. Users
do not store an App; they can store a token of their own (below).
[github-access.md](github-access.md) covers the rest.

An installation token lasts an hour. While a session is in use, the server
mints a new one before the old one expires and hands it to the sandbox, so git
and `gh` keep working for as long as the session does. A session nobody is
using is not renewed, because writing into a sandbox would keep it alive. When
its person comes back, the token is renewed within a minute. If a renewal
fails, the session page says so.

### A personal access token (hosted)

`spov credentials add github` on a hosted server stores a token, which is used
as it is, like a Forgejo token. A fine-grained token can read every public
repository, so the repository pickers mark those as possibly read-only: the
token can push only to repositories it was given.

Whenever a launch uses a stored token, the forge is asked first whether the
token can see each repository, and the launch is refused before any sandbox
exists if it cannot.

## Forgejo, Gitea and GitLab

```yaml
platforms:
  - name: codefloe
    kind: forgejo              # github | forgejo | gitea | gitlab
    base_url: https://codefloe.com
    account: your-username
```

```sh
spov credentials add codefloe
```

For Forgejo, create a token under **Settings → Applications → Generate New
Token**, with **Specific repositories** listing the repositories sandboxes
should reach, and `repository` set to *Read and write*. The session gets that
token as it is: spov cannot mint narrower ones, because Forgejo only creates
tokens against your account password, and it cannot see a token's scope, so
the narrowing is yours to set. The session page says when a sandbox holds a
stored token.

Gitea and GitLab tokens are stored the same way. What to create for each, and
what the proxy lets a stored token do, is in [forges.md](forges.md).

## Model providers

**API keys.** spov hands `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` and the
other provider keys it recognises to the session's credential proxy, which
adds each to requests for its provider. OpenCode in the sandbox holds a
placeholder and chooses among the providers. With a local server an exported
key just works. To avoid exporting one in every shell, store it:

```sh
spov credentials add ANTHROPIC_API_KEY
```

An exported key wins over a stored one. A local server reads its keys when it
starts, so one added while it runs is seen after it restarts: it exits after
a minute with nothing to do, or end the `spov serve --local` process. A hosted server reads your stored keys at each launch.

**OpenCode Go and Zen.** `spov login opencode` (or **Connect OpenCode** on the
Settings page) signs in to your OpenCode Console account in the browser. The
server keeps the sign-in, refreshes it when a session needs it, and gives it
to each session through the credential proxy, so the sandbox never holds it.
Models are then `opencode-go/<model>` for the Go subscription and
`opencode/<model>` for Zen. This sign-in is spov's own: it does not reuse, and
cannot sign out, the one your local OpenCode holds. A console API key stored
with `spov credentials add OPENCODE_API_KEY` works too. `spov credentials`
says whether you are connected, and `spov logout opencode` forgets the
sign-in.

Which model a session starts on is `--model`, then your default model (set
in the dashboard's Settings, on a hosted server), then OpenCode's own choice.
The TUI, or the session page, switches it while the session runs.

## Private registries and other services

A **routed credential** is a secret for something other than your forge, such
as a private npm registry, an internal API or a Maven repository, together
with where it may be used. The credential proxy holds it outside the sandbox
and adds it only to requests for the hosts (and, optionally, the paths) you
name. Inside the sandbox it is a placeholder in `SPOV_CREDENTIAL_<NAME>`, so
the agent never holds the secret, and a request anywhere else goes out
without it. Routed credentials need the credential proxy: a server without it
refuses a launch that would attach one.

```sh
# A bearer token for one host:
spov credentials add acme-api --host api.acme.com --bearer
# Basic auth, or a header of your own, below a path:
spov credentials add acme-files --host files.acme.com --path /team/ --basic deploy
spov credentials add acme-search --host search.acme.com --header X-Api-Key
```

The secret is prompted for without echo (or read with `--secret-file`) and
stored unchecked. `--host` repeats, and the hosts must be exact names; a route
can never cover a forge's or a model provider's host.

**Presets** fill in the scheme and the host from a registry's URL, and write
the client's own config file in the sandbox with the placeholder in it:

| Preset | Options | Written in the sandbox |
|---|---|---|
| `npm` | `--url`, `--scope @acme` (optional) | `~/.npmrc`, read by npm, pnpm and Bun |
| `maven` | `--url`, `--id` (the repository's id in `pom.xml`), `--user` | `~/.m2/settings.xml` |
| `nuget` | `--url` (the feed's `index.json`), `--user` | `~/.nuget/NuGet/NuGet.Config` |
| `pypi` | `--url` (the index's `/simple/` URL), `--user` | `pip.conf` and `uv.toml`, as an extra index |

```sh
spov credentials add acme-npm --preset npm --url https://npm.acme.com/ --scope @acme
```

**Which sessions get one.** Every session and job gets each routed credential
you hold by default. `--attach asked` stores one that only a launch naming it
gets. To narrow per launch, use `--credentials a,b` (exactly these,
asked-only ones included) or `--no-credentials` on `spov fly` and
`spov jobs create`. In config, `sandbox.credentials` keeps only the ones it
names, or `[none]`; jobs use `sandbox.job_credentials` instead and never take
the interactive list. A project file can only take credentials away, never
add one
([configuration.md](configuration.md#a-project-file-can-only-take-routed-credentials-away)).
`sandbox.needs` lists hosts a project needs, and a launch says when no routed
credential covers one. On a hosted server an admin can store the
organisation's own with `--org`, which every member's sessions get. The
session page lists each credential a session holds, with its hosts, never the
secret.

Values that are not secret, such as a registry's URL or a feature flag, belong
in `sandbox.env` instead ([The sandbox](sandbox.md#environment-variables)).
