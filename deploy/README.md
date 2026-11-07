# Deploying spov

One image serves the API and runs migrations; which it does is the
argument (`serve`, or `serve migrate up`).

Recorded in [DL-010](../decisions/records/server/DL-010.md).

## Required environment

| Variable | Why |
|---|---|
| `SPOV_PUBLIC_URL` | The externally visible origin. **Required behind a proxy**: the `Host` header names it rather than the listen address, and the browser guard runs before authentication — omit it and every request is refused with 403. It is also the OAuth redirect base and the cookie origin. |
| `SPOV_DATABASE_URL` | Postgres. Environment-only; a database URL carries a password. Without it the server runs on an in-memory store that a restart empties, and jobs and browser login are unavailable. |
| `SPOV_SERVER_TOKEN` | A single shared bearer token. The server refuses to start without one. Prefer per-user CLI tokens (`spov login`, below) for people; this one identifies nobody and cannot be revoked without restarting the server. |
| `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET` | The deployment's own Modal credential, minted separately rather than reused from a laptop. |
| `MODAL_ENVIRONMENT` | The Modal environment sandboxes are created in. Use a dedicated one: DL-004's reaper terminates any `spov=1` sandbox past its deadline, and sharing an environment with your local work puts that work in range. |

Optional: `SPOV_LISTEN`, `ANTHROPIC_API_KEY` and other model provider keys.

`SPOV_LISTEN` is the address to bind, used when `--listen` is not given. The
default is `127.0.0.1:8080`, loopback, so a server is reachable from the
network only when its deployment says so (DL-010). That is unreachable from
outside a container, so `compose.yaml`, `kamal/config/deploy.yml` and
`k8s/deployment.yaml` each set `:8080` themselves; a deployment of your own
must too. Any config key can also be
set as `SPOV_<PATH>` (for example `SPOV_SANDBOX_TIMEOUT`); the server reads its
user file and the environment only, never a `.spov.yaml`. See
[`docs/configuration.md`](../docs/configuration.md).

## The credential proxy

Forge tokens and model provider keys reach a sandbox through a proxy sidecar
and never sit in its main container (DL-034). A hosted server **refuses to
start** unless its binary embeds that proxy, which `scripts/build-proxy.sh`
builds in before `go build`, or `SPOV_TOKENS_IN_SANDBOX=1` is set to run
without it, with every sandbox holding its tokens and keys. The release build
and `deploy/Dockerfile` both run the script, so an image built from the
Dockerfile always has the proxy.

## The sandbox image

