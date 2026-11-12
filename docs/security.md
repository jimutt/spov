# Running a shared server safely

This page is for someone deciding whether, and how, to run one spov server for
several people. On your own machine, with your own credentials, most of it does
not apply: the local server listens on loopback only and holds only your keys.

The short version: a shared server is safe to put in front of people you would
trust with the deployment's Modal account and model keys, and not yet in front of
strangers. Spend is limited as a rate, not a total, and every model key in the
server's environment is every user's.

## What spov protects and what it does not

spov is designed so that **the agent is untrusted**. It reads repositories and
issues that may contain hostile instructions, and it has a shell. The rest of the
design follows from that:

- **The agent works in a Modal sandbox.** It has no access to the control plane's
  host, database or other users' sandboxes.
- **Credentials sit in a proxy sidecar, and the agent holds placeholders.** Forge
  tokens and model provider keys are held by a second container in the sandbox. It
  adds each one only to requests its rules allow, and scrubs it from responses.
  The server refuses to start without the proxy unless `SPOV_TOKENS_IN_SANDBOX=1`
  (see [the sandbox](sandbox.md#credentials-stay-outside-the-sandbox)).
- **A user's sessions and jobs are theirs alone.** Every session and job route
  answers 404 for another user's, unless the caller is an admin. Stored credentials are sealed at rest with
  `SPOV_CREDENTIAL_KEY`, and no endpoint returns one.
- **Signed-in users are trusted more than the agent, and less than the
  operator.** They are on an allowlist, but they share one organisation. An admin
  can see and control every user's sessions and jobs. Only the operator holds the
  server's environment, database and Modal account.

Two properties have not yet been tested against a live deployment. That the agent
cannot read the sidecar's memory, configuration or CA key from the main container
is what the design assumes and a live test on a throwaway Modal workspace has not
yet confirmed. What an exit snapshot contains has not been checked either. Treat
both as intended behaviour, not as measured.

### What is a guardrail and not a boundary

A guardrail makes the wrong thing harder. A boundary stops it. These are
guardrails:

- **A job's permission allowlist.** The plugin inside a job's sandbox denies
  tools the job does not need and steers the agent away from credential files. The
  agent's shell and the sandbox's own API are outside it.
- **Network egress.** `sandbox.egress` defaults to `open`: a sandbox reaches the
  whole internet from your Modal account, jobs included. The server's setting is
  only the default for a request that names no preset. A user's request can name
  another, so an operator cannot use it to limit what users' sandboxes reach.
- **Sandbox sizes, regions and clouds.** The quotas below bound CPU, memory and
  time. The regions and cloud a launch asks for are the caller's choice.

What does limit a prompt-injected agent is the credential's own scope and the
proxy's rules. A GitHub App token is narrowed to the session's repositories, and a
stored token only reaches the session's repositories' endpoints, unless the user
chose git access `all` (see [GitHub access](github-access.md)). Protect default
branches on the forge. See [the sandbox](sandbox.md#credentials-stay-outside-the-sandbox).

## Who can sign in

Only people on the allowlist can use a hosted server. Browser sign-in is GitHub
OAuth; the identity is the account's **primary, verified** e-mail address, which
must be on the allowlist. `spov login` is the same identity through spov's own
device flow. An empty allowlist admits nobody.

- **The allowlist lives in the database** and is managed by admins through
  `/api/v1/allowlist`. `SPOV_ALLOWLIST_FILE` only seeds a new deployment.
- **An admin is an allowlist entry with `admin` set.** Admin comes from the
  allowlist alone and is re-checked against the database on every write. The
  server refuses to leave the organisation with no admin, or to let an admin
  remove their own entry. Keep at least two, so that one person's lost GitHub
  account does not leave you editing the database. If an organisation ends up with
  none, restoring the seed file and restarting imports it again.
- **`SPOV_SERVER_TOKEN` is not an admin of the allowlist's organisation.** It is a
  standing credential for a separate user and organisation, `local`. Its holder can
  launch sessions and jobs there, on the deployment's own credentials and keys. It
  is not rate limited, is never revoked except by a restart, and has no strength
  check, so make it long and random.

### What removing someone does today

Removing an address from the allowlist:

- stops that person signing in, and refuses their browser sessions and CLI tokens
  on their next request, on every replica, after at most 10 seconds;
- deletes their browser sessions, so that adding the address back does not revive
  an old login;
- leaves an already open event stream running for up to five minutes.

It does **not**:

- end their running sessions, jobs or sandboxes. They run until they finish, are
  stopped, or hit their timeout, which can be hours;
- revoke their CLI tokens. They are refused while the address is absent and work
  again if it is added back;
- stop the renewal of the git tokens of their sessions that are still running.

To off-board someone, remove them and then end their sessions yourself. An admin
sees every user's sessions, in the dashboard and in `spov ls`, and can stop them.

<!-- security-round: off-boarding. When the stream that ends a removed person's
sandboxes and revokes their tokens lands, replace the "does not" list above with
what it now does. -->

## Shared model keys

**Every model provider key in the server's environment is given to every signed-in
user's sessions.** That includes `ANTHROPIC_API_KEY`, which the example Compose,
Kamal and Kubernetes files all pass through.

The mechanism: at launch spov hands the sandbox's proxy sidecar the user's own
stored keys layered over the deployment's, per provider. A user with no key of
their own for a provider gets the deployment's. The proxy then sets the key on **any request** to that
provider's API host, on any path and with any method, because only OpenCode's own
entry is restricted to paths. The agent holds only a placeholder, so it cannot
read the key, but it can use it: it can run inference, and call whatever else that
key can reach on the provider, such as stored files and batches, or an
aggregator's account endpoints. Whatever the key's budget is, every user spends
it.

The goal preparation pass makes this easy to do by accident. It is the server's
own model call, once per job ([jobs](jobs.md)). With no `prep` setting it looks for
the first of `OPENROUTER_API_KEY`, `GEMINI_API_KEY` and `OPENAI_API_KEY` in the
server's environment, or its own credential store, and uses it. Those are the same
variables the sandboxes receive. Setting one so that jobs get a prepared goal also
gives it to all users. `ANTHROPIC_API_KEY` is not one of the three, but is shared
in the same way.

To keep a key to yourself:

- **Leave the provider variables unset.** Users then store their own with
  `spov credentials add ANTHROPIC_API_KEY` or in Settings, which reach only that
  user's sessions. Users can also sign in to OpenCode Go or Zen, which is a grant of their own.
- **Give the prep pass a variable of its own.** Set `prep.key_env` (or
  `SPOV_PREP_KEY_ENV`) to a name that is not a provider variable, for example
  `SPOV_PREP_API_KEY`, and set `prep.model` (and `prep.base_url` for a gateway or
  a provider other than OpenAI). A name that is not in the provider catalogue is
  read by the prep pass and never forwarded to a sandbox. Put a spending limit on
  that key at the provider.
- **Without a provider key, the prep pass is skipped.** Each job then opens on its
  original goal alone, and `spov serve` says so at startup. Nothing else breaks.

If you choose to share a key on purpose, say so to your users, give it a
spending limit at the provider, and use a key made for the deployment.

## Cost

**Spend is bounded as a rate, not as a total.** Limits apply per user and per
create. There is no monthly cap, no per-user budget and no limit across the
deployment. `GET /api/v1/me/usage` estimates month-to-date hours and cost. It does
not enforce anything.

The defaults, per user (zero or unset means the default, never unlimited):

| Limit | Default | Setting |
|---|---|---|
| Concurrent sessions, excluding jobs' own | 3 | `quotas.max_concurrent_sessions` |
| Unfinished jobs, queued included | 2 | `quotas.max_concurrent_jobs` |
| A session's lifetime | 8 h, never above 12 h | `quotas.max_timeout` |
| A job's wall clock | 4 h | `quotas.max_job_duration` |
| CPU, request and burst | 4 | `quotas.max_cpu` |
| Memory, request and burst | 16384 MiB | `quotas.max_memory_mib` |

Each also has an environment form, such as `SPOV_QUOTAS_MAX_CONCURRENT_JOBS`. A
request over a size or time limit is refused, and over a concurrency limit it is
answered with 429. An admin can set a different limit for one user with `PATCH
/api/v1/users/{id}`; see [deploy/README.md](../deploy/README.md#quotas).

**The worst case, per user, per day, at the defaults.** Three sessions and two
jobs make five sandboxes at once. A user who relaunches each as it ends keeps all
five running around the clock: 5 × 24 = **120 sandbox-hours a day**. If each asks
for the most it may, that is 4 CPUs and 16 GiB, which is 20 CPUs and 80 GiB
continuously, or 480 core-hours a day. Multiply by the number of people on the
allowlist. A sandbox idles out after 30 minutes by default, but a script that
keeps talking to it does not idle, and a job never idles out, so do not count on
that. Modal bills a sandbox for its wall-clock life, at the larger of what it requested
and what it used. Read the current rates at Modal; spov does not state a price.

Model tokens are paid through the keys the sandbox runs with, which spov does not
meter. See [Shared model keys](#shared-model-keys).

What you can do:

- **Lower the quotas before you add people.** For example, one session, one job,
  2 CPUs, 4 GiB and a 2 h timeout (`quotas.*` in the server's user file), then
  raise them for particular users with the per-user override.
- **Watch the metrics** ([metrics.md](metrics.md)). `spov_sessions` and `spov_jobs`
  show what is running now, `spov_session_lifetime_seconds` is the basis for
  billable time, and `spov_quota_refusals_total` shows who is hitting limits.
  Alert on live sandboxes beyond what your users should need.
- **Set a spending limit and alerts in Modal**, in a workspace and environment
  (`MODAL_ENVIRONMENT`) used only by this deployment. spov has no budget of its
  own, so this is the only hard stop.
- **Set `modal.build_on_demand: false`.** By default any user can start an image
  build by asking for an add-on or an apt package, and builds are not limited per
  user. With it off, users get the variants you build with `spov serve image build`.

<!-- security-round: limits. When the stream that adds request rate limits, stream
caps and per-user build limits lands, add a sentence here and in the checklist
saying what is limited and how to set it. -->

## Before you expose a server: a checklist

Do each of these before a server is reachable by people you do not know.

1. **Put TLS in front of it.** `spov serve` speaks plain HTTP and ends no TLS.
   Run it behind a reverse proxy that terminates TLS, adds HSTS and limits request
   rates and connections, and never publish port 8080 directly. The server trusts
   no forwarded header; it identifies itself by `SPOV_PUBLIC_URL`, so the proxy
   must pass the original `Host`. The server sets only a header-read timeout, so
   have the proxy set the others.
2. **Set `SPOV_PUBLIC_URL` to the `https://` origin.** Login needs it, and the
   session cookie is marked `Secure` only when it is https. The server warns when
   it is not.
3. **Generate `SPOV_SERVER_TOKEN` yourself:** `openssl rand -hex 32`. Keep it in
   the same secret store as `SPOV_CREDENTIAL_KEY`. Do not give it to users; they
   should use `spov login`.
4. **Set `SPOV_CREDENTIAL_KEY`** (`openssl rand -base64 32`), and back it up
   separately from the database. Without it the server stores no user credentials,
   and if you lose it every stored credential has to be entered again. Use a
   binary with the credential proxy built in, which the release and the Dockerfile
   give you, and never set `SPOV_TOKENS_IN_SANDBOX`.
5. **Protect the database and its backups.** Postgres holds sealed credentials
   and, in clear, every session's transcript and events and every job's reports.
   It also holds browser session tokens as issued, so a person who can read the
   `web_sessions` table can act as a signed-in user, an admin included, until
   those expire (after at most seven days). Give the database its own credentials
   and TLS, restrict who can read it and its backups, and size its disk. Hosted
   mode keeps sessions, events, messages and job files until you delete them by
   hand, and has no retention setting yet. (`history.retention` applies to a local
   server only.)
6. **Choose the allowlist deliberately, with at least two admins.** Each person on
   it can run the quotas above around the clock, and uses the organisation's
   credentials. Under the organisation's GitHub App that means any repository the
   App is installed on ([GitHub access](github-access.md)). Leave the organisation's
   credentials unset for people you do not know.
7. **Decide on `git.allow_all`.** It is off by default. On, a user may give a
   session git access to every repository the organisation's credential reaches,
   not only the session's. Leave it off unless you want that.
8. **Decide on shared model keys** ([above](#shared-model-keys)) and **set the
   quotas** ([above](#cost)).
9. **Set `modal.build_on_demand: false`**, and run `spov serve image build` for the
   add-ons you offer.
10. **Decide on snapshots.** With the proxy and a persistent store, a sandbox's
    filesystem is saved when it ends, so that a session can be resumed: 7 days and
    20 per user by default, in your Modal workspace. It holds the checked-out
    repositories and whatever the agent wrote. Set `snapshots.enabled: false` to
    take none.
11. **Keep the metrics listener private.** `metrics.prometheus_listen` is off by
    default. It serves with no authentication, so bind it to a loopback or private
    address and do not publish the port. Check from outside that it is not reachable.
12. **Use a dedicated Modal environment** (`MODAL_ENVIRONMENT`) and a token made for
    the deployment. spov's reaper ends any sandbox it created in that environment
    past its deadline.
13. **Migrate in a separate step.** Use `spov serve migrate up` in your deploy,
    not `SPOV_MIGRATE_ON_START`.
14. **Run a signed release, by digest, and keep it updated.** Releases are signed
    with Sigstore and carry a build provenance attestation. Verify the image and
    archives as [deploy/README.md](../deploy/README.md#the-published-image) and
    [releasing.md](releasing.md#checking-a-release) describe, and deploy the image
    by its `sha256` digest rather than a tag. A server is as patched as its
    release's build day, so watch for new releases and update.

<!-- security-round: trusted proxies. When `server.trusted_proxies` lands, say in
item 1 what it does and when to set it. -->

## Reporting a vulnerability

Please do not open a public issue. See [`SECURITY.md`](../SECURITY.md): report it
privately through GitHub's "Report a vulnerability" on the repository.
