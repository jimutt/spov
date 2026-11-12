# Configuration reference

spov reads its configuration in layers, each overriding the one before:

1. built-in defaults;
2. the **user** file, `~/.config/spov/config.yaml` (or `$XDG_CONFIG_HOME/spov/config.yaml`);
3. the **project** file, `.spov.yaml` in the current directory or its nearest ancestor;
4. `SPOV_*` **environment variables**.

A later layer **replaces** a list rather than merging it, so a project that
declares `repos:` lists every repository it wants. Secrets never go in these
files; `spov credentials add` stores them in the credential store.
`spov config show` prints the merged result and the files and variables it was
built from.

A project file usually arrives with a repository someone else wrote, so it may
not set `platforms` or `server.url`: both decide where your tokens are sent.
spov refuses a `.spov.yaml` that sets either, naming the file and the key; move
them to the user file (or use `SPOV_SERVER` or `--server` for the server).

`spov serve` reads no project file, local or hosted: its configuration is the
user file and the environment. The CLI's local server is started in its state
directory, so the directory you ran a command in does not configure it. A hosted
server reads its own; see [`deploy/README.md`](../deploy/README.md).

## Every key

This sample sets every key spov reads, as a user file: a project file may not set
`platforms` or `server.url`. `scripts/check.sh` checks it: it must load and
validate, set every key the schema has, and come back whole from
`spov config show` (`TestTheConfigurationSampleIsComplete`). Two keys are
commented out, because setting them would make the file invalid.