A launch never builds the base image. After the first deploy, and again after
every OpenCode pin bump, run `spov serve image build` once with the server's
Modal environment; it returns at once when the image is already published, so
it is safe before every deploy. Add-on variants are built on first use unless
`modal.build_on_demand` is false. See
[`docs/configuration.md`](../docs/configuration.md#add-ons).

## Browser login

Recorded in [DL-007](../decisions/records/server/DL-007.md). Login is
GitHub OAuth, and it is **off unless configured**: without an OAuth app
the server serves its API on the bearer token alone, which is what a
single-operator deployment runs until it has users.

| Variable | Why |
|---|---|
| `SPOV_GITHUB_OAUTH_CLIENT_ID`, `SPOV_GITHUB_OAUTH_CLIENT_SECRET` | The OAuth app. Both or neither: half a credential is refused at startup. |
| `SPOV_ALLOWLIST_FILE` | Seeds a **new** deployment's allowlist. Default `/etc/spov/allowlist`. After the first boot the allowlist lives in the database and this file is ignored. |

Login also needs `SPOV_DATABASE_URL` (the sessions and the users live
there) and `SPOV_PUBLIC_URL` (GitHub redirects back to it), so a server
with an OAuth app but neither of those refuses to start rather than
failing at the first login attempt.

Register the OAuth app on GitHub with the callback
`$SPOV_PUBLIC_URL/oauth/github/callback`. A staging deployment needs its
own app, since the callback is part of the registration.

The app needs no configuration on GitHub's side: spov requests
`read:user` and `user:email` itself on every login
(`internal/auth/github.go`), and GitHub shows that consent screen to the
user. `user:email` is required rather than convenient — a private address
is only in `/user/emails`, and the login identity is the address that is
both primary and verified there. The device flow (`spov login`) is spov's
own authorization server (`/device/code`, `/device/token`, approval at
`/device`); GitHub is never a party to it, so there is nothing
device-related to enable on GitHub.

@decision(DL-105)

### The allowlist

**The allowlist lives in the database**, and is managed through the API.
The file below only bootstraps a new deployment.

Only a GitHub address that is both **primary and verified** is accepted,
because GitHub lets anyone add an unverified one.

#### Managing it

Every call needs an administrator's credential **belonging to the org
the allowlist governs** — a browser session, or a per-user token from
`spov login`. The shared `SPOV_SERVER_TOKEN` is *not* one: DL-006 gives
it its own org, so it is refused here rather than allowed to edit an
allowlist nothing reads.

```sh
curl -H "Authorization: Bearer $TOKEN" https://spov.example.com/api/v1/allowlist

# Add someone, or change their admin rights (the same call does both).
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"admin":false}' https://spov.example.com/api/v1/allowlist/bob@example.com

# Remove someone. This also ends their browser sessions.
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://spov.example.com/api/v1/allowlist/bob@example.com
```

Each replica keeps a cached copy, refreshed every 10 s, and consults it
on every request — so removing an entry locks that person out on their
next request after at most that interval, **on every replica**. (A file
reloaded by signal could not do this: a signal reaches one process, and
a replica that missed it never learned.) For an already-open SSE stream
the bound is the stream's lifetime instead, since the cookie is checked
when it opens.

Three changes are refused, because the API is the only way back:

- removing your **own** admin rights, or deleting your own entry — ask
  another admin;
- any change that would leave the org with **no** administrator;
- any change by someone whose own admin rights have already been
  removed. Reads keep the 10 s bound above; writes are checked against
  the database, so a removed admin cannot use the window to put
  themselves back.

#### Seeding a new deployment

`SPOV_ALLOWLIST_FILE` is imported **once**, at startup, when the org has
no administrator. One entry per line: an address, optionally followed by
`admin`. `#` starts a comment, and matching is case-insensitive.

```
# The people who may use this server.
alice@example.com admin
bob@example.com
```

Mount it from a host directory, as `deploy.yml` does with `volumes:`, or from a
Kubernetes ConfigMap. On a **first
boot** — nothing in the allowlist at all — a file that fails to parse,
that allows nobody, or that is missing is fatal: a server nobody can log
in to looks like an OAuth problem rather than a mount problem. Once the
org has an administrator the file is ignored, and the server says so at
startup — editing it after that does nothing.

The import re-runs whenever the org has **no administrator**, not only
when the table is empty. That is the break-glass for an org whose last
admin went some other way (a stray `DELETE` in SQL): restore the file,
restart, and it is imported again. The refusals above are what keep that
path unreachable in normal operation.

If that happens and the file is *gone* — a reasonable state, given it is
documented as ignored — the server starts anyway and says loudly that
nobody can manage the allowlist. Everyone's existing access is
unaffected; refusing to start would take the whole deployment down over
a management problem.

#### SIGHUP

The server traps SIGHUP and reloads nothing: it reports where the
allowlist lives. Go's default action for SIGHUP is to *terminate*, so
without the trap a signal sent by habit would take the server down.

```sh
docker compose -f deploy/compose.yaml kill -s HUP server   # compose
kubectl exec deploy/spov -- kill -HUP 1                    # kubernetes
```

Sessions are server-side rows (`web_sessions`), so revoking one is a
delete: `GET /api/v1/me/sessions` lists a user's logins and
`DELETE /api/v1/me/sessions/{id}` ends one. The cookie is
`HttpOnly; SameSite=Lax; Path=/`, and `Secure` whenever `SPOV_PUBLIC_URL`
is https — on a plain-http trial it is not, and the server warns.

`SPOV_MIGRATE_ON_START=true` applies migrations before listening. It is
for compose and development. A real deployment migrates in a step that
can fail the deploy — the Kamal `pre-deploy` hook, or the Kubernetes
Job — so a bad migration never reaches a serving container.

## CLI login

Recorded in [DL-006](../decisions/records/server/DL-006.md). Once browser
login is configured, spov is also its own device-flow authorization
server, so a terminal can get its own token:

```sh
spov login --server https://spov.example.com
```

It prints a URL and a code, you approve it in a browser as yourself, and
the token is stored in the CLI's credential store under `server:<host>`.
After that every command authenticates without `SPOV_SERVER_TOKEN`.

`spov whoami` shows who the server thinks you are; `spov logout` revokes
the token server-side and deletes the local copy — and still deletes it
locally if the server cannot be reached, so nothing stale lingers.

Tokens last 90 days. `GET /api/v1/me/tokens` lists them and
`DELETE /api/v1/me/tokens/{id}` revokes one; `current` means the token
making the request. The token itself is shown once, in the terminal that
asked — only its hash is stored, so a database dump cannot be replayed.

The device endpoints (`/device/code`, `/device/token`) are
unauthenticated by construction: a CLI polling for a token has no
credential yet. The approval page at `/device` needs a login, and five
wrong codes lock that user out for fifteen minutes.

## Credentials

Recorded in [DL-042](../decisions/records/git/DL-042.md). Every
credential a sandbox receives is resolved in two tiers, **the user's own
first, then the deployment's**. A deployment that configures only the
org tier uses its credentials for every session.

| Variable | Why |
|---|---|
| `SPOV_CREDENTIAL_KEY` | Seals user credentials at rest, AES-256-GCM. Base64, 32 bytes: `openssl rand -base64 32`. Unset, the server stores no user credentials and answers 501 to anyone trying. |
| `SPOV_CREDENTIAL_KEY_OLD` | The previous key during a rotation. Rows sealed under it still open while they are rewritten. Refused without the current key. |
| `SPOV_GITHUB_APP_ID`, `SPOV_GITHUB_APP_PRIVATE_KEY` | The org tier for GitHub — one App for everyone (DL-041). An admin can store it in Settings instead, sealed in the database; when these are set they win and Settings only shows it. See [GitHub access](../docs/github-access.md). |
| `SPOV_PLATFORM_TOKEN_<NAME>` | The org tier for one configured platform, named after it: a platform called `codefloe` reads `SPOV_PLATFORM_TOKEN_CODEFLOE`. Non-alphanumerics become `_`. |

### One person, no organisation credential

A single-operator deployment needs no org tier at all: configure OAuth,
log in, and store a personal token in the dashboard's Settings or with
`spov credentials add github`. Launches use your own credential first;
the deployment tier is only a fallback, and omitting it is a supported
shape rather than a missing step. `SPOV_CREDENTIAL_KEY` is still worth
setting: without it the server stores no user credentials at all.

An org token variable attaches to a platform in the server's user file
([The server's user file](#the-servers-user-file)): a variable naming no
declared platform is ignored. Declare the platform before setting the
variable.

@decision(DL-105)

Each key seals under a version derived from the key itself, so a key
always opens what it sealed and the order the two variables are set in
does not matter.

To rotate: set the new key as `SPOV_CREDENTIAL_KEY`, move the previous
one to `SPOV_CREDENTIAL_KEY_OLD`, and deploy. Existing rows keep opening
under the old key and are rewritten under the new one as users store
credentials. **There is no bulk rewrite yet**, so leave the old key set
until you are willing to have anyone who has not re-stored theirs be
told to add it again — the fallback is a clear message, not a broken
session.

A user stores their own with:

```sh
spov credentials add github                # a fine-grained GitHub PAT
spov credentials add forgejo:codefloe.com  # a repository-scoped token
spov credentials add ANTHROPIC_API_KEY
spov credentials                           # what the server holds for you
spov credentials remove github
```

A git token is verified against its platform before it is stored, so a typo
fails there rather than partway through a launch. A model provider key has
nothing to verify against and is stored as given. No endpoint returns a
stored credential; the API can say one exists and which account it
proved to own, and nothing more.

**Losing `SPOV_CREDENTIAL_KEY` makes stored user credentials, and an
organisation App stored in Settings, unrecoverable.** Users re-enter their
own and an admin stores the App again; nothing else breaks. Until then a
launch that would use an unreadable credential fails and says to store it
again, rather than using the other tier's (DL-042). Keep it in the same
secret store as `SPOV_SERVER_TOKEN`.

Note the authorisation consequence, stated plainly in DL-042: under the
org GitHub App, any allowlisted user can start a session on any
repository the App is installed on. The allowlist and the App's
installation scope are the boundary; per-repository access control is a
later decision.

[GitHub access](../docs/github-access.md) sets out the four kinds of access
(signing in, the App, the allowlist, a sandbox's git access, which on the
organisation's credential needs `SPOV_GIT_ALLOW_ALL` for `all`), how to move the
App from the environment into Settings, and what replacing or removing it does
to running sessions.

## The server's user file

`spov serve` reads its user file and the environment only, never a
`.spov.yaml`. The environment carries secrets and the visible origin;
everything else server-wide lives in the user file:
`platforms` (which the org-tier token variables attach to),
`git.allow_all`, `sandbox` defaults, `quotas`, and `prep`. See
[`docs/configuration.md`](../docs/configuration.md) for every key; no key
in the file takes a secret.

The file ships with the deployment files and every template mounts it,
so there is nothing to provide from outside — fill in the account and
deploy:

- **Kamal and Compose** mount `deploy/server-config.yaml` at
  `/home/nonroot/.config/spov/config.yaml` in the container
  (`$XDG_CONFIG_HOME/spov/config.yaml`).
- **Kubernetes** embeds the same content in the `spov-server-config`
  ConfigMap (`deploy/k8s/server-config.yaml`), mounted at the same path.
  The manifests are standalone plain YAML, so the two copies must be kept
  in step.

Without a declared platform the matching org token variable does
nothing, and launches cannot name that platform — including launches on
a user's own stored credential.

@decision(DL-105)

## Quotas

Recorded in [DL-009](../decisions/records/server/DL-009.md). Every
limit is per user and checked before anything billable. The defaults
apply whether or not you configure them — an unbounded default is how a
hosted server gets a surprise bill.

| Variable | Default | Bounds |
|---|---|---|
| `SPOV_QUOTAS_MAX_CONCURRENT_SESSIONS` | 3 | starting + running sessions per user, **excluding a job's own session** |
| `SPOV_QUOTAS_MAX_CONCURRENT_JOBS` | 2 | unfinished autonomous jobs per user ([DL-045](../decisions/records/jobs/DL-045.md)) |
| `SPOV_QUOTAS_MAX_JOB_DURATION` | 4h | the longest wall clock one job may ask for ([DL-049](../decisions/records/jobs/DL-049.md)) |
| `SPOV_QUOTAS_MAX_TIMEOUT` | 8h | a session's lifetime, itself capped at the 12 h of DL-029 |
| `SPOV_QUOTAS_MAX_CPU` | 4 | the request **and** any `cpu_limit` burst ceiling |
| `SPOV_QUOTAS_MAX_MEMORY_MIB` | 16384 | the request **and** any `memory_limit_mib` ceiling |

The same keys work under a `quotas:` section of the server's user file,
`$XDG_CONFIG_HOME/spov/config.yaml` (`/home/nonroot/.config/spov/config.yaml`
in the image). The server reads no `.spov.yaml` (DL-026).

### Preparing a job's goal

Before a job's sandbox is created, spov makes **one** model call of its
own to read the request and summarise it for the agent — the assessment
opens the session beside the user's original words, never instead of
them ([DL-047](../decisions/records/jobs/DL-047.md)). It may also
propose a check command when the caller gave none.

**It needs no configuration if a key is already set.** The first of
these present decides the endpoint and the model:

| Key | Model |
|---|---|
| `OPENROUTER_API_KEY` | the newest `anthropic/claude-sonnet-*` on the model list, else `anthropic/claude-sonnet-5` |
| `GEMINI_API_KEY` | the newest `gemini-*-flash`, else `gemini-3.8-flash` |
| `OPENAI_API_KEY` | the newest `gpt-*-luna`, else `gpt-5.6-luna` |

The model is picked again on every pass, so it follows the list
([DL-047](../decisions/records/jobs/DL-047.md)); the literal after "else" is only
the fallback.

Each is looked for in the server's environment **and** in the serving
machine's own credential store, never in a user's stored keys. Set it in
the deployment's environment; an exported key wins over a stored one.
`spov credentials add` from a CLI pointed at the server stores a key for
that user's sessions, which the prep pass does not read.

With none of them the pass is skipped and every job opens on its goal
alone. `spov serve` says so at startup.

To point it somewhere else — a LiteLLM-style gateway, a proxy, or a
model not listed above:

| Variable | Meaning |
|---|---|
| `SPOV_PREP_BASE_URL` | any OpenAI-compatible endpoint; **requires** `SPOV_PREP_MODEL`, since spov cannot know what a gateway serves |
| `SPOV_PREP_MODEL` | the model to ask for; on its own it keeps the detected endpoint |
| `SPOV_PREP_KEY_ENV` | names the variable holding the key, so this call can be billed apart from the sandboxes' |

The same keys work under a `prep:` section of the server's user file. This is the
only outbound model call the server makes; everywhere else a provider
key is a value carried into a sandbox.

**Jobs and sessions are separate pools**, not a shared one. A job owns a
session, but that session is counted against the job limit and not the
session limit — so a queue of jobs can never lock someone out of their
own `spov fly`, and someone opening sessions can never stall the queue.
The job count includes `queued` jobs, which hold no sandbox yet but are
committed to getting one.

**A job is bounded by its wall clock**
([DL-049](../decisions/records/jobs/DL-049.md)): the request's duration, else
the user's default, else 1 h, up to `SPOV_QUOTAS_MAX_JOB_DURATION`. It is one
allowance for the whole job. It counts while the job works or waits on a
permission, pauses while someone steers it, and a resume continues with what
is left. When it runs out the job is stopped, what it produced is collected,
and the verdict says the clock ended it. No count of turns or continuations
ends a job ([DL-049](../decisions/records/jobs/DL-049.md)). A job's sandbox
lives for its clock plus 10 minutes for the launch and delivery plus an hour
for steering, up to `SPOV_QUOTAS_MAX_TIMEOUT`
([DL-049](../decisions/records/jobs/DL-049.md)). There is no spend cap beyond
this and the concurrency limit, and model tokens are paid through the
provider keys the job runs with, which spov does not meter.

An over-limit size is refused with `422` naming the limit, never
silently shrunk. Exceeding the concurrency limit is `429`, since the
request would succeed once a session ends.

The burst ceilings count against the quota, not just the reservation:
Modal bills `max(requested, actual)`, so a sandbox that may burst to 64
CPUs is 64 CPUs of exposure however little it reserves.

**Per-user overrides** are an admin's to set, never a user's:

```sh
# Raise one user's concurrency and shorten their maximum session.
curl -X PATCH "$SPOV_SERVER/api/v1/users/$ID" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"quota": {"max_concurrent_sessions": 6, "max_timeout": "4h"}}'
```

A field the override omits keeps the org default, and `{"quota": {}}`
clears the override entirely. `GET /api/v1/users` lists everyone with
their effective quota; `GET /api/v1/me` reports the caller's own, so a
client can say why a create will be refused before making it.

Concurrency is counted inside the create transaction under a per-user
advisory lock, so the limit holds across replicas. `GET /api/v1/me/usage`
reports the caller's month-to-date hours and cost — **an estimate**,
and a floor: Modal bills `max(requested, actual)`.

A sandbox older than `max_timeout` plus an hour is terminated by the
reconciler (one replica at a time holds its lock and runs it) whatever the
store says, which is the backstop for a sandbox
nothing is tracking. Modal's own `Timeout` remains the last resort.

## Health

| Path | Checks | Use |
|---|---|---|
| `/livez` | Nothing. Returns 200 whenever the process can answer. | Liveness. It never touches Postgres: a database blip that failed this would restart every replica at once. |
| `/readyz` | Store ping, migrations at head. | Readiness, and the load balancer's check. |
| `/startupz` | The same, with a longer budget. | Startup, while a cold database is still waking. |

All three are unauthenticated: a load balancer has no credential.

## Metrics

Off unless you configure them ([DL-011](../decisions/records/server/DL-011.md)).
[`docs/metrics.md`](../docs/metrics.md) lists every metric.

| Variable | Effect |
|---|---|
| `SPOV_METRICS_PROMETHEUS_LISTEN` | Serves `GET /metrics` at this address, on its own listener, e.g. `127.0.0.1:9464`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Pushes over OTLP/HTTP. The exporter's other standard variables (headers, timeout, TLS, `OTEL_METRIC_EXPORT_INTERVAL`) apply. |

The `/metrics` listener has no authentication, so bind it to a loopback or
private address that only your Prometheus can reach, never the public interface.
In a container, that means the container network's address rather than
`127.0.0.1`, and not publishing the port. A port that is taken is logged and
skipped, and the server starts anyway.

With several replicas, sum the counters and histograms across instances. The
state gauges (`spov_jobs`, `spov_sessions`, `spov_job_oldest_queued_age_seconds`)
come only from the replica holding the reconciler lock, so don't sum them.
[`grafana/spov-dashboard.json`](grafana/spov-dashboard.json) is an importable
dashboard written that way.

## Shutdown

On SIGTERM the server stops accepting, drains in-flight requests within
20 s, then stops the reconciler and the job queue and closes the Modal client
and the database pool. In-flight launches are **not** awaited — they keep
running in the sandbox, and another replica finishes them (DL-003).

Set the orchestrator's grace period above 20 s: Kubernetes'
`terminationGracePeriodSeconds: 30` and Kamal's `drain_timeout: 25`
both do.

## The published image

Each release publishes `ghcr.io/jimutt/spov:<version>` for `linux/amd64`
and `linux/arm64`, signed by its digest and with a build provenance
attestation (DL-082). Deploy it by digest, so a moved tag can never change
what runs: the release workflow prints `ghcr.io/jimutt/spov:<version>@sha256:<digest>`
as a notice, and `docker buildx imagetools inspect ghcr.io/jimutt/spov:<version>`
shows it too. Check it before deploying, with cosign v3 (cosign v2.6 or
newer needs `--new-bundle-format`):

```sh
cosign verify ghcr.io/jimutt/spov@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/jimutt/spov/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/jimutt/spov@sha256:<digest> --repo jimutt/spov
```

The Kubernetes manifests name the published image by digest. Compose and
Kamal build from `deploy/Dockerfile` instead, which is how they prove a
change before it is released; to run a release with Compose, replace its
`build:` with `image: ghcr.io/jimutt/spov:<version>@sha256:<digest>`.

`:latest` follows the newest release that is not a prerelease. Nothing here
uses it.

## The image has no shell

It is distroless, so there is no shell, no `curl` and no `wget`. A
container-level healthcheck cannot reach `/readyz`; probe it from
outside, which is what the Kubernetes probes and Kamal's proxy do.

`HOME` is set in the image and must stay set: the Modal SDK resolves its
config path through `os.UserHomeDir` *before* reading the credentials it
was handed, and fails with `$HOME is not defined` otherwise. The path
need not exist.

## Smoke test

`./deploy/smoke.sh` checks what only a real container and database can
prove: that the image builds, migrations reach head, health answers,
authentication and the browser guard hold, and — with `--repo` — that a
session survives a restart, which is what distinguishes a Postgres store
from an in-memory one.

```sh
export SPOV_SERVER_TOKEN=$(openssl rand -hex 32)
export MODAL_TOKEN_ID=... MODAL_TOKEN_SECRET=...
./deploy/smoke.sh                        # health, auth, guard
./deploy/smoke.sh --repo owner/name      # also the restart check
./deploy/smoke.sh --url https://spov.example.com   # something already running
```

`--repo` creates a **billable** sandbox and terminates it on the way
out, including when the script fails partway.

## Compose

```sh
export SPOV_SERVER_TOKEN=$(openssl rand -hex 32)
export MODAL_TOKEN_ID=... MODAL_TOKEN_SECRET=...
docker compose -f deploy/compose.yaml up --build
```

To try browser login as well, register an OAuth app with the callback
`http://localhost:8080/oauth/github/callback` and add yourself to an
allowlist:

```sh
cp deploy/allowlist.example deploy/allowlist   # gitignored; add your address
export SPOV_ALLOWLIST_SOURCE=./allowlist
export SPOV_GITHUB_OAUTH_CLIENT_ID=... SPOV_GITHUB_OAUTH_CLIENT_SECRET=...
docker compose -f deploy/compose.yaml up --build
```

`SPOV_ALLOWLIST_SOURCE` defaults to `./allowlist.example`, whose only
entry is a placeholder nobody owns — the bind source has to exist, or
Docker creates a directory there and the server fails on a confusing
read error. It is imported into the database on the first start; after
that, edit the allowlist through `/api/v1/allowlist`, not the file. To
start over, drop the volume.

A completed login lands on `/`, the dashboard the image embeds. To see who the
server thinks you are, visit `/api/v1/me`, which returns your identity on the
session cookie alone.

Over plain http the session cookie is not marked `Secure` (it would
never be sent back), and the server says so at startup.

## Kamal

`deploy/kamal/` holds `config/deploy.yml` and a `pre-deploy` hook that
runs `spov serve migrate up` against the new version before any
container serves. Run Kamal from the repository root, naming the config:

```sh
kamal setup  -c deploy/kamal/config/deploy.yml   # the first time
kamal deploy -c deploy/kamal/config/deploy.yml
```

The config points Kamal at the hook and the secrets under
`deploy/kamal/.kamal/` (`hooks_path`, `secrets_path`), and builds from the
root with `deploy/Dockerfile`. Fill in the registry, server and host, and
put secrets in `deploy/kamal/.kamal/secrets`, which is gitignored and kept
out of the image's build context.

Place the server's user file on the host before the first deploy, next to
the allowlist seed — Kamal does not sync relative volume sources, and the
remote docker resolves them against the SSH session's working directory
instead of the repository:

```sh
scp deploy/server-config.yaml <user>@<host>:/etc/spov/server-config.yaml
```

To run a published release instead of building, pull it and name its tag —
nothing else in the config changes, and the builder section is unused:

```sh
kamal deploy -c deploy/kamal/config/deploy.yml --skip-push --version 0.2.1
```

Images carry the `service=spov` label Kamal inspects from v0.2.1; older
tags are refused with Kamal's own "missing the 'service' label" error.
`:latest` is available but a version tag is reproducible, so prefer it.

@decision(DL-105)

## Kubernetes

`deploy/k8s/` is plain YAML: a Deployment with the three probes, a
Service, a migrate Job to run before rolling the Deployment, a ConfigMap
for the allowlist seed, a ConfigMap for the server's user file, and a
Secret template. Pin the image by digest. Helm is not adopted until
someone needs it.
