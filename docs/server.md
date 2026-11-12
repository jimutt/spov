# The server and the dashboard

`spov serve` is spov's control plane: it holds the credentials, creates the
sandboxes and supervises the jobs. The `spov` CLI and the web dashboard are both
clients of it. You rarely start it yourself.

## Local or with a database

| | **Local** | **With a server and a database** |
|---|---|---|
| Setup | None: the CLI starts its own server | `spov serve` with Postgres; see [`deploy/README.md`](../deploy/README.md) |
| Interactive sessions (`spov fly`) | Yes | Yes |
| Autonomous jobs (`spov jobs create`) | No | Yes |
| Web dashboard | No | Yes, with GitHub sign-in |
| Credentials | On your machine (keyring or a 0600 file) | Per user, encrypted on the server |
| Users | You | Several, with an allowlist and quotas |

The server with a database can run on your own machine too. It is what the
dashboard and jobs need, not a remote host.

## The local server

By default there is nothing to set up. The CLI starts a server for itself the
first time it needs one (`spov serve --local`, on an ephemeral loopback port),
finds it again through `~/.local/state/spov/server.json`, and the server exits
after a minute with nothing to do (end the `spov serve --local` process to
restart it sooner, say after storing a model key). Its log is `~/.local/state/spov/server.log`,
which is where to look if a launch fails early. After an upgrade the old
server is stopped and replaced, so two builds never share one.

The local server keeps its sessions' history on disk, so a stopped session can
be resumed after a restart ([local-history.md](local-history.md)).

## A hosted server

A server is used instead of the local one when it is named: the `--server`
flag, then `SPOV_SERVER`, then `server.url` in config. Sign in with
`spov login`: spov prints a code to enter in a browser and stores the token
that comes back. `spov whoami` shows who the server sees, and `spov logout`
revokes the token.

Running a server, with its database, browser login, allowlist, quotas,
credentials, health checks and the published image, is covered in
[`deploy/README.md`](../deploy/README.md). That page also covers the limits on
image builds and the 60-second cache of the repository listing.