```yaml
version: 1                          # the only schema version

# --- Git platforms and repositories (CLI) ---------------------------------
platforms:
  - name: codefloe                  # referenced by repos[].platform and by `spov credentials add`
    kind: forgejo                   # github | forgejo | gitea | gitlab (docs/forges.md)
    base_url: https://codefloe.com  # required for forgejo and gitea; github defaults to https://github.com (set it for GHE), gitlab to https://gitlab.com
    account: alice                  # the user the token belongs to, or the org a GitHub App is installed on
  - name: github
    kind: github
    base_url: https://github.com
    account: acme
    prefer: personal                # personal | shared: which credential a launch from here tries first on a hosted server (docs/github-access.md)

repos:                              # cloned into /workspace/<dir> when a session starts; a gitlab owner may be a group path (group/subgroup)
  - platform: codefloe
    owner: alice
    name: widgets
  - platform: github
    owner: acme
    name: api
    dir: backend                    # default: the repository's name

landing: widgets                    # the repository directory OpenCode starts in; default /workspace

# --- The sandbox ------------------------------------------------------------
sandbox:
  cpu: 1                            # default 0.5
  memory_mib: 4096                  # default 2048
  cpu_limit: 2                      # hard ceilings, at least the request; 0 means none
  memory_limit_mib: 8192
  timeout: 4h                       # an interactive session's lifetime; default 4h, at most 12h. A job's follows from its clock
  idle_timeout: 30m                 # an interactive session's; default 30m, at least 1m, at most timeout. A job's equals its timeout
  regions: [eu]                     # Modal regions, broad (us, eu, ap) or narrow (us-east, eu-west, ...)
  cloud: aws                        # aws | gcp | oci | auto
  workdir: /workspace               # absolute; every session lands in /workspace regardless
  egress: install                   # open | install | agent; default open, for sessions and jobs
  egress_extra_domains: [pkg.example.com]  # added to the preset, at most 16, sent with each launch; not allowed with open
  inbound_cidrs: [203.0.113.0/24]   # who may reach the sandbox's edge
  git_access: repos                 # repos | all, for spov fly; the server treats it as a preference
  job_git_access: repos             # repos | all, for spov jobs create; never inherits git_access
  credentials: [acme-npm]           # keep only these of your routed credentials for spov fly, or [none]; only narrows (or pass --credentials / --no-credentials)
  job_credentials: [none]           # the same for spov jobs create; never inherits credentials
  needs: [npm.acme.com]             # hosts the project needs; a launch says when no routed credential covers one
  job_max_duration: 2h              # a job's wall clock when --max-duration is unset; at least 1m, clamped to your limit (quotas.max_job_duration)
  job_sync_hold: 1h                 # how long a finished sync job's sandbox is held for spov jobs apply when --hold is unset; 30m by default, at least 1m
  sync: false                       # unset (the default): a plain spov fly inside a checkout syncs it; true: always (--sync); false: never (--no-sync)
  addons: [rust, postgres@16, node@22]  # dotnet, java, php, playwright, postgres, redis, ruby, rust; or bun, go, node, python, uv
  apt: [libvips-dev]                # extra Debian packages
  env:                              # environment variables in the sandbox, for sessions and jobs; not for secrets (see below)
    NODE_ENV: development
    OPENCODE_LOG_LEVEL: info
  # image: my-custom-image          # an image spov did not build; refused together with addons or apt
  # gpu: a10g                       # recognised only to be refused: v2 sandboxes have no GPU

# --- OpenCode ------------------------------------------------------------------
opencode:
  version: 2.2.5                    # the pin: the sandbox must match it, the local client is graded against it
  port: 4096

# --- Modal -----------------------------------------------------------------------
modal:
  app: spov                         # default spov
  environment: main                 # default: Modal's own default environment
  build_on_demand: true             # the server builds an add-on variant on its first launch; default true

# --- Commit identity ----------------------------------------------------------------
git:
  name: Alice Example               # default: your local git config
  email: alice@example.com
  allow_all: false                  # server only: lets git_access all use the deployment's own credentials (a local server always allows it)

# --- The server the CLI talks to -----------------------------------------------------
server:
  url: https://spov.example.com     # --server and SPOV_SERVER take precedence; empty means a local server
  trusted_proxies: [10.0.0.0/8, 192.0.2.10]  # spov serve only: reverse proxies whose X-Forwarded-For the per-address limits read; default none

# --- Read only by spov serve ---------------------------------------------------------
quotas:                             # per-user defaults; 0 means the built-in default, not unlimited
  max_concurrent_sessions: 3
  max_concurrent_jobs: 2
  max_job_duration: 4h
  max_timeout: 8h
  max_cpu: 4
  max_memory_mib: 16384

snapshots:                          # exit snapshots for resume; servers under the credential proxy that keep their sessions
  enabled: true                     # false takes no new snapshots; those kept age out
  retention: 168h                   # 1h to 720h (Modal keeps one 30 days)
  max_per_user: 20                  # oldest pruned first; 0 means no cap

history:                            # a local server's session history, under $XDG_DATA_HOME/spov/sessions
  retention: 720h                   # kept this long after a session stopped; 0 keeps it forever

metrics:                            # see docs/metrics.md; OTLP is configured by OTEL_EXPORTER_OTLP_* instead
  prometheus_listen: 127.0.0.1:9464 # serves GET /metrics on its own listener, unauthenticated; empty serves none

prep:                               # the server's own model call that prepares a job's goal
  base_url: https://openrouter.ai/api/v1  # any OpenAI-compatible endpoint; requires model
  model: anthropic/claude-haiku-4.5
  key_env: SPOV_PREP_API_KEY        # the variable holding its key; default: detected from the environment
```

## Who reads what

| Section | Read by |
|---|---|
| `repos`, `landing`, `server` | the CLI |
| `platforms` | the CLI, and `spov serve`, which resolves its deployment credentials by platform name |
| `sandbox`, `git.name`, `git.email` | the CLI, which sends them with each request; the server uses its own as defaults |
| `opencode` | the CLI, and `spov serve`, whose pin it publishes |
| `modal`, `quotas`, `prep`, `metrics`, `git.allow_all`, `server.trusted_proxies` | `spov serve` |

The server's own `sandbox.addons` and `sandbox.apt` apply to a request that
names neither, such as a session started from the browser.

`platforms` and `server` belong in the user file only (above). `spov serve`
reads no project file.

### Behind a reverse proxy: `server.trusted_proxies`

`spov serve` limits how often one client address may start a sign-in and poll
for a CLI token (the limits are in [`server.md`](server.md#limits)). The address
is the connection's peer. Behind a reverse proxy or a load balancer that peer is
the proxy, so every client shares one address, and one busy office or one
attacker can use up everyone's sign-ins, until you name the proxy here.

`server.trusted_proxies` is a list of addresses and CIDR ranges, empty by
default. For a request whose peer is in it, spov reads `X-Forwarded-For` from
the right and takes the first address that is not itself in the list, so a
client cannot choose its address by sending the header itself. From any other
peer the header is ignored, and the first such request logs one warning naming
this setting. An entry that is not an address or a range, or a range with host
bits set (`10.0.0.1/8`), refuses to start the server. Like every list it is set
only in the file, not through `SPOV_*`. Name only the proxies you run: a range
that covers the clients themselves lets them choose their address. The setting
is read by these limits alone; cookies, redirects and the browser guard never
read a forwarded header.

### A project file can only take routed credentials away

Every session gets the routed credentials you hold (`spov credentials add <name>
--host ...`), except those you stored with `--attach asked`. `sandbox.credentials`,
in your user file or a project file, keeps only the ones it names, and `[none]`
keeps none; it never attaches one you did not already get, so a repository you
clone from someone else cannot send your credentials anywhere new. A launch's own
`--credentials a,b` attaches exactly those, asked-only ones included, and
`--no-credentials` attaches none.

### A project file can ask for git access `all`

`sandbox.git_access` and `sandbox.job_git_access` are allowed in a project file.
With `all`, the sandbox's proxy adds your token to requests for **every**
repository on each forge you hold a token for, not only the session's. A
repository you clone from someone else can therefore ask for it, and its
`AGENTS.md`, which the agent is told to read, can then instruct the agent to
fetch your other private repositories and send them out over open egress. Before
launching in a repository you did not write, check its `.spov.yaml` for
`git_access: all`. The session page lists each credential's git access, and
`--git-access repos` on the launch overrides the file.

## Environment variables

Every scalar key has a variable built from its path, upper-cased, with dots as
underscores: `SPOV_SANDBOX_TIMEOUT=2h`, `SPOV_MODAL_BUILD_ON_DEMAND=false`,
`SPOV_GIT_ALLOW_ALL=1`. Lists (`platforms`, `repos`, `regions`, `addons`, `apt`
and the other list keys) can be set only in a file. `spov config show` lists
the variables it applied and the `SPOV_*` variables it ignored. A value that is
empty counts as unset. A variable that is not a config key is reported as
ignored there, even when something else reads it, such as `SPOV_SERVER` below.
`SPOV_SERVER_URL` does set `server.url`.

### Variables that are not config keys

These are read directly, not through the config layers. A hosted deployment's
full list, with what each one does, is in [`deploy/README.md`](../deploy/README.md).

| Variable | Read by | Meaning |
|---|---|---|
| `SPOV_SERVER` | the CLI | The server's URL; `--server` wins over it, and it wins over `server.url`. |
| `SPOV_SERVER_TOKEN` | the CLI, `spov serve` | The CLI sends it as its bearer token. A hosted server refuses to start without it. |
| `SPOV_DATABASE_URL` | `spov serve` | Postgres. Jobs and browser login need it. |
| `SPOV_MIGRATE_ON_START` | `spov serve` | `1`, `true`, `yes` or `on` applies the migrations before listening. |
| `SPOV_PUBLIC_URL` | `spov serve` | The externally visible origin, which the browser guard, cookies and login redirects use. |
| `SPOV_LISTEN` | `spov serve` | The address to bind when `--listen` is not given; default `127.0.0.1:8080`. |
| `SPOV_TOKENS_IN_SANDBOX` | `spov serve` | `1` lets a hosted server built without the credential proxy start, with every sandbox holding its tokens. |
| `SPOV_CREDENTIAL_KEY`, `SPOV_CREDENTIAL_KEY_OLD` | `spov serve` | Seals users' stored credentials; the second is the previous key during a rotation. |
| `SPOV_GITHUB_APP_ID`, `SPOV_GITHUB_APP_PRIVATE_KEY` | `spov serve` | The organisation's GitHub App. |
| `SPOV_PLATFORM_TOKEN_<NAME>` | `spov serve` | The organisation's token for the platform called `<name>`, upper-cased, with anything but letters and digits as `_`. |
| `SPOV_GITHUB_OAUTH_CLIENT_ID`, `SPOV_GITHUB_OAUTH_CLIENT_SECRET` | `spov serve` | The OAuth app for browser login; both or neither. |
| `SPOV_ALLOWLIST_FILE` | `spov serve` | Seeds a new deployment's allowlist. |

## Environment variables in the sandbox

`sandbox.env` sets environment variables in every session's and job's
sandbox, as a map of name to value. It is for ordinary settings: `NODE_ENV`,
a test database's URL, a locale, a feature flag, OpenCode's own
`OPENCODE_LOG_LEVEL`. They are not secrets: spov stores them with the
session, shows them on the session page and in `spov ls --json`, and they are
visible in Modal's control plane. A token or a key belongs in a routed
credential (`spov credentials add <name> --host ...`), which the credential
proxy holds outside the sandbox.

- **Values are text.** Quote one YAML would read as something else: `"true"`,
  `"1.10"`, `"0755"`, `"2024-01-01"`. An unquoted one is refused rather than
  rewritten (YAML would send `true` as `1` and `1.10` as `1.1`).
- **Layers merge by name.** A project file's `sandbox.env` adds to your user
  file's, and overrides it name by name, unlike lists, which a later layer
  replaces. An empty `env:` in a project file clears the user file's. There
  is no `SPOV_*` variable for it.
- **`--env KEY=VALUE`** on `spov fly` and `spov jobs create` sets one more,
  over the config; repeat it for several. `KEY=` sets an empty value.
- **The server's own `sandbox.env`** is the base under every request's, so a
  session started from the browser gets it too. A local server reads your
  user file, so it is the same map.
- **A resumed session** gets its variables again.

Some names are refused, because they would change how spov sets up the
sandbox, how a forge CLI authenticates or how OpenCode is configured. A
refusal names the variable and the rule:

- every name spov sets itself: `HOME`, `PATH`, the add-ons' variables, the
  proxy's, the provider keys (`ANTHROPIC_API_KEY` and the rest) and
  `OPENCODE_PASSWORD`;
- anything starting with `SPOV_`, `GIT_`, `LD_`, `XDG_` (it moves where
  OpenCode, mise and the forge CLIs read the config spov writes) or `MISE_`;
- `BASH_ENV`, `ENV`, `SHELLOPTS`, `BASHOPTS`, `IFS`; the proxy and trust-store
  variables (`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`,
  `SSL_CERT_FILE`, `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`,
  `CURL_CA_BUNDLE`, `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `JDK_JAVA_OPTIONS`);
- the forge CLIs' token and host overrides (`GH_TOKEN`, `GITHUB_TOKEN`,
  `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, `GH_HOST`, `GH_CONFIG_DIR`,
  `GITLAB_TOKEN`, `GITLAB_ACCESS_TOKEN`, `GITLAB_HOST`, `GITLAB_URI`, `GL_HOST`,
  `GLAB_CONFIG_DIR`, `GITEA_TOKEN`, `GITEA_SERVER_URL`);
- the OpenCode variables that touch its authentication, where its
  configuration or model list comes from, or its own plumbing:
  `OPENCODE_PASSWORD`, `OPENCODE_SERVER_PASSWORD`, `OPENCODE_API_KEY`,
  `OPENCODE_CONFIG`, `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG_DIR`,
  `OPENCODE_CLI_CONFIG_CONTENT`, `OPENCODE_CONFIG_PROJECT_DISABLE`,
  `OPENCODE_DISABLE_PROJECT_CONFIG`, `OPENCODE_MODELS_URL`,
  `OPENCODE_MODELS_PATH`, `OPENCODE_CLIENT`, `OPENCODE_ROUTE`,
  `OPENCODE_SESSION_ID`, and anything starting with `OPENCODE_PTY_` or
  `OPENCODE_SSH_ASKPASS_`. Every other `OPENCODE_*` is allowed.

Names are compared without regard to case. A name is letters, digits and `_`,
not starting with a digit, at most 128 bytes; a value is at most 4 KiB. A
session takes at most 64 variables and 32 KiB in all, the server's base
included. `spov fly` and `spov jobs create` check before asking the server, the
server checks again, and `spov serve` refuses to start on a bad
`sandbox.env` of its own.

## Add-ons

`sandbox.addons` takes `name` or `name@version`. A bare name gets spov's tested
version. spov checks a version's shape, not a list, so a release newer than spov
works; one that does not exist fails the build:

| Add-on | Versions | Default |
|---|---|---|
| `postgres` | a major, from apt.postgresql.org: `postgres@14` | 17 |
| `php` | major.minor, from deb.sury.org: `php@8.1`; Composer comes with it | 8.4 |
| `redis` | major.minor or exact, from packages.redis.io: `redis@7.2`, `redis@7.2.16` | 8.0 |
| `node`, `python`, `go`, `bun`, `uv` | any version mise installs: `node@22` | the base image's |

Naming a default language pins it in the image in place of the base's version;
the base's stays installed, and a repository's own version file still wins. An
entry at the base's own version builds nothing. Any add-on, or any `apt` package the base image
does not already have, makes the session's image a variant of the base. A server
builds a variant on its first launch, which takes seconds to a few minutes, once,
unless `modal.build_on_demand` is false. In that case an operator prebuilds it
with `spov serve image build --addons ... --apt ...`. The default toolchain and
the add-ons are described in [The sandbox](sandbox.md).

### Partial versions follow new releases

A partial version such as `node@22`, `java@21`, `postgres@14` or `rust@stable`
is resolved by the server to the newest release it covers, from mise's version
index or the add-on's apt repository, and the image is named after the exact
release. The answer is kept for an hour. The server needs egress to
`mise-versions.jdx.dev`, `apt.postgresql.org`, `packages.sury.org` and
`packages.redis.io`; a lookup that fails falls back to the version as written.

When a new patch is out, a launch boots the newest image already built for the
same add-ons and builds the new one in the background, so the next launch gets
it. With `modal.build_on_demand: false` launches stay on the last build until an
operator runs `spov serve image build --addons ...`, which resolves, builds and
prints the exact versions. A session's `installed_addons` says which releases
its sandbox got.

A local server keeps these answers and the record of what is built in
`variants.json` beside its `server.json` in `~/.local/state/spov`; a hosted
server keeps the records in its database.

### Pruning unused images

The server records every image it publishes and when a launch last used it, and
once a day deletes those unused for long enough:

| Image | Deleted after this long unused |
|---|---|
| a variant a newer build of the same add-ons replaced | 7 days |
| the newest variant of its add-ons, only while `build_on_demand` is on | 30 days |
| a base image of an older OpenCode pin, or a variant built on one | 30 days |
| a proxy image of an older spov build | 7 days |

The current base and proxy images, and any image a live session was created
from, are never deleted. A launch that needs a deleted variant builds it again;
a deleted base image is rebuilt with `spov serve image build`.
`spov serve image prune --dry-run` lists what would go, and `spov serve image
prune` runs it now; run it from the deployed build, since it spares that build's
base and proxy images.

Each deployment keeps its own registry. If two share a Modal environment, such as
a local and a hosted server, one can delete an image the other still records. A
launch that hits such an image marks it deleted and the next launch rebuilds it;
`spov serve image build --rebuild` republishes one by hand. Give each deployment
its own Modal environment to avoid this. An image that is not in the registry is
never deleted by spov; delete it in Modal's console if it is unused.