> **Before you share a server with others,** read
> [Running a shared server safely](security.md). Model keys in the server's
> environment are given to every user's sessions
> ([Shared model keys](security.md#shared-model-keys)), and spend is limited per
> user as a rate, with no total cap ([Cost](security.md#cost)).

## The web dashboard

A server with a database also serves a dashboard, signed in to with GitHub.
[`deploy/README.md`](../deploy/README.md) covers the OAuth app and the
allowlist.

- **Sessions**: every session, live, with its state, its job if it has one,
  and its age.
- **Jobs**: every job, filtered by All, Active, Needs you (blocked or steered)
  and Finished, with each job's outcome.
- **Start a session** and **Start a job**: from a form each, with a picker of
  your repositories. The job form makes the same job as `spov jobs create`.
- **A session's page** is also its job's page:
  - the live transcript;
  - the job's state, time limit, verdict gate by gate, and its pull request or
    report;
  - a message box to steer it, a model switch, and answers to the agent's
    permission questions;
  - **Interrupt** to stop the current turn, **Resume** to hand a paused job
    back or reopen a stopped session, **Cancel job** and **End session**. The
    last two act only on a second press;
  - what credentials and environment variables the sandbox was given.
- **Usage**: sessions, sandbox hours and estimated sandbox cost this month.
  Model token cost is marked as not yet tracked.
- **Tokens**: your CLI tokens, which you can revoke.
- **Settings**: your stored credentials and OpenCode connection, default
  model, git access, job time limit and git identity. An admin also stores the
  organisation's GitHub App here (see [credentials.md](credentials.md#github)),
  and manages who may sign in under **People** ([below](#who-may-sign-in)).

## Who may sign in

A hosted server admits only the people on its allowlist. An administrator
manages it on the Settings page, under **People**: add a person by their GitHub
username, or by an address, and tick "administrator" for someone who should
manage the list too.

- **Use the GitHub username.** The server looks it up on GitHub when you add it
  and shows the name GitHub has for that account, so you can see at once whether
  you typed the right one. The entry is the account itself, not its name: it
  keeps working after the person renames their account, and it does not admit
  whoever takes a released name.
- **An address works too**, but it must be the address the person's GitHub
  account has as its primary, verified address, which is often a personal one.
  When someone is refused, the page they see names both their account and their
  address, so they can tell you either.
- The list shows "never signed in" beside an entry nobody has used yet.
- **Removing a person** ends their browser sessions, cancels their jobs, stops
  their sessions, revokes their CLI tokens and deletes their stored credentials.
  Their history and templates are kept. If another entry still covers them (an
  address and a username for the same person), they keep their access and
  nothing of theirs is ended.

The same list is the `/api/v1/allowlist` API, for scripts, and a new deployment
is seeded from a file. [`deploy/README.md`](../deploy/README.md#the-allowlist)
covers both.

The browser never talks to a sandbox directly. The page shows what the server
has recorded, and an open tab does not keep a sandbox alive.

## Limits

The server limits how often and how much one client may ask of it, so that one
user or one anonymous address cannot take it down. The limits are fixed; only
`server.trusted_proxies` is a setting. A refused request is a 429 with a
`Retry-After` header (in seconds) and a message saying which limit refused it,
which the CLI prints and the dashboard shows.

| Limit | Value | Refused with |
|---|---|---|
| Sign-ins started from one address (`GET /login` and `POST /device/code` together) | 30 a minute | 429 |
| Sign-ins started from one network (an IPv4 /24 or an IPv6 /48) | 300 a minute | 429 |
| CLI polls for a sign-in token from one address (`POST /device/token`) | 120 a minute | 429 `slow_down`, which makes the CLI poll less often |
| CLI polls for a sign-in token from one network | 1,200 a minute | 429 `slow_down` |
| Browser sign-ins in progress on one replica | 10,000 started within 15 minutes | 503 with `Retry-After` |
| CLI sign-ins (device codes) in progress on one replica | 10,000 started within 10 minutes | 503 with `Retry-After` |
| Of each of those two, the places one network holds | 500 | 429 with the wait until that network's oldest start ends |
| API requests (`/api/v1/*`) per user | 600 a minute, after a burst of 100 | 429 |
| Session creates, job creates and session resumes per user | 20 a minute | 429 |
| Open live views (event streams) per user | 20 | 429 on the next |
| Requests in progress per user, streams not counted | 32 | 429 |
| Request body | 1 MiB; 24 MiB on session create, job create and resume, which carry a sync bundle | 413 |
| Time to send a request body | 30 s; 5 minutes on those three routes | the request fails (408 on the API) |
| Time to send the headers | 10 s | the connection is closed |
| An idle keep-alive connection | closed after 2 minutes | |

An IPv6 address counts as its /64 for the per-address limits. The sign-in
limits count a request only once the browser guard has accepted it, so a
request it refuses (a cross-site page sending visitors to `/login`, say) uses
up nothing. The per-network share keeps one allocation from filling a cap and
leaving everyone else at 503, but sources spread over 20 or more networks can
still fill a cap until their starts leave its window; the cap then answers 503
for everyone.

A user is the account a credential belongs to; the shared `SPOV_SERVER_TOKEN`
is one user. `/livez`, `/readyz`, `/startupz`, `/opencode-pin.json`, the web
app's own files and the Prometheus listener are not limited. A live view
reopens every five minutes and has no time limit across reopens.

`spov jobs wait` and the wait in `spov attach` sleep for a 429's `Retry-After`
(at least their poll interval, at most a minute) and read again, so a wait over
the request limit slows down rather than failing. The dashboard opens a live
view refused with a 429 once more after its `Retry-After` (between one second
and a minute) and shows the refusal only if that is refused too.

Every limit is kept in memory by each replica, so N replicas allow N times as
much, and a restart starts each count again. The two sign-in caps bound how
many sign-in rows one replica can create while the rows live.

**Behind a reverse proxy every client shares the proxy's address** for the
per-address limits, until `server.trusted_proxies` names the proxy
([configuration](configuration.md#behind-a-reverse-proxy-servertrusted_proxies)).
Until then the server logs one warning the first time a request arrives with an
`X-Forwarded-For` header it ignores. Refusals are counted in
`spov_requests_limited_total` ([metrics](metrics.md)).
